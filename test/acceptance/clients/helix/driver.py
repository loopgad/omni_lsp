#!/usr/bin/env python3
"""Prepare and validate a Helix profile; never claim a native smoke pass.

The profile contract is useful before Helix is available, but a successful
contract check is not evidence that Helix consumed an LSP result. The native
TUI driver remains fail-closed until it can capture visible results.
"""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tomllib
from typing import Any


EXPECTED_HELIX_VERSION = "25.07.1"
CASES = (
    ("go", "go", "go"),
    ("c", "c", "cpp"),
    ("cpp", "cpp", "cpp"),
    ("rust", "rust", "rust"),
    ("python", "python", "python"),
    ("typescript", "typescript", "typescript"),
    ("typescriptreact", "tsx", "typescript"),
    ("javascript", "javascript", "typescript"),
    ("javascriptreact", "jsx", "typescript"),
)


def normalized_path(path: str | Path) -> str:
    return os.path.normcase(os.path.normpath(str(Path(path).resolve())))


def render_profile(server: Path) -> str:
    # JSON strings are a compatible subset of TOML basic strings.
    server_value = json.dumps(str(server.resolve()), ensure_ascii=False)
    lines = [
        "[language-server.omnilsp]",
        f"command = {server_value}",
        'args = ["serve"]',
        'environment = { "OMNILSP_TRUST" = "trusted" }',
        "",
    ]
    for case_name, helix_name, _family in CASES:
        lines.extend(
            [
                "[[language]]",
                f"name = {json.dumps(helix_name)}",
                f"language-id = {json.dumps(case_name)}",
                'language-servers = ["omnilsp"]',
                "",
            ]
        )
    return "\n".join(lines)


def validate_profile(profile_text: str, server: Path) -> dict[str, Any]:
    config = tomllib.loads(profile_text)
    server_config = config.get("language-server", {}).get("omnilsp")
    if not isinstance(server_config, dict):
        raise ValueError("profile is missing [language-server.omnilsp]")
    if normalized_path(server_config.get("command", "")) != normalized_path(server):
        raise ValueError("omnilsp command does not resolve to the requested candidate binary")
    if server_config.get("args") != ["serve"]:
        raise ValueError('omnilsp args must be exactly ["serve"]')
    if server_config.get("environment") != {"OMNILSP_TRUST": "trusted"}:
        raise ValueError('omnilsp environment must set OMNILSP_TRUST="trusted"')

    languages = config.get("language", [])
    if not isinstance(languages, list):
        raise ValueError("profile has no [[language]] entries")
    by_name: dict[str, list[dict[str, Any]]] = {}
    for language in languages:
        if isinstance(language, dict) and isinstance(language.get("name"), str):
            by_name.setdefault(language["name"], []).append(language)

    mappings: dict[str, dict[str, str]] = {}
    for case_name, helix_name, _family in CASES:
        entries = by_name.get(helix_name, [])
        if len(entries) != 1:
            raise ValueError(f"expected exactly one Helix profile entry named {helix_name!r}; found {len(entries)}")
        entry = entries[0]
        if entry.get("language-id") != case_name:
            raise ValueError(
                f"Helix language {helix_name!r} sends language-id {entry.get('language-id')!r}; "
                f"expected {case_name!r}"
            )
        if entry.get("language-servers") != ["omnilsp"]:
            raise ValueError(f"Helix language {helix_name!r} must select only the omnilsp server")
        mappings[case_name] = {"helix_language": helix_name, "language_id": case_name}

    return {
        "server": str(server.resolve()),
        "trust": "trusted",
        "language_ids": mappings,
    }


def runtime_blocker(binary: str | None) -> str:
    candidate = binary or shutil.which("hx") or shutil.which("helix")
    if not candidate:
        return (
            f"Helix {EXPECTED_HELIX_VERSION} is unavailable: neither hx nor helix is on PATH and no "
            "--helix-bin was supplied. Even with the pinned binary, this host has no native TUI result "
            "capture adapter; server traces alone cannot pass the client smoke."
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
        return f"Helix runtime probe failed for {candidate!r}: {exc}; native client results remain unobserved."
    version = (probe.stdout or probe.stderr).strip()
    if probe.returncode != 0:
        return f"Helix version probe exited {probe.returncode} for {candidate!r}; native client results remain unobserved."
    if EXPECTED_HELIX_VERSION not in version:
        return f"Helix reports {version!r}; expected pinned version {EXPECTED_HELIX_VERSION}; native client results remain unobserved."
    return (
        f"Pinned Helix {EXPECTED_HELIX_VERSION} is present, but no native TUI action/result capture adapter is "
        "configured. Without client-visible hover, definition, completion, diagnostics, references, and rename "
        "refusal evidence, the case remains not_verified."
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
        for case_name, _helix_name, family in CASES
    ]


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--workspace", required=True, type=Path)
    parser.add_argument("--omnilsp-bin", required=True, type=Path)
    parser.add_argument("--helix-bin", help="optional pinned Helix executable path")
    parser.add_argument("--result", type=Path, help="write the structured client result JSON")
    args = parser.parse_args()

    workspace = args.workspace.resolve()
    server = args.omnilsp_bin.resolve()
    if not workspace.is_dir():
        parser.error(f"workspace directory does not exist: {workspace}")
    if not server.is_file():
        parser.error(f"OmniLSP candidate binary does not exist: {server}")
    profile_path = workspace / ".helix" / "languages.toml"
    profile_path.parent.mkdir(parents=True, exist_ok=True)
    profile_path.write_text(render_profile(server), encoding="utf-8", newline="\n")

    try:
        profile_observed = validate_profile(profile_path.read_text(encoding="utf-8"), server)
        if os.environ.get("OMNILSP_TRUST") != "trusted":
            raise ValueError('driver environment must contain OMNILSP_TRUST="trusted"')
    except (OSError, tomllib.TOMLDecodeError, ValueError) as exc:
        print(json.dumps({"client": "helix", "status": "failed", "error": str(exc)}, indent=2))
        return 1

    blocker = runtime_blocker(args.helix_bin)
    result = {
        "client": "helix",
        "status": "not_verified",
        "stage": "native-result-consumption",
        "cleanExit": False,
        "clientTestsCompleted": False,
        "serverExitConfirmed": False,
        "error": blocker,
        "cases": result_rows(blocker),
        "observed": {
            "profile_path": str(profile_path),
            "profile_contract": "passed",
            "native_result_observed": False,
            "profile": profile_observed,
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
