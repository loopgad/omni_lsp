# Emacs / Eglot profile

Eglot is included with GNU Emacs 29 and later. Map the major modes actually
used for the fixture files to OmniLSP; add `:language-id` where a mode's
default LSP identifier differs from the fixture's identifier:

```elisp
(with-eval-after-load 'eglot
  (add-to-list
   'eglot-server-programs
   '(((go-mode :language-id "go")
      (go-ts-mode :language-id "go")
      (c-mode :language-id "c")
      (c++-mode :language-id "cpp")
      (rust-mode :language-id "rust")
      (rust-ts-mode :language-id "rust")
      (python-mode :language-id "python")
      (python-ts-mode :language-id "python")
      (js-mode :language-id "javascript")
      (js-ts-mode :language-id "javascript")
      (js-jsx-mode :language-id "javascriptreact")
      (typescript-ts-mode :language-id "typescript")
      (tsx-ts-mode :language-id "typescriptreact"))
     . ("omnilsp" "serve"))))
```

Use the corresponding available mode for each file. Tree-sitter modes require
their grammar; if the pinned Emacs build does not provide a fixture's major
mode, or it cannot send the required language identifier, that case is blocked
until the mode and any grammar are supplied and pinned.
Eglot itself uses Emacs's xref, completion-at-point, ElDoc, and Flymake
integrations for definition/references, completion, hover documentation, and
diagnostics. Feature evidence must be read back from those client-facing
integrations after the real buffer is opened and changed; an LSP trace alone
does not establish a pass. The refused rename must leave both the buffer and
file unchanged, and the Eglot server process must shut down cleanly.

**Host check (Windows/amd64, 2026-10-02):** the locked GNU Emacs `30.2`
portable binary is present and its SHA-256 matches `tools.lock.json`. The
native runner has exercised Eglot's actual client APIs. The nine-case batch
passed against development candidate `ff404be3952361964284ee5ec192dfb736708b51d9150764c930e1b1b0c22838`,
including Go's client-visible Flymake diagnostic and clean server/client exit.
Independent review confirmed native operation and result consumption. The
formal matrix now enables this driver, but every frozen candidate must repeat
the run; this development result is not a complete release pass.
The Emacs runner records the shutdown response, exit notification, server PID,
and process exit status without accepting a trace as feature evidence.
