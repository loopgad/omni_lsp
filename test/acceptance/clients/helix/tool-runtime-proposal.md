# Project-local Helix runtime proposal

- Pin: Helix `25.07.1`, x86_64 Windows, release commit `a05c151`.
- Official archive: <https://github.com/helix-editor/helix/releases/download/25.07.1/helix-25.07.1-x86_64-windows.zip>
- Official GitHub release API SHA-256: `5c8325ced8bacd8418d62706f669e96d9c3578a9237526e34d546900cbc049b6`.
- Verified locally; extracted only `hx.exe` and the release `runtime/` into ignored `test/acceptance/tools/bin/helix-25.07.1/`. No global install.
- `hx.exe` SHA-256: `2e1c9583b295f55c2bca55f1ee9793640f9af61accdbef6ea15e31679ea9ab76`.
- Extracted `runtime/`: 1,228 files, 193,428,854 bytes. Set `HELIX_RUNTIME` to that directory's `runtime` when launching.
- `hx.exe --version` reports `helix 25.07.1 (a05c151b)`.
- Integration proposal: add this URL and digest to the acceptance tool manifest/installer, then pass the extracted binary to the Helix profile runner. This note does not change the shared manifest or runner.
