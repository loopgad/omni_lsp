;;; runner.el --- isolated OmniLSP Eglot acceptance driver -*- lexical-binding: t; -*-

(require 'cl-lib)
(require 'completion)
(require 'eglot)
(require 'flymake)
(require 'json)
(require 'project)
(require 'subr-x)
(require 'url-util)
(require 'xref)

(defconst omnilsp-language-modes
  '(("go" . omnilsp-go-mode)
    ("c" . omnilsp-c-mode)
    ("cpp" . omnilsp-cpp-mode)
    ("rust" . omnilsp-rust-mode)
    ("python" . omnilsp-python-mode)
    ("typescript" . omnilsp-typescript-mode)
    ("typescriptreact" . omnilsp-tsx-mode)
    ("javascript" . omnilsp-javascript-mode)
    ("javascriptreact" . omnilsp-jsx-mode)))
(defvar omnilsp-result
  '(("client" . "emacs-eglot") ("status" . "running")
    ("cleanExit" . :json-false) ("clientTestsCompleted" . :json-false)
    ("serverExitConfirmed" . :json-false) ("cases" . nil)))
(defvar omnilsp-result-path (getenv "OMNILSP_CLIENT_RESULT"))
(defvar omnilsp-workspace nil)
(defvar omnilsp-tool-status nil)
(defvar omnilsp-servers nil)
(defvar omnilsp-shutdown-ok t)
(defvar omnilsp-shutdown-count 0)
(defvar omnilsp-shutdown-request-observation nil)
(defvar omnilsp-exit-notification-sent nil)
(defvar omnilsp-jsonrpc-cleanup-observation nil)
(defvar omnilsp-publish-diagnostics-events nil)
(defvar omnilsp-didopen-metadata nil)

(cl-defmethod eglot-handle-notification :around
  ((server eglot-lsp-server) method &rest params)
  (when (eq method 'textDocument/publishDiagnostics)
    (push (list :uri (plist-get params :uri)
                :diagnostics (plist-get params :diagnostics))
          omnilsp-publish-diagnostics-events))
  (cl-call-next-method))

(defun omnilsp-jsonrpc-shutdown-with-grace (original connection &optional cleanup)
  (let* ((process (jsonrpc--process connection))
         (started (float-time))
         (deadline (+ (float-time) 2.0)))
    ;; Batch Emacs can run the JSON-RPC sentinel later than its short default
    ;; wait. Give the server a bounded chance to exit after Eglot sends `exit'.
    (while (and (process-live-p process)
                (not (process-get process 'jsonrpc-sentinel-cleanup-started))
                (< (float-time) deadline))
      (accept-process-output process 0.05))
    (let ((sentinel-cleanup-started
           (process-get process 'jsonrpc-sentinel-cleanup-started)))
      (prog1 (funcall original connection cleanup)
        (setq omnilsp-jsonrpc-cleanup-observation
              `(("serverPid" . ,(process-id process))
                ("graceWaitMs" . ,(round (* 1000 (- (float-time) started))))
                ("sentinelCleanupStartedBeforeJsonrpcCleanup"
                 . ,(if sentinel-cleanup-started t :json-false))
                ("processStatusAfterJsonrpcCleanup" . ,(format "%s" (process-status process)))
                ("processExitCodeAfterJsonrpcCleanup" . ,(process-exit-status process))))))))

(advice-add 'jsonrpc-shutdown :around #'omnilsp-jsonrpc-shutdown-with-grace)

(defun omnilsp-capture-didopen (original connection method &rest params)
  (when (eq method :textDocument/didOpen)
    (let* ((document (plist-get (car params) :textDocument))
           (text (plist-get document :text)))
      (setq omnilsp-didopen-metadata
            (list :server (jsonrpc-name connection)
                  :uri (plist-get document :uri)
                  :language-id (plist-get document :languageId)
                  :version (plist-get document :version)
            :text-length (length (or text ""))
            :text-sha256 (secure-hash 'sha256 (or text ""))))))
  (when (eq method :exit)
    (setq omnilsp-exit-notification-sent t))
  (apply original connection method params))

(advice-add 'jsonrpc-notify :around #'omnilsp-capture-didopen)

(defun omnilsp-capture-shutdown-request (original connection method params &rest args)
  (if (eq method :shutdown)
      (let ((started (float-time)))
        (condition-case err
            (prog1 (apply original connection method params args)
              (setq omnilsp-shutdown-request-observation
                    `(("responseReceived" . t)
                      ("elapsedMs" . ,(round (* 1000 (- (float-time) started)))))))
          (error
           (setq omnilsp-shutdown-request-observation
                 `(("responseReceived" . ,:json-false)
                   ("elapsedMs" . ,(round (* 1000 (- (float-time) started))))
                   ("error" . ,(error-message-string err))))
           (signal (car err) (cdr err)))))
    (apply original connection method params args)))

(advice-add 'jsonrpc-request :around #'omnilsp-capture-shutdown-request)

(defun omnilsp-get (object key)
  (let ((keyword (intern (concat ":" key))))
    (cond ((hash-table-p object) (or (gethash key object) (gethash keyword object)))
          ((and (listp object) (consp (car-safe object)))
           (or (cdr (assoc-string key object)) (plist-get object keyword)))
          ((listp object) (plist-get object keyword)))))

(defun omnilsp-put (object key value)
  (let ((cell (assoc-string key object)))
    (if cell (setcdr cell value) (push (cons key value) object))
    object))

(defun omnilsp-write-result ()
  (when omnilsp-result-path
    (make-directory (file-name-directory omnilsp-result-path) t)
    (with-temp-file omnilsp-result-path
      (insert (json-encode omnilsp-result) "\n"))))

(defun omnilsp-case-row (case status)
  `(("name" . ,(omnilsp-get case "name"))
    ("languageId" . ,(omnilsp-get case "languageId"))
    ("family" . ,(omnilsp-get case "family"))
    ("status" . ,status)))

(defun omnilsp-set-row (row key value)
  (setcdr row (omnilsp-put (cdr row) key value)))

(defun omnilsp-try-project (directory)
  (when (and omnilsp-workspace
             (file-in-directory-p (file-truename directory)
                                  (file-name-as-directory (file-truename omnilsp-workspace))))
    (cons 'omnilsp-acceptance (file-name-as-directory (file-truename omnilsp-workspace)))))

(cl-defmethod project-root ((project (head omnilsp-acceptance))) (cdr project))
(add-hook 'project-find-functions #'omnilsp-try-project)

(defun omnilsp-define-modes ()
  (let ((server (getenv "OMNILSP_BIN")))
    (unless (and server (file-executable-p server))
      (error "OMNILSP_BIN must name the frozen executable"))
    (dolist (entry omnilsp-language-modes)
      (let ((language (car entry)) (mode (cdr entry)))
        (eval `(define-derived-mode ,mode prog-mode ,(capitalize language)))
        (add-to-list 'eglot-server-programs
                     (cons (list mode :language-id language) (list server "serve")))))))

(defun omnilsp-normalize-list (value)
  (cond ((vectorp value) (append value nil)) ((listp value) value) (t nil)))

(defun omnilsp-read-json-file (path)
  (with-temp-buffer
    (insert-file-contents path)
    (json-parse-buffer :object-type 'alist :array-type 'list
                       :null-object nil :false-object :json-false)))

(defun omnilsp-missing-tools (case)
  (cl-remove-if (lambda (tool) (eq (omnilsp-get omnilsp-tool-status tool) t))
                (omnilsp-normalize-list (omnilsp-get case "requiredTools"))))

(defun omnilsp-wait (predicate description &optional seconds)
  (let ((deadline (+ (float-time) (or seconds 30))))
    (while (and (not (funcall predicate)) (< (float-time) deadline))
      ;; Batch mode has no user events; `sit-for' lets normal timers and
      ;; process notifications run while the driver waits.
      (sit-for 0.05))
    (unless (funcall predicate) (error "Timed out waiting for %s" description))))

(defun omnilsp-snapshot-epoch (server)
  (or (omnilsp-get (jsonrpc-request server :omnilsp/status nil) "SnapshotEpochs") 0))

(defun omnilsp-wait-for-snapshot-epoch (server before timeout description)
  (let ((deadline (+ (float-time) timeout)) (epoch before))
    (while (and (<= epoch before) (< (float-time) deadline))
      (setq epoch (omnilsp-snapshot-epoch server))
      (when (<= epoch before) (sit-for 0.1)))
    (unless (> epoch before)
      (error "Timed out waiting for %s (SnapshotEpochs stayed at %s)"
             description before))
    epoch))

(defun omnilsp-flush-batch-document-change ()
  (when (eglot-managed-p)
    ;; Eglot debounces document changes with an idle timer. Batch Emacs never
    ;; becomes idle, so deliver the normal didChange hook explicitly.
    (accept-process-output nil 0.01)
    (when (timerp eglot--change-idle-timer)
      (cancel-timer eglot--change-idle-timer))
    (setq eglot--change-idle-timer nil)
    (run-hooks 'eglot--document-changed-hook)
    (sit-for 0.05)
    (when (timerp eglot--change-idle-timer)
      (cancel-timer eglot--change-idle-timer))
    (setq eglot--change-idle-timer nil)))

(defun omnilsp-find-occurrences (symbol)
  (save-excursion
    (goto-char (point-min))
    (let (positions)
      (while (search-forward symbol nil t) (push (- (point) (length symbol)) positions))
      (nreverse positions))))

(defun omnilsp-hover (position symbol &optional rust-cold-start)
  (goto-char position)
  (unless (memq #'eglot-hover-eldoc-function eldoc-documentation-functions)
    (error "Eglot did not register its hover provider with ElDoc"))
  (let* ((deadline (+ (float-time) (if rust-cold-start 60 30)))
         (done nil) (info nil) (failure nil) (attempt 0))
    (while (and (or (= attempt 0) rust-cold-start)
                (not (and (stringp info)
                          (string-match-p (regexp-quote symbol) info)))
                (< (float-time) deadline))
      (setq done nil info nil failure nil)
      (condition-case err
          (funcall #'eglot-hover-eldoc-function
                   (lambda (text &rest _metadata)
                     (setq info text done t)))
        (error (setq failure err done t)))
      (when failure
        (signal (car failure) (cdr failure)))
      (unless rust-cold-start
        (setq attempt 1))
      (let ((attempt-deadline
             (if rust-cold-start
                 (min deadline (+ (float-time) 2))
               deadline)))
        (while (and (not done) (< (float-time) attempt-deadline))
          (accept-process-output nil 0.05)))
      (unless (or (and (stringp info)
                       (string-match-p (regexp-quote symbol) info))
                  (not rust-cold-start))
        ;; Rust-analyzer can answer before its initial crate graph is ready.
        ;; Retry through Eglot's ElDoc integration only when it returned no
        ;; positive symbol result; transport/JSON-RPC errors remain failures.
        (cl-incf attempt)
        (when (< (float-time) deadline)
          (accept-process-output nil (min 0.5 (* 0.1 attempt))))))
    (unless (and (stringp info)
                 (string-match-p (regexp-quote symbol) info))
      (let* ((params (eglot--TextDocumentPositionParams))
             (document (plist-get params :textDocument))
             (position (plist-get params :position)))
        (error "Eglot ElDoc did not describe %s within the bounded %s probe: didOpen=%S hoverUri=%s position=%S capability=%S callback=%S"
               symbol (if rust-cold-start "Rust" "hover") omnilsp-didopen-metadata
               (plist-get document :uri) position (eglot-server-capable :hoverProvider) info)))
    info))

(defun omnilsp-xref-items (kind identifier)
  (let ((backend (xref-find-backend)))
    (unless backend (error "Eglot did not provide an Xref backend"))
    (pcase kind
      ('definitions (xref-backend-definitions backend identifier))
      ('references (xref-backend-references backend identifier)))))

(defun omnilsp-xref-loc (item)
  (let ((location (xref-item-location item)))
    (cons (xref-location-group location) (xref-location-line location))))

(defun omnilsp-same-file-p (left right)
  (and left right
       (condition-case nil
           (equal (file-truename left) (file-truename right))
         (error (equal (expand-file-name left) (expand-file-name right))))))

(defun omnilsp-publish-events-for-file (path)
  (cl-remove-if-not
   (lambda (event)
     (let ((uri-path (condition-case nil
                         (eglot-uri-to-path (plist-get event :uri))
                       (error nil))))
       (and uri-path (omnilsp-same-file-p uri-path path))))
   omnilsp-publish-diagnostics-events))

(defun omnilsp-completions-at (position symbol)
  (goto-char position)
  (let* ((capf (run-hook-with-args-until-success 'completion-at-point-functions))
         (start (nth 0 capf)) (end (nth 1 capf)) (table (nth 2 capf))
         (prefix (and start (buffer-substring-no-properties start (point))))
         (all (and table prefix
                   (completion-all-completions prefix table nil (length prefix))))
          (candidates (cl-remove-if-not #'stringp (omnilsp-normalize-list all)))
          (matching
           (cl-find-if
            (lambda (candidate)
              (let* ((item (get-text-property 0 'eglot--lsp-item candidate))
                     (text-edit (plist-get item :textEdit))
                     (insert-text (plist-get item :insertText))
                     (insert-format (plist-get item :insertTextFormat))
                     (effective-text
                      (or (and text-edit (plist-get text-edit :newText))
                          (and (not (eql insert-format 2)) insert-text)
                          (plist-get item :label)
                          (substring-no-properties candidate))))
                (equal effective-text symbol)))
            candidates))
          (candidate-labels (mapcar #'substring-no-properties candidates)))
    (unless (and capf end table matching)
      (error "Emacs completion-at-point did not offer %s; candidates=%S" symbol candidates))
    candidate-labels))

(defun omnilsp-rename-name (case)
  (pcase (omnilsp-get case "name")
    ((or "go" "c" "cpp") "UseTarget")
    ((or "rust" "python") "use_target")
    ((or "typescript" "javascript") "useTarget")
    ((or "typescriptreact" "javascriptreact") "useComponent")
    (_ (error "No rename collision fixture for %s" (omnilsp-get case "name")))))

(defun omnilsp-rename-refusal-p (case text)
  (let ((message (downcase text)) (family (omnilsp-get case "family")))
    (cond
     ((equal family "go")
      (and (string-match-p "rename refused: symbol is exported; importer packages are not loaded" message)
           (string-match-p "sem-safe-001" message)))
     ((equal family "cpp")
      (and (string-match-p "rename refused:" message)
           (string-match-p "collision analysis is not proven" message)
           (string-match-p "sem-safe-001" message)))
     ((equal family "typescript")
      (and (string-match-p "rename refused:" message)
           (string-match-p "typescript/javascript" message)
           (string-match-p "sem-safe-001" message)))
     ((equal family "python")
      (and (string-match-p "rename refused:" message)
           (string-match-p "upstream language service returned no edits" message)
           (string-match-p "sem-safe-001" message)))
     (t (and (string-match-p "upstream language service refused rename" message)
             (or (string-match-p "-32803" message)
                 (string-match-p "request failed" message)))))))

(defun omnilsp-diagnostic-probe (case)
  (let ((name "omnilspMissingSymbol"))
    (pcase (omnilsp-get case "languageId")
      ("go" (format "var _ = %s" name))
      ((or "c" "cpp") (format "int clientDiagnosticProbe = %s;" name))
      ("rust" (format "fn client_diagnostic_probe() { let _ = %s; }" name))
      ("python" (format "client_diagnostic_probe = %s" name))
      ((or "typescript" "typescriptreact")
       (format "export const clientDiagnosticProbe: number = %s;" name))
      ((or "javascript" "javascriptreact")
       (format "export const clientDiagnosticProbe = %s;" name))
      (_ (error "No diagnostic probe for %s" (omnilsp-get case "languageId"))))))

(defun omnilsp-find-flymake-diagnostic (needle position timeout)
  (let ((deadline (+ (float-time) timeout)) found)
    (while (and (not found) (< (float-time) deadline))
      (setq found
            (cl-find-if
             (lambda (diagnostic)
               (and (<= (flymake-diagnostic-beg diagnostic) position)
                    (<= position (flymake-diagnostic-end diagnostic))
                    (string-match-p (regexp-quote needle)
                                    (flymake-diagnostic-text diagnostic))))
             (flymake-diagnostics)))
      (unless found (sit-for 0.1)))
    found))

(defun omnilsp-short-text (value &optional limit)
  (let* ((text (format "%s" (or value "")))
         (maximum (or limit 512)))
    (if (> (length text) maximum)
        (concat (substring text 0 maximum) "…")
      text)))

(defun omnilsp-lsp-position-summary (position)
  (when position
    `(("line" . ,(omnilsp-get position "line"))
      ("character" . ,(omnilsp-get position "character")))))

(defun omnilsp-publish-event-summary (event)
  (let* ((diagnostics (omnilsp-normalize-list (plist-get event :diagnostics)))
         (summaries
          (mapcar
           (lambda (diagnostic)
             (let* ((range (omnilsp-get diagnostic "range"))
                    (start (omnilsp-get range "start"))
                    (end (omnilsp-get range "end")))
               `(("message" . ,(omnilsp-short-text
                                (omnilsp-get diagnostic "message")))
                 ("severity" . ,(omnilsp-get diagnostic "severity"))
                 ("code" . ,(omnilsp-short-text (omnilsp-get diagnostic "code") 80))
                 ("source" . ,(omnilsp-short-text (omnilsp-get diagnostic "source") 80))
                 ("start" . ,(omnilsp-lsp-position-summary start))
                 ("end" . ,(omnilsp-lsp-position-summary end)))))
           (cl-subseq diagnostics 0 (min 4 (length diagnostics))))))
    `(("uri" . ,(plist-get event :uri))
      ("diagnosticCount" . ,(length diagnostics))
      ("diagnostics" . ,summaries))))

(defun omnilsp-diagnostic-string-summary (values)
  (let ((values (omnilsp-normalize-list values)))
    `(("count" . ,(length values))
      ("items" . ,(mapcar (lambda (value) (omnilsp-short-text value))
                          (cl-subseq values 0 (min 4 (length values))))))))

(defun omnilsp-result-meta-summary (server uri)
  (condition-case err
      (let* ((entries (omnilsp-normalize-list
                       (jsonrpc-request server :omnilsp/resultMeta `(:uri ,uri))))
             (recent (cl-subseq entries (max 0 (- (length entries) 3)))))
        `(("count" . ,(length entries))
          ("recent" . ,(mapcar
                         (lambda (entry)
                           `(("method" . ,(omnilsp-get entry "method"))
                             ("status" . ,(omnilsp-get entry "status"))
                             ("completeness" . ,(omnilsp-get entry "completeness"))
                             ("internalDiagnostics" . ,(omnilsp-diagnostic-string-summary
                                                        (omnilsp-get entry "internalDiagnostics")))))
                         recent))))
    (error `(("error" . ,(omnilsp-short-text (error-message-string err)))))))

(defun omnilsp-explain-summary (server uri)
  (condition-case err
      (let* ((response (jsonrpc-request server :omnilsp/explain `(:uri ,uri)))
             (evidence (omnilsp-normalize-list (omnilsp-get response "evidence")))
             (recent (cl-subseq evidence (max 0 (- (length evidence) 3)))))
        `(("snapshot" . ,(omnilsp-get response "snapshot"))
          ("syncRejects" . ,(omnilsp-get response "syncRejects"))
          ("evidenceCount" . ,(length evidence))
          ("recent" . ,(mapcar
                         (lambda (entry)
                           `(("method" . ,(omnilsp-get entry "method"))
                             ("kind" . ,(omnilsp-get entry "kind"))
                             ("assurance" . ,(omnilsp-get entry "assurance"))
                             ("snapshotRev" . ,(omnilsp-get entry "snapshotRev"))
                             ("backend" . ,(omnilsp-get entry "backend"))
                             ("diagnostics" . ,(omnilsp-diagnostic-string-summary
                                                (omnilsp-get entry "diagnostics")))))
                         recent))))
    (error `(("error" . ,(omnilsp-short-text (error-message-string err)))))))

(defun omnilsp-flymake-summary ()
  (let* ((diagnostics (flymake-diagnostics))
         (summaries
          (mapcar
           (lambda (diagnostic)
             `(("begin" . ,(flymake-diagnostic-beg diagnostic))
               ("end" . ,(flymake-diagnostic-end diagnostic))
               ("type" . ,(format "%s" (flymake-diagnostic-type diagnostic)))
               ("message" . ,(omnilsp-short-text
                              (flymake-diagnostic-text diagnostic)))))
           (cl-subseq diagnostics 0 (min 4 (length diagnostics))))))
    `(("count" . ,(length diagnostics))
      ("diagnostics" . ,summaries))))

(defun omnilsp-diagnostic-telemetry (server path snapshot-before publish-count-before)
  (let* ((events (omnilsp-publish-events-for-file path))
         (open-uri (plist-get omnilsp-didopen-metadata :uri))
         (uri-path (and open-uri
                        (condition-case nil (eglot-uri-to-path open-uri) (error nil))))
         (uri (if (and uri-path (omnilsp-same-file-p path uri-path))
                  open-uri (eglot-path-to-uri path)))
         (epoch-after (condition-case err
                          (omnilsp-snapshot-epoch server)
                        (error (error-message-string err)))))
    `(("uri" . ,uri)
      ("snapshotEpochBefore" . ,snapshot-before)
      ("snapshotEpochAfter" . ,epoch-after)
      ("publishDiagnosticsEventsForUri" . ,(length events))
      ("publishDiagnosticsEventsSinceCheckpoint"
       . ,(max 0 (- (length events) publish-count-before)))
      ("publishDiagnostics" . ,(mapcar #'omnilsp-publish-event-summary
                                        (cl-subseq events 0 (min 3 (length events)))))
      ("resultMeta" . ,(omnilsp-result-meta-summary server uri))
      ("explain" . ,(omnilsp-explain-summary server uri))
      ("flymake" . ,(omnilsp-flymake-summary)))))

(defun omnilsp-diagnostic-failure-context
    (server path needle position snapshot-before publish-count-before)
  (append `(("needle" . ,needle) ("probePosition" . ,position))
          (omnilsp-diagnostic-telemetry server path snapshot-before
                                        publish-count-before)))

(defun omnilsp-run-case-body (case row)
  (let* ((path (expand-file-name (omnilsp-get case "file") omnilsp-workspace))
         (language (omnilsp-get case "languageId"))
         (mode (cdr (assoc language omnilsp-language-modes)))
         (symbol (omnilsp-get case "symbol"))
         (family (omnilsp-get case "family"))
         (buffer (find-file-noselect path))
         (server nil)
         (publish-count-before (length (omnilsp-publish-events-for-file path)))
         (observed nil))
    (unless mode (error "No Eglot mode for %s" language))
    (unwind-protect
        (with-current-buffer buffer
          (setq default-directory (file-name-directory path))
          ;; Eglot's ElDoc hover callback only runs for a displayed buffer.
          (set-window-buffer (selected-window) buffer)
          (funcall mode)
          (setq-local eglot-send-changes-idle-time 0.05)
          (setq-local flymake-no-changes-timeout 0.05)
          (eglot-ensure)
          ;; Batch mode has no interactive command loop to run this hook.
          (run-hooks 'post-command-hook)
          (omnilsp-wait (lambda () (and (eglot-managed-p)
                                        (eglot-current-server)))
                        "Eglot to manage the fixture buffer" 60)
          (setq server (eglot-current-server))
          (setq omnilsp-shutdown-request-observation nil
                omnilsp-exit-notification-sent nil
                omnilsp-jsonrpc-cleanup-observation nil)
          (cl-pushnew server omnilsp-servers)
          (unless (bound-and-true-p flymake-mode) (flymake-mode 1))
          (let* ((before (omnilsp-snapshot-epoch server))
                 (comment (if (equal language "python") "\n# client-sync-probe\n"
                            "\n// client-sync-probe\n")))
            (goto-char (point-max))
            (insert comment)
            (omnilsp-flush-batch-document-change)
            (omnilsp-wait (lambda () (> (omnilsp-snapshot-epoch server) before))
                          "OmniLSP snapshot after Eglot document change" 15)
            (setq observed (omnilsp-put observed "snapshot_epoch_before" before))
            (setq observed (omnilsp-put observed "snapshot_epoch_after"
                                         (omnilsp-snapshot-epoch server))))
          (let* ((occurrences (omnilsp-find-occurrences symbol))
                 (definition (nth 0 occurrences))
                 (use (nth 1 occurrences)))
            (unless (and definition use) (error "Fixture lacks declaration/use of %s" symbol))
            (setq observed (omnilsp-put observed "hover"
                                         (omnilsp-hover definition symbol
                                                        (equal language "rust"))))
            (goto-char use)
            (let* ((items (omnilsp-xref-items 'definitions symbol))
                   (expected-line (line-number-at-pos definition)))
              (unless (cl-some (lambda (item)
                                 (let ((location (omnilsp-xref-loc item)))
                                   (and (omnilsp-same-file-p (car location) path)
                                        (= (or (cdr location) 0) expected-line)))) items)
                (error "Emacs Xref missed fixture definition at line %d" expected-line))
              (setq observed (omnilsp-put observed "definition_count" (length items))))
            (goto-char (+ use (min 5 (length symbol))))
            (setq observed (omnilsp-put observed "completion_candidates"
                                         (omnilsp-completions-at (point) symbol)))
            (goto-char use)
            (let* ((items (omnilsp-xref-items 'references symbol))
                   (decl-line (line-number-at-pos definition))
                   (use-line (line-number-at-pos use)))
              (unless (and (cl-some (lambda (item)
                                      (let ((loc (omnilsp-xref-loc item)))
                                        (and (omnilsp-same-file-p (car loc) path)
                                             (= (or (cdr loc) 0) decl-line)))) items)
                           (cl-some (lambda (item)
                                      (let ((loc (omnilsp-xref-loc item)))
                                        (and (omnilsp-same-file-p (car loc) path)
                                             (= (or (cdr loc) 0) use-line)))) items))
                (error "Emacs Xref references omitted the fixture declaration or use"))
              (setq observed (omnilsp-put observed "reference_count" (length items))))
            (let ((source-before (buffer-substring-no-properties (point-min) (point-max)))
                  (disk-before (with-temp-buffer
                                 (insert-file-contents-literally path)
                                 (buffer-string)))
                  (rename-error nil))
              (goto-char definition)
              (condition-case err
                  (eglot-rename (omnilsp-rename-name case))
                (error (setq rename-error (format "%s %S"
                                                   (error-message-string err) err))))
              (unless (and rename-error (omnilsp-rename-refusal-p case rename-error))
                (error "Unsafe Eglot rename did not give expected refusal: %s"
                       (or rename-error "no error")))
              (unless (equal source-before (buffer-substring-no-properties (point-min) (point-max)))
                (error "Eglot rename refusal changed the buffer"))
              (unless (equal disk-before
                             (with-temp-buffer (insert-file-contents-literally path) (buffer-string)))
                (error "Eglot rename refusal changed the file"))
              (setq observed (omnilsp-put observed "rename_refusal" rename-error)))
            (if (equal family "cpp")
                (progn
                  (let ((snapshot-before (omnilsp-snapshot-epoch server)))
                    (condition-case err
                        (omnilsp-wait
                         (lambda () (> (length (omnilsp-publish-events-for-file path))
                                       publish-count-before))
                         "Eglot publishDiagnostics for clean C/C++ fixture" 20)
                      (error
                       (setq observed
                             (omnilsp-put
                              observed "clean_diagnostic_failure_context"
                              (omnilsp-diagnostic-telemetry
                               server path snapshot-before publish-count-before)))
                       (signal (car err) (cdr err)))))
                  (let* ((event (car (omnilsp-publish-events-for-file path)))
                         (diagnostics (omnilsp-normalize-list
                                       (plist-get event :diagnostics))))
                    (unless (null diagnostics)
                      (error "Clean C/C++ source produced publishDiagnostics: %S" diagnostics))
                    (setq observed (omnilsp-put observed "diagnostic_delivery" "Eglot publishDiagnostics"))
                    (setq observed (omnilsp-put observed "published_count" 0)))
                  (setq observed (omnilsp-put observed "diagnostic_scope"
                                               "clean_source_no_false_positive"))
                  (setq observed (omnilsp-put observed "deferred_capability" "DEF-CCLSDIAG"))
                  (unless (null (flymake-diagnostics))
                    (error "Clean C/C++ source produced Flymake diagnostics")))
              (let* ((probe (omnilsp-diagnostic-probe case))
                     (needle "omnilspMissingSymbol")
                     (snapshot-before (omnilsp-snapshot-epoch server))
                     (publish-count-before-probe
                      (length (omnilsp-publish-events-for-file path)))
                     diagnostic-position diagnostic change-epoch)
                (goto-char (point-max))
                (insert "\n" probe "\n")
                (setq diagnostic-position (save-excursion
                                           (goto-char (point-max))
                                           (search-backward needle)
                                           (point)))
                (condition-case err
                    (progn
                      (omnilsp-flush-batch-document-change)
                      (setq change-epoch
                            (omnilsp-wait-for-snapshot-epoch
                             server snapshot-before (if (equal language "rust") 60 15)
                             "OmniLSP snapshot after unresolved-name diagnostic probe"))
                      ;; rust-analyzer's compiler diagnostics are save-triggered.
                      ;; Save only after didChange is known to have reached the
                      ;; server, otherwise didSave compiles the previous snapshot.
                      (when (equal language "rust")
                        (save-buffer)
                        (setq change-epoch
                              (omnilsp-wait-for-snapshot-epoch
                               server change-epoch 60
                               "OmniLSP snapshot after saving the Rust diagnostic probe")))
                      (setq observed
                            (omnilsp-put observed "diagnostic_snapshot_epoch_before"
                                         snapshot-before))
                      (setq observed
                            (omnilsp-put observed "diagnostic_snapshot_epoch_after"
                                         change-epoch))
                      (flymake-start t)
                      (setq diagnostic
                            (omnilsp-find-flymake-diagnostic
                             needle diagnostic-position
                             (if (equal language "rust") 60 20)))
                      (unless diagnostic
                        (error "Flymake did not show unresolved-name diagnostic for %s"
                               language)))
                  (error
                   (setq observed
                         (omnilsp-put
                          observed "diagnostic_failure_context"
                          (omnilsp-diagnostic-failure-context
                           server path needle diagnostic-position snapshot-before
                           publish-count-before-probe)))
                   (signal (car err) (cdr err))))
                (setq observed (omnilsp-put observed "diagnostic_scope" "semantic_unresolved_name"))
                (setq observed (omnilsp-put observed "diagnostic_type"
                                             (format "%s" (flymake-diagnostic-type diagnostic))))
                (setq observed (omnilsp-put observed "diagnostic_message"
                                             (flymake-diagnostic-text diagnostic))))))
          (omnilsp-set-row row "observed" observed)
          (omnilsp-set-row row "status" "passed"))
      (when server
        (condition-case err
            (progn
              (unless (buffer-live-p buffer)
                (error "Managed Eglot buffer died before graceful shutdown"))
              (with-current-buffer buffer
                (when (timerp eglot--change-idle-timer)
                  (cancel-timer eglot--change-idle-timer))
                (setq eglot--change-idle-timer nil)
                (let* ((process (jsonrpc--process server)))
                  (setq observed
                        (omnilsp-put observed "server_process_pid" (process-id process)))
                  (unwind-protect
                      (eglot-shutdown server nil)
                    (setq observed
                          (omnilsp-put observed "shutdown_request"
                                       (or omnilsp-shutdown-request-observation
                                           '(("responseReceived" . :json-false)
                                             ("error" . "shutdown request was not observed")))))
                    (setq observed
                          (omnilsp-put observed "exit_notification_sent"
                                       (if omnilsp-exit-notification-sent t :json-false)))
                    (when omnilsp-jsonrpc-cleanup-observation
                      (setq observed
                            (omnilsp-put observed "jsonrpc_cleanup"
                                         omnilsp-jsonrpc-cleanup-observation)))
                    (omnilsp-set-row row "observed" observed))
                  (let ((status (process-status process))
                        (exit-code (process-exit-status process)))
                    (setq observed
                          (omnilsp-put observed "server_process_exit_status"
                                       (format "%s/%d" status exit-code)))
                    (omnilsp-set-row row "observed" observed)
                    (unless (and (eq status 'exit) (= exit-code 0))
                      (setq omnilsp-shutdown-ok nil)
                      (error "Eglot server did not exit cleanly: status=%s code=%d"
                             status exit-code))))
                (cl-incf omnilsp-shutdown-count)))
          (error
           (setq omnilsp-shutdown-ok nil)
           (omnilsp-set-row row "status" "failed")
           (omnilsp-set-row row "cleanupError" (error-message-string err))
           (omnilsp-set-row row "error"
                            (format "Eglot shutdown failed: %s"
                                    (error-message-string err)))))
      (when (buffer-live-p buffer)
        (with-current-buffer buffer (set-buffer-modified-p nil))
        (kill-buffer buffer))))))

(defun omnilsp-run-case (case)
  (let ((row (omnilsp-case-row case "running")))
    (setq omnilsp-result
          (omnilsp-put omnilsp-result "cases"
                       (append (omnilsp-get omnilsp-result "cases") (list row))))
    (omnilsp-write-result)
    (let ((missing (omnilsp-missing-tools case)))
      (if missing
          (progn
            (omnilsp-set-row row "status" "not_verified")
            (omnilsp-set-row row "reason"
                             (format "locked prerequisites unavailable: %s"
                                     (string-join missing ", "))))
        (condition-case err
          (omnilsp-run-case-body case row)
          (error
           (omnilsp-set-row row "status" "failed")
           (omnilsp-set-row
            row "error"
            (if-let ((cleanup (omnilsp-get row "cleanupError")))
                (format "%s (Eglot cleanup: %s)"
                        (error-message-string err) cleanup)
              (error-message-string err)))))
      (omnilsp-write-result)))))

(defun omnilsp-run ()
  (condition-case err
      (let* ((manifest (getenv "OMNILSP_CLIENT_CASES"))
             (case-only (string-trim (or (getenv "OMNILSP_CLIENT_CASE_ONLY") "")))
             (cases nil))
        (unless (and manifest (file-readable-p manifest))
          (error "OMNILSP_CLIENT_CASES must name the generated manifest"))
        (setq omnilsp-workspace (file-name-directory (file-truename manifest)))
        (setq omnilsp-tool-status
              (json-parse-string (or (getenv "OMNILSP_CLIENT_TOOL_STATUS") "{}")
                                 :object-type 'alist :array-type 'list
                                 :null-object nil :false-object :json-false))
        (setq omnilsp-shutdown-ok t)
        (setq omnilsp-shutdown-count 0)
        (setq cases (omnilsp-read-json-file manifest))
        (when (and (not (string-empty-p case-only))
                   (not (cl-some (lambda (case) (equal (omnilsp-get case "name") case-only)) cases)))
          (error "Unknown OMNILSP_CLIENT_CASE_ONLY case %S" case-only))
        (omnilsp-define-modes)
        (setq eglot-sync-connect t eglot-autoshutdown t eglot-send-changes-idle-time 0.05)
        (setq xref-file-name-display 'abs)
        (dolist (case cases)
          (if (or (string-empty-p case-only)
                  (equal (omnilsp-get case "name") case-only))
              (omnilsp-run-case case)
            (let ((row (omnilsp-case-row case "not_verified")))
              (omnilsp-set-row row "reason"
                               (format "not executed because OMNILSP_CLIENT_CASE_ONLY=%s" case-only))
              (setq omnilsp-result
                    (omnilsp-put omnilsp-result "cases"
                                 (append (omnilsp-get omnilsp-result "cases") (list row))))
              (omnilsp-write-result))))
        (let* ((rows (omnilsp-get omnilsp-result "cases"))
               (statuses (mapcar (lambda (row) (omnilsp-get row "status")) rows)))
          (setq omnilsp-result
                (omnilsp-put omnilsp-result "clientTestsCompleted"
                             (if (member "failed" statuses) :json-false t)))
          (setq omnilsp-result
                (omnilsp-put omnilsp-result "serverExitConfirmed"
                             (if omnilsp-shutdown-ok t :json-false)))
          (setq omnilsp-result
                (omnilsp-put omnilsp-result "serverExitEvidence"
                             (cond
                              ((not omnilsp-shutdown-ok)
                               "At least one Eglot server did not complete eglot-shutdown")
                              ((= omnilsp-shutdown-count (length omnilsp-servers))
                               (format "Eglot eglot-shutdown returned for %d server(s); outer runner verifies process-tree exit"
                                       omnilsp-shutdown-count))
                              (t "No Eglot server was started; outer runner verifies no client descendants remain"))))
          (setq omnilsp-result
                (omnilsp-put omnilsp-result "status"
                             (cond ((member "failed" statuses) "failed")
                                   ((member "not_verified" statuses) "not_verified")
                                   (t "passed")))))
        (setq omnilsp-result (omnilsp-put omnilsp-result "cleanExit" :json-false))
        (omnilsp-write-result)
        (when (equal (omnilsp-get omnilsp-result "status") "failed") (kill-emacs 1)))
    (error
     (setq omnilsp-result (omnilsp-put omnilsp-result "status" "failed"))
     (setq omnilsp-result (omnilsp-put omnilsp-result "error" (error-message-string err)))
     (setq omnilsp-result (omnilsp-put omnilsp-result "cleanExit" :json-false))
     (omnilsp-write-result)
     (kill-emacs 1))))

(omnilsp-run)

;;; runner.el ends here
