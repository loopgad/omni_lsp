#!/usr/bin/env python3
"""Prepare and validate isolated Zed profiles; never claim a UI smoke pass.

Zed ignores project settings and language-server launches in restricted
worktrees. This preflight explicitly trusts only the disposable user-data
profile and still returns not_verified until UI-visible results are captured.
"""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
from typing import Any


EXPECTED_ZED_VERSION = "1.21.0"
CASES = (
    ("go", "Go", "go", "gopls"),
    ("c", "C", "cpp", "clangd"),
    ("cpp", "C++", "cpp", "clangd"),
    ("rust", "Rust", "rust", "rust-analyzer"),
    ("python", "Python", "python", "basedpyright"),
    ("typescript", "TypeScript", "typescript", "vtsls"),
    ("typescriptreact", "TSX", "typescript", "vtsls"),
    ("javascript", "JavaScript", "typescript", "vtsls"),
    ("javascriptreact", "JSX", "typescript", "vtsls"),
)


def build_project_settings(server: Path) -> dict[str, Any]:
    languages: dict[str, dict[str, list[str]]] = {}
    adapters: set[str] = set()
    for _case_name, zed_language, _family, adapter in CASES:
        languages[zed_language] = {"language_servers": [adapter]}
        adapters.add(adapter)
    lsp = {
        adapter: {
            "binary": {
                "path": str(server.resolve()),
                "arguments": ["serve"],
                "env": {"OMNILSP_TRUST": "trusted"},
            }
        }
        for adapter in sorted(adapters)
    }
    return {"languages": languages, "lsp": lsp}


def validate_settings(project: dict[str, Any], user: dict[str, Any], server: Path) -> dict[str, Any]:
    languages = project.get("languages")
    lsp = project.get("lsp")
    if not isinstance(languages, dict) or not isinstance(lsp, dict):
        raise ValueError("project settings must define language and lsp objects")

    mappings: dict[str, dict[str, str]] = {}
    for case_name, zed_language, _family, adapter in CASES:
        language_config = languages.get(zed_language)
        if not isinstance(language_config, dict) or language_config.get("language_servers") != [adapter]:
            raise ValueError(f"Zed language {zed_language!r} must select only adapter {adapter!r}")
        adapter_config = lsp.get(adapter)
        binary = adapter_config.get("binary") if isinstance(adapter_config, dict) else None
        if not isinstance(binary, dict):
            raise ValueError(f"Zed adapter {adapter!r} has no binary configuration")
        if os.path.normcase(os.path.normpath(str(Path(binary.get("path", "")).resolve()))) != os.path.normcase(
            os.path.normpath(str(server.resolve()))
        ):
            raise ValueError(f"Zed adapter {adapter!r} does not resolve to the requested candidate binary")
        if binary.get("arguments") != ["serve"]:
            raise ValueError(f"Zed adapter {adapter!r} arguments must be exactly [\"serve\"]")
        if binary.get("env") != {"OMNILSP_TRUST": "trusted"}:
            raise ValueError(f"Zed adapter {adapter!r} must set OMNILSP_TRUST=trusted")
        mappings[case_name] = {"zed_language": zed_language, "adapter": adapter}

    if user.get("session", {}).get("trust_all_worktrees") is not True:
        raise ValueError("isolated Zed user settings must set session.trust_all_worktrees=true")
    return {"trust": "trusted disposable worktrees", "languages": mappings}


def read_json_object(path: Path) -> dict[str, Any]:
    if not path.exists():
        return {}
    value = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(value, dict):
        raise ValueError(f"settings file must contain a JSON object: {path}")
    return value


def merge_object(existing: dict[str, Any], updates: dict[str, Any]) -> dict[str, Any]:
    result = dict(existing)
    for key, value in updates.items():
        if isinstance(value, dict) and isinstance(result.get(key), dict):
            result[key] = merge_object(result[key], value)
        else:
            result[key] = value
    return result


def write_settings(path: Path, settings: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(settings, ensure_ascii=False, indent=2) + "\n", encoding="utf-8", newline="\n")


def runtime_blocker(binary: str | None) -> str:
    candidate = binary or shutil.which("zed") or shutil.which("Zed.exe")
    if not candidate:
        return (
            f"Pinned Zed stable {EXPECTED_ZED_VERSION} is unavailable: no --zed-bin or zed executable on PATH. "
            "This host also has no verified UI automation that performs editor actions and checks rendered results."
        )
    try:
        probe = subprocess.run(
            [candidate, "--version"],
            check=False,
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=10,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        return f"Zed version probe failed for {candidate!r}: {exc}; client-visible results remain unobserved."
    version = (probe.stdout or probe.stderr).strip()
    if probe.returncode != 0:
        return f"Zed version probe exited {probe.returncode} for {candidate!r}; client-visible results remain unobserved."
    if EXPECTED_ZED_VERSION not in version or "preview" in version.lower():
        return (
            f"Zed reports {version!r}; expected stable {EXPECTED_ZED_VERSION}. The preview build is not a pinned "
            "acceptance client, and no UI-visible result adapter is configured."
        )
    return (
        f"Pinned Zed stable {EXPECTED_ZED_VERSION} is present, but no native UI action/result capture adapter is "
        "configured. Without client-visible hover, definition, completion, diagnostics, references, and rename "
        "refusal evidence, the cases remain not_verified."
    )


def result_rows(reason: str) -> list[dict[str, str]]:
    return [
        {
            "name": case_name,
            "languageId": case_name,
            "family": family,
            "status": "not_verified",
            "reason": reason,
        }
        for case_name, _zed_language, family, _adapter in CASES
    ]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--workspace", required=True, type=Path)
    parser.add_argument("--user-data-dir", required=True, type=Path)
    parser.add_argument("--omnilsp-bin", required=True, type=Path)
    parser.add_argument("--zed-bin", help="optional pinned stable Zed executable path")
    parser.add_argument("--result", type=Path, help="write the structured client result JSON")
    args = parser.parse_args()

    workspace = args.workspace.resolve()
    user_data = args.user_data_dir.resolve()
    server = args.omnilsp_bin.resolve()
    if not workspace.is_dir():
        parser.error(f"workspace directory does not exist: {workspace}")
    if not server.is_file():
        parser.error(f"OmniLSP candidate binary does not exist: {server}")

    project_path = workspace / ".zed" / "settings.json"
    user_path = user_data / "User" / "settings.json"
    try:
        project = merge_object(read_json_object(project_path), build_project_settings(server))
        user = merge_object(read_json_object(user_path), {"session": {"trust_all_worktrees": True}})
        write_settings(project_path, project)
        write_settings(user_path, user)
        observed = validate_settings(
            read_json_object(project_path),
            read_json_object(user_path),
            server,
        )
    except (OSError, json.JSONDecodeError, ValueError) as exc:
        print(json.dumps({"client": "zed", "status": "failed", "error": str(exc)}, indent=2))
        return 1

    blocker = runtime_blocker(args.zed_bin)
    result = {
        "client": "zed",
        "status": "not_verified",
        "stage": "native-result-consumption",
        "cleanExit": False,
        "clientTestsCompleted": False,
        "serverExitConfirmed": False,
        "error": blocker,
        "cases": result_rows(blocker),
        "observed": {
            "project_settings_path": str(project_path),
            "isolated_user_settings_path": str(user_path),
            "settings_contract": "passed",
            "language_id_mapping": {
                "status": "not_verified",
                "reason": (
                    "This preflight validates Zed language names and adapter settings, not the languageId sent by "
                    "the client. In particular, JSX→javascriptreact is not established by the reviewed vtsls "
                    "adapter source; keep JSX unverified until a pinned-client didOpen observation confirms it."
                ),
            },
            "native_result_observed": False,
            "profile": observed,
        },
    }
    serialized = json.dumps(result, ensure_ascii=False, indent=2) + "\n"
    if args.result:
        args.result.parent.mkdir(parents=True, exist_ok=True)
        args.result.write_text(serialized, encoding="utf-8", newline="\n")
    print(serialized, end="")
    return 0


if __name__ == "__main__":
    sys.exit(main())
