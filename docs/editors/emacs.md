# Emacs Client Profile (§S11)

LSP-mode (recommended):

```elisp
(with-eval-after-load 'lsp-mode
  (add-to-list 'lsp-language-server-configuration
               '(go-mode . ("omnilsp" "serve"))))
```

Eglot alternative:

```elisp
(add-to-list 'eglot-server-programs
             '((go-mode rust-mode python-mode) . ("omnilsp" "serve")))
```

Notes:

- omnilsp speaks stdio JSON-RPC 3.17; no extra capabilities flags needed.
- UTF-16 position encoding is negotiated automatically (C4).
- Rename is fail-closed without build context — refusals arrive as normal
  LSP errors, not editor crashes.
