import json
import os
import re
import threading
import time
from pathlib import Path
from urllib.parse import unquote, urlparse

import sublime
import sublime_plugin


_LSP_IMPORT_ERROR = None


def _native_case_evidence_identity():
    identity = {}
    bootstrap_identity = Path(__file__).with_name("acceptance_identity.json")
    try:
        with bootstrap_identity.open("r", encoding="utf-8") as source:
            bootstrap = json.load(source)
        if isinstance(bootstrap, dict):
            run_id = bootstrap.get("run_id", "")
            candidate_hash = bootstrap.get("candidate_sha256", "")
            if isinstance(run_id, str) and run_id.strip():
                identity["run_id"] = run_id.strip()[:128]
            if isinstance(candidate_hash, str) and candidate_hash.strip():
                identity["candidate_sha256"] = candidate_hash.strip().lower()[:64]
    except FileNotFoundError:
        pass
    except (OSError, ValueError, TypeError) as exc:
        identity["identity_bootstrap_error"] = (type(exc).__name__ + ": " + str(exc))[:512]
    run_id = os.environ.get("OMNILSP_SUBLIME_GO_RUN_ID", "").strip()
    candidate_hash = os.environ.get("OMNILSP_SUBLIME_GO_CANDIDATE_SHA256", "").strip().lower()
    if run_id and "run_id" not in identity:
        identity["run_id"] = run_id[:128]
    if candidate_hash and "candidate_sha256" not in identity:
        identity["candidate_sha256"] = candidate_hash[:64]
    return identity


def _write_lsp_import_failure(message):
    result_path = os.environ.get("OMNILSP_CLIENT_RESULT", "")
    if not result_path:
        return
    result = {
        "client": "sublime-lsp",
        "status": "failed",
        "error": "Sublime LSP import failed before acceptance plugin initialization: " + message[:4096],
        "cleanExit": False,
        "clientTestsCompleted": False,
        "serverExitConfirmed": False,
        "serverExitEvidence": "Sublime LSP import failed before an LSP session could start",
        "cases": [],
    }
    result.update(_native_case_evidence_identity())
    try:
        os.makedirs(os.path.dirname(os.path.abspath(result_path)), exist_ok=True)
        temporary = result_path + ".tmp"
        with open(temporary, "w", encoding="utf-8", newline="\n") as output:
            json.dump(result, output, indent=2, ensure_ascii=False)
            output.write("\n")
        os.replace(temporary, result_path)
    except Exception as exc:
        print("omnilsp_acceptance: could not write bounded startup failure result: " + str(exc)[:1024])


try:
    from LSP.plugin import LspPlugin
    from LSP.plugin.core.protocol import Request
except Exception as exc:
    _LSP_IMPORT_ERROR = (type(exc).__name__ + ": " + str(exc))[:4096]
    _write_lsp_import_failure(_LSP_IMPORT_ERROR)
    LspPlugin = object
    Request = None


_lock = threading.Lock()
_client_requests = []
_client_notifications = []
_responses = []
_notifications = []
_rename_results = []
_completion_consumptions = []
_session_plugins = []
_driver = None
_registration_requested = False
_registration_error = None
_active_completion_capture = None
_completion_consumer_context = threading.local()
_pending_completion_lists = {}
_client_completion_hooks_installed = False
_active_hover_capture = None
_hover_popup_events = []
_hover_capture_sequence = 0
_completion_capture_sequence = 0
_COMPLETION_CONSUMER_TIMEOUT_SECONDS = 2.0
_NATIVE_CONSUMER_TIMEOUT_SECONDS = 5.0
_syntax_by_case = {
    "go": ("OmniLSP-Go.sublime-syntax", "source.go"),
    "c": ("OmniLSP-C.sublime-syntax", "source.c"),
    "cpp": ("OmniLSP-Cpp.sublime-syntax", "source.c++"),
    "rust": ("OmniLSP-Rust.sublime-syntax", "source.rust"),
    "python": ("OmniLSP-Python.sublime-syntax", "source.python"),
    "typescript": ("OmniLSP-TypeScript.sublime-syntax", "source.ts"),
    "typescriptreact": ("OmniLSP-TSX.sublime-syntax", "source.tsx"),
    "javascript": ("OmniLSP-JavaScript.sublime-syntax", "source.js"),
    "javascriptreact": ("OmniLSP-JSX.sublime-syntax", "source.jsx"),
}


def _plain(value):
    if isinstance(value, dict):
        return {str(key): _plain(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [_plain(item) for item in value]
    if value is None or isinstance(value, (str, int, float, bool)):
        return value
    return str(value)


def _record(target, value):
    with _lock:
        target.append(_plain(value))


def _completion_item_snapshot(item):
    fields = {
        "trigger": getattr(item, "trigger", ""),
        "annotation": getattr(item, "annotation", ""),
        "completion": getattr(item, "completion", ""),
        "completionFormat": getattr(item, "completion_format", None),
        "kind": getattr(item, "kind", None),
        "details": getattr(item, "details", ""),
        "flags": getattr(item, "flags", None),
    }
    return {
        key: (_plain(value)[:2048] if isinstance(value, str) else _plain(value))
        for key, value in fields.items()
    }


def _normalize_completion_trigger(trigger, annotation):
    if not isinstance(trigger, str):
        return None
    if "\t" not in trigger:
        return trigger
    label, suffix = trigger.rsplit("\t", 1)
    if not isinstance(annotation, str) or suffix != annotation:
        return None
    return label


def _completion_select_payload(completion):
    prefix = "lsp_select_completion "
    if not isinstance(completion, str) or not completion.startswith(prefix):
        return None
    try:
        payload = json.loads(completion[len(prefix):])
    except (TypeError, ValueError):
        return None
    if (not isinstance(payload, dict) or set(payload) != {"index", "session_name"}
            or type(payload.get("index")) is not int
            or not isinstance(payload.get("session_name"), str)):
        return None
    return payload


def _completion_consumption_matches(event, candidate, response_index, session_name, view_id, case_epoch, capture_token):
    if (not isinstance(event, dict) or event.get("targetBound") is not True
            or event.get("candidate") != candidate or event.get("normalizedTrigger") != candidate
            or event.get("viewId") != view_id or event.get("caseEpoch") != case_epoch
            or event.get("captureToken") != capture_token):
        return False
    selection = event.get("selection")
    item = event.get("item")
    if not isinstance(selection, dict) or not isinstance(item, dict):
        return False
    return (selection.get("index") == response_index and selection.get("session_name") == session_name
            and _normalize_completion_trigger(item.get("trigger"), item.get("annotation")) == candidate)


def _hover_popup_consumption_matches(event, view_id, case_epoch, capture_token, candidate):
    content = event.get("content") if isinstance(event, dict) else None
    candidate_content_matches = (isinstance(content, str)
                                 and re.search(r"(?<![\w])" + re.escape(candidate) + r"(?![\w])", content) is not None)
    return (isinstance(event, dict) and event.get("api") == "LSP.plugin.hover.show_lsp_popup"
            and event.get("viewId") == view_id and event.get("caseEpoch") == case_epoch
            and event.get("capturedViewId") == view_id and event.get("captureToken") == capture_token
            and event.get("popupVisible") is True
            and event.get("contentMatchesCandidate") is True and candidate_content_matches)


def _location_range(location):
    if not isinstance(location, dict):
        return None
    target = location.get("targetUri") or location.get("uri")
    target_range = location.get("targetSelectionRange") or location.get("range")
    if not isinstance(target, str) or not isinstance(target_range, dict):
        return None
    start = target_range.get("start")
    end = target_range.get("end")
    if (not isinstance(start, dict) or not isinstance(end, dict)
            or type(start.get("line")) is not int or type(start.get("character")) is not int
            or type(end.get("line")) is not int or type(end.get("character")) is not int):
        return None
    return {"uri": target, "start": start, "end": end}


def _definition_landing_matches(window, expected, source_view_id):
    if not window or not isinstance(expected, dict):
        return None
    try:
        view = window.active_view()
        if not view or not view.is_valid() or view.id() != source_view_id:
            return None
        path = view.file_name()
        if not path or not _same_uri(Path(path).as_uri(), expected.get("uri")):
            return None
        selection = view.sel()
        if len(selection) != 1:
            return None
        line, column = view.rowcol(selection[0].b)
        start = expected["start"]
        end = expected["end"]
        if not (start["line"] <= line <= end["line"]
                and (line != start["line"] or column >= start["character"])
                and (line != end["line"] or column <= end["character"])):
            return None
        return {"viewId": view.id(), "file": os.path.abspath(path), "line": line, "character": column}
    except (AttributeError, IndexError, OSError, RuntimeError, TypeError, ValueError):
        return None


def _references_panel_consumption(window, symbol, expected_lines):
    if not window:
        return None
    try:
        if window.active_panel() != "output.references":
            return None
        panel = window.find_output_panel("references")
        if not panel or not panel.is_valid():
            return None
        content = panel.substr(sublime.Region(0, panel.size()))
        if (not isinstance(content, str) or not all(line and line in content for line in expected_lines)
                or len(re.findall(r"\b" + re.escape(symbol) + r"\b", content)) < 2):
            return None
        return {"panel": "output.references", "lineCount": len(expected_lines), "content": content[:4096]}
    except (AttributeError, RuntimeError, TypeError, ValueError):
        return None


def _did_change_matches(event, uri, previous_version, marker):
    if not isinstance(event, dict) or event.get("method") != "textDocument/didChange":
        return False
    if not _same_uri(event.get("uri"), uri):
        return False
    version = event.get("version")
    if type(version) is not int or (type(previous_version) is int and version <= previous_version):
        return False
    changes = event.get("contentChanges")
    return (isinstance(changes, list) and any(
        isinstance(change, dict) and isinstance(change.get("text"), str)
        and any(line.strip() == marker for line in change["text"].splitlines())
        for change in changes))


def _install_completion_consumption_hook():
    global _client_completion_hooks_installed
    completion_list_type = getattr(sublime, "CompletionList", None)
    original = getattr(completion_list_type, "set_completions", None)
    if not callable(original):
        raise RuntimeError("pinned Sublime API has no CompletionList.set_completions consumer hook")
    if not getattr(original, "_omnilsp_acceptance_completion_hook", False):
        def tracked_set_completions(completion_list, completions, flags=0):
            target_bound = getattr(completion_list, "target", None) is not None
            result = original(completion_list, completions, flags)
            context = getattr(_completion_consumer_context, "value", None)
            if target_bound and isinstance(completions, (list, tuple)) and context:
                with _lock:
                    capture = dict(_active_completion_capture) if _active_completion_capture else None
                if (capture and capture.get("captureToken") == context.get("captureToken")
                        and capture.get("viewId") == context.get("viewId")
                        and capture.get("caseEpoch") == context.get("caseEpoch")):
                    for item in completions:
                        snapshot = _completion_item_snapshot(item)
                        normalized_trigger = _normalize_completion_trigger(
                            snapshot.get("trigger"), snapshot.get("annotation"))
                        selection = _completion_select_payload(snapshot.get("completion"))
                        if (normalized_trigger != capture["candidate"] or not selection
                                or selection.get("session_name") != capture.get("sessionName")):
                            continue
                        with _lock:
                            if (_active_completion_capture
                                    and _active_completion_capture.get("captureToken") == context.get("captureToken")):
                                _completion_consumptions.append({
                                    "candidate": capture["candidate"],
                                    "targetBound": True,
                                    "normalizedTrigger": normalized_trigger,
                                    "selection": selection,
                                    "item": snapshot,
                                    "viewId": context["viewId"],
                                    "caseEpoch": context["caseEpoch"],
                                    "captureToken": context["captureToken"],
                                })
                                del _completion_consumptions[:-64]
            return result

        tracked_set_completions._omnilsp_acceptance_completion_hook = True
        completion_list_type.set_completions = tracked_set_completions

    if _client_completion_hooks_installed:
        return
    try:
        from LSP.plugin.documents import DocumentSyncListener
        original_query = DocumentSyncListener.on_query_completions
        original_resolved = DocumentSyncListener._on_query_completions_resolved_async
    except (AttributeError, ImportError) as exc:
        raise RuntimeError("pinned LSP has no DocumentSyncListener completion handoff: " + str(exc))
    if not callable(original_query) or not callable(original_resolved):
        raise RuntimeError("pinned LSP DocumentSyncListener completion handoff is not callable")
    if not getattr(original_query, "_omnilsp_acceptance_completion_hook", False):
        def tracked_query_completions(listener, *args, **kwargs):
            completion_list = original_query(listener, *args, **kwargs)
            try:
                view_id = listener.view.id()
            except (AttributeError, RuntimeError):
                return completion_list
            with _lock:
                capture = dict(_active_completion_capture) if _active_completion_capture else None
                if (capture and capture.get("viewId") == view_id and completion_list is not None):
                    _pending_completion_lists[id(completion_list)] = {
                        "list": completion_list,
                        "viewId": view_id,
                        "caseEpoch": capture["caseEpoch"],
                        "captureToken": capture["captureToken"],
                    }
            return completion_list

        tracked_query_completions._omnilsp_acceptance_completion_hook = True
        DocumentSyncListener.on_query_completions = tracked_query_completions
    if not getattr(original_resolved, "_omnilsp_acceptance_completion_hook", False):
        def tracked_resolved_completions(listener, completion_list, *args, **kwargs):
            with _lock:
                pending = _pending_completion_lists.get(id(completion_list))
                capture = dict(_active_completion_capture) if _active_completion_capture else None
                if pending and pending.get("list") is completion_list:
                    del _pending_completion_lists[id(completion_list)]
                else:
                    pending = None
            view_id = None
            try:
                view_id = listener.view.id()
            except (AttributeError, RuntimeError):
                pass
            context = None
            if (pending and capture and pending.get("viewId") == view_id
                    and capture.get("viewId") == view_id
                    and pending.get("caseEpoch") == capture.get("caseEpoch")
                    and pending.get("captureToken") == capture.get("captureToken")):
                context = pending
            previous = getattr(_completion_consumer_context, "value", None)
            _completion_consumer_context.value = context
            try:
                return original_resolved(listener, completion_list, *args, **kwargs)
            finally:
                _completion_consumer_context.value = previous

        tracked_resolved_completions._omnilsp_acceptance_completion_hook = True
        DocumentSyncListener._on_query_completions_resolved_async = tracked_resolved_completions
    _client_completion_hooks_installed = True


def _install_hover_popup_consumption_hook():
    import importlib

    hover_module = importlib.import_module("LSP.plugin.hover")
    original = getattr(hover_module, "show_lsp_popup", None)
    if not callable(original):
        raise RuntimeError("pinned LSP hover module has no show_lsp_popup renderer")
    if getattr(original, "_omnilsp_acceptance_hover_hook", False):
        return

    def tracked_show_lsp_popup(view, content, *args, **kwargs):
        with _lock:
            capture = dict(_active_hover_capture) if _active_hover_capture else None
        result = original(view, content, *args, **kwargs)
        if capture:
            try:
                visible = bool(view.is_popup_visible())
                view_id = view.id()
            except (AttributeError, RuntimeError):
                visible = False
                view_id = None
            rendered_content = str(content)[:8192]
            candidate = capture.get("candidate", "")
            candidate_matches = (isinstance(candidate, str) and bool(candidate)
                                 and re.search(r"(?<![\w])" + re.escape(candidate) + r"(?![\w])",
                                               rendered_content) is not None)
            _record(_hover_popup_events, {
                "api": "LSP.plugin.hover.show_lsp_popup",
                "viewId": view_id,
                "capturedViewId": capture.get("viewId"),
                "caseEpoch": capture["caseEpoch"],
                "captureToken": capture["captureToken"],
                "popupVisible": visible,
                "contentMatchesCandidate": candidate_matches,
                "content": rendered_content,
            })
        return result

    tracked_show_lsp_popup._omnilsp_acceptance_hover_hook = True
    hover_module.show_lsp_popup = tracked_show_lsp_popup


class OmniLspAcceptance(LspPlugin):
    def __init__(self, weaksession):
        super().__init__(weaksession)
        self._acceptance_initialized = False
        self._acceptance_ended = None
        with _lock:
            _session_plugins.append(self)

    def on_initialized_async(self):
        with _lock:
            self._acceptance_initialized = True

    def on_pre_send_request_async(self, request, view):
        method = request.get("method") if isinstance(request, dict) else None
        view_path = ""
        if view:
            try:
                view_path = os.path.abspath(view.file_name() or "")
            except Exception:
                pass
        _record(_client_requests, {"method": method, "view_path": view_path})

    def on_pre_send_notification_async(self, notification):
        method = notification.get("method") if isinstance(notification, dict) else None
        params = notification.get("params", {}) if isinstance(notification, dict) else {}
        event = {"method": method}
        if method in ("textDocument/didOpen", "textDocument/didChange") and isinstance(params, dict):
            text_document = params.get("textDocument", {})
            if isinstance(text_document, dict):
                event["uri"] = text_document.get("uri")
                event["version"] = text_document.get("version")
                if method == "textDocument/didOpen":
                    event["languageId"] = text_document.get("languageId")
            if method == "textDocument/didChange":
                changes = params.get("contentChanges", [])
                event["contentChanges"] = [
                    {
                        "text": change.get("text", "")[:8192],
                        "range": _plain(change.get("range")),
                        "rangeLength": change.get("rangeLength"),
                    }
                    for change in changes[:8] if isinstance(change, dict)
                ] if isinstance(changes, list) else []
        _record(_client_notifications, event)

    def on_server_response_async(self, response):
        _record(_responses, response)

    def on_server_notification_async(self, notification):
        _record(_notifications, notification)

    def on_session_end_async(self, exit_code, exception):
        with _lock:
            self._acceptance_ended = {
                "exit_code": exit_code,
                "exception": (type(exception).__name__ + ": " + str(exception))[:1024] if exception else None,
            }
        if _driver:
            _driver.server_exited(exit_code, exception)


def _windows_path(uri):
    parsed = urlparse(uri or "")
    path = unquote(parsed.path or "")
    if re.match(r"^/[A-Za-z]:/", path):
        path = path[1:]
    return os.path.normcase(os.path.abspath(path.replace("/", os.sep)))


def _same_uri(left, right):
    try:
        return _windows_path(left) == _windows_path(right)
    except (OSError, ValueError):
        return False


def _same_window(left, right):
    try:
        return left.id() == right.id()
    except (AttributeError, RuntimeError):
        return left == right


def _location_line(location):
    target_range = _location_range(location)
    if not target_range:
        return None
    return target_range["uri"], target_range["start"]["line"]


def _response_result(response):
    if "error" in response and response["error"]:
        raise RuntimeError("LSP command returned error: " + json.dumps(response["error"]))
    return response.get("result")


def _contents_text(value):
    if isinstance(value, dict):
        return " ".join(_contents_text(item) for item in value.values())
    if isinstance(value, list):
        return " ".join(_contents_text(item) for item in value)
    return str(value or "")


def _rename_collision(case):
    if case["name"] in ("go", "c", "cpp"):
        return "UseTarget"
    if case["name"] in ("rust", "python"):
        return "use_target"
    if case["name"] in ("typescript", "javascript"):
        return "useTarget"
    return "useComponent"


def _expected_rename_refusal(case, error):
    if not isinstance(error, dict) or error.get("code") != -32803:
        return False
    message = str(error.get("message", "")).lower()
    family = case["family"]
    if family == "go":
        return ("rename refused: symbol is exported; importer packages are not loaded" in message
                and "sem-safe-001" in message)
    if family == "cpp":
        return message == "rename refused: c/c++ function/method collision analysis is not proven (sem-safe-001)"
    if family == "typescript":
        return ("rename refused:" in message and "typescript/javascript" in message
                and "sem-safe-001" in message)
    if family == "python":
        return ("rename refused:" in message and "upstream language service returned no edits" in message
                and "sem-safe-001" in message)
    return "upstream language service refused rename" in message


def _profile_registration_state():
    from LSP.plugin.api import get_plugin
    from LSP.plugin.core.settings import client_configs

    module_name = OmniLspAcceptance.__module__
    registration_name = module_name.split(".")[0]
    plugin = get_plugin(registration_name)
    config = client_configs.all.get(registration_name)
    return {
        "registerCalledFromPluginLoaded": _registration_requested,
        "registrationCallError": _registration_error,
        "module": module_name,
        "registrationName": registration_name,
        "settingsResource": "Packages/" + registration_name + "/" + registration_name + ".sublime-settings",
        "registered": plugin is OmniLspAcceptance,
        "registeredClass": (plugin.__module__ + "." + plugin.__name__) if plugin else None,
        "configPresent": config is not None,
        "enabled": bool(getattr(config, "enabled", False)) if config else False,
        "selector": str(getattr(config, "selector", "")) if config else "",
        "schemes": list(getattr(config, "schemes", []) or []) if config else [],
        "commandConfigured": bool(getattr(config, "command", [])) if config else False,
    }


class _AcceptanceRun:
    def __init__(self):
        self.result_path = os.environ.get("OMNILSP_CLIENT_RESULT", "")
        self.manifest = os.environ.get("OMNILSP_CLIENT_CASES", "")
        self.workspace = os.path.dirname(os.path.abspath(self.manifest)) if self.manifest else ""
        self.tool_status = json.loads(os.environ.get("OMNILSP_CLIENT_TOOL_STATUS", "{}"))
        self.case_only = os.environ.get("OMNILSP_CLIENT_CASE_ONLY", "").strip()
        self.rows = []
        self.cases = []
        self.index = 0
        self.case = None
        self.view = None
        self.case_path = ""
        self.uri = ""
        self.symbol = ""
        self.positions = []
        self.source_before = ""
        self.disk_before = b""
        self.observed = {}
        self.started = 0.0
        self.rust_hover_deadline = 0.0
        self.rust_hover_attempts = 0
        self.session_name = ""
        self.notification_mark = 0
        self.rename_mark = 0
        self.completion_consumption_mark = 0
        self.completion_response_item = None
        self.completion_response_index = -1
        self.completion_capture_token = ""
        self.completion_consumption_deadline = 0.0
        self.case_epoch = 0
        self.hover_popup_mark = 0
        self.hover_capture_token = ""
        self.hover_consumer_deadline = 0.0
        self.did_change_mark = 0
        self.did_change_previous_version = None
        self.did_change_marker = ""
        self.did_change_deadline = 0.0
        self.definition_source_view_id = None
        self.definition_expected = None
        self.definition_consumer_deadline = 0.0
        self.reference_panel_lines = []
        self.references_consumer_deadline = 0.0
        self.finalizing = False
        self.result = {
            "client": "sublime-lsp",
            "status": "running",
            "cleanExit": False,
            "clientTestsCompleted": False,
            "serverExitConfirmed": False,
            "cases": self.rows,
        }
        self.result.update(_native_case_evidence_identity())

    def write(self):
        if not self.result_path:
            return
        os.makedirs(os.path.dirname(self.result_path), exist_ok=True)
        temporary = self.result_path + ".tmp"
        with open(temporary, "w", encoding="utf-8", newline="\n") as output:
            json.dump(self.result, output, indent=2, ensure_ascii=False)
            output.write("\n")
        os.replace(temporary, self.result_path)

    def start(self):
        try:
            if not self.result_path or not self.manifest or not os.path.isfile(self.manifest):
                raise RuntimeError("OMNILSP_CLIENT_RESULT and generated OMNILSP_CLIENT_CASES are required")
            with open(self.manifest, "r", encoding="utf-8") as source:
                self.cases = json.load(source)
            if self.case_only and not any(case.get("name") == self.case_only for case in self.cases):
                raise RuntimeError("OMNILSP_CLIENT_CASE_ONLY does not match the manifest")
            self.write()
            self._wait_for_profile_registration(time.monotonic() + 5)
        except Exception as exc:
            self._fatal(exc)

    def _wait_for_profile_registration(self, deadline):
        try:
            state = _profile_registration_state()
            self.result["pluginRegistration"] = state
            if state["registered"] and state["configPresent"]:
                if not state["enabled"]:
                    raise RuntimeError("Sublime LSP acceptance configuration is registered but disabled: " + json.dumps(state, sort_keys=True))
                if not state["selector"] or not state["schemes"] or not state["commandConfigured"]:
                    raise RuntimeError("Sublime LSP acceptance configuration is missing selector, schemes, or command: " + json.dumps(state, sort_keys=True))
                self.write()
                self._next_case()
                return
            if time.monotonic() >= deadline:
                raise RuntimeError("Sublime LSP acceptance plugin/configuration was not registered within 5 seconds: " + json.dumps(state, sort_keys=True))
            self.write()
            sublime.set_timeout(lambda: self._wait_for_profile_registration(deadline), 100)
        except Exception as exc:
            self._fatal(exc)

    def _next_case(self):
        self.case = None
        while self.index < len(self.cases):
            case = self.cases[self.index]
            self.index += 1
            if self.case_only and case.get("name") != self.case_only:
                self.rows.append({
                    "name": case.get("name"), "languageId": case.get("languageId"),
                    "family": case.get("family"), "status": "not_verified",
                    "reason": "not executed because OMNILSP_CLIENT_CASE_ONLY=" + self.case_only,
                })
                continue
            missing = [tool for tool in case.get("requiredTools", []) if self.tool_status.get(tool) is not True]
            if missing:
                self.rows.append({
                    "name": case.get("name"), "languageId": case.get("languageId"),
                    "family": case.get("family"), "status": "not_verified",
                    "reason": "locked prerequisites unavailable: " + ", ".join(missing),
                })
                continue
            self.case = case
            break
        if self.case is None:
            self._finish()
            return
        self.case_epoch += 1
        self._open_case()

    def _open_case(self):
        try:
            window = sublime.active_window()
            path = os.path.abspath(os.path.join(self.workspace, self.case["file"]))
            if not window or not os.path.isfile(path):
                raise RuntimeError("Sublime did not open the generated fixture workspace")
            self.view = window.open_file(path)
            if not self.view:
                raise RuntimeError("Sublime did not return a view for the generated fixture")
            self.case_path = path
            self._wait_for_view_load(self.view, time.monotonic() + 30)
        except Exception as exc:
            self._case_failed(exc)

    def _wait_for_view_load(self, view, deadline):
        if self.view is not view:
            return
        try:
            if not view.is_valid():
                raise RuntimeError("Sublime closed the generated fixture before it finished loading")
            if view.is_loading():
                if time.monotonic() >= deadline:
                    raise RuntimeError("Sublime fixture buffer did not finish loading within 30 seconds")
                sublime.set_timeout(lambda: self._wait_for_view_load(view, deadline), 50)
                return
            # set_timeout runs on Sublime's main thread; view syntax assignment
            # and applicability checks must use that UI-thread lifecycle.
            sublime.set_timeout(lambda: self._prepare_loaded_case(view), 0)
        except Exception as exc:
            self._case_failed(exc)

    def _prepare_loaded_case(self, view):
        if self.view is not view:
            return
        try:
            self.observed = {}
            self._assign_case_syntax(view)
            self._wait_for_case_syntax(view, time.monotonic() + 5)
        except Exception as exc:
            self._case_failed(exc)

    def _wait_for_case_syntax(self, view, deadline):
        if self.view is not view:
            return
        try:
            if not view.is_valid():
                raise RuntimeError("Sublime closed the generated fixture before applying its syntax")
            state = self.observed["syntaxRegistration"]
            active_syntax = view.syntax()
            state["pollCount"] = state.get("pollCount", 0) + 1
            state["assignedPath"] = str(getattr(active_syntax, "path", "")) if active_syntax else ""
            state["assignedScope"] = str(getattr(active_syntax, "scope", "")) if active_syntax else ""
            if (state["assignedPath"] == state["requestedPath"]
                    and state["assignedScope"] == state["expectedScope"]):
                state["assigned"] = True
                self.observed["syntax"] = view.settings().get("syntax")
                self._continue_loaded_case(view)
                return
            if time.monotonic() >= deadline:
                state["syntaxDiagnostics"] = self._capture_syntax_diagnostics(view.window(), state)
                raise RuntimeError("Sublime did not apply the acceptance syntax within 5 seconds: "
                                   + json.dumps(state, sort_keys=True))
            sublime.set_timeout(lambda: self._wait_for_case_syntax(view, deadline), 50)
        except Exception as exc:
            self._case_failed(exc)

    def _capture_syntax_diagnostics(self, window, state):
        if not window:
            return {"source": "Sublime console", "available": False, "reason": "fixture window unavailable"}
        try:
            console = window.find_output_panel("console")
            if console is None:
                window.run_command("show_panel", {"panel": "console"})
                console = window.find_output_panel("console")
            if console is None:
                return {"source": "Sublime console", "available": False, "reason": "console panel unavailable"}
            end = console.size()
            start = max(0, end - 8192)
            output = console.substr(sublime.Region(start, end))
            syntax_path = state["requestedPath"].lower()
            relevant = []
            for line in str(output).splitlines():
                lowered = line.lower()
                if syntax_path in lowered or ("error" in lowered and "lexer" in lowered):
                    relevant.append(line[:512])
            return {
                "source": "Sublime console",
                "available": True,
                "relevantOutput": "\n".join(relevant[-8:])[-4096:],
            }
        except Exception as exc:
            return {"source": "Sublime console", "available": False,
                    "error": (type(exc).__name__ + ": " + str(exc))[:512]}

    def _continue_loaded_case(self, view):
        self.uri = Path(self.case_path).as_uri()
        self.symbol = self.case["symbol"]
        registration = self.result.get("pluginRegistration", {})
        registration_name = registration.get("registrationName", "omnilsp_acceptance")
        self.session_name = registration_name
        self.observed["activation_recheck"] = {
            "command": "lsp_check_applicable",
            "session_name": registration_name,
            "invoked": True,
            "afterSyntaxApplied": True,
        }
        view.run_command("lsp_check_applicable", {"session_name": registration_name})
        text = view.substr(sublime.Region(0, view.size()))
        matches = list(re.finditer(r"\b" + re.escape(self.symbol) + r"\b", text))
        if len(matches) < 2:
            raise RuntimeError("fixture does not contain symbol declaration and use")
        self.positions = [match.start() for match in matches[:2]]
        self.disk_before = Path(self.case_path).read_bytes()
        self.started = time.monotonic()
        self.rust_hover_attempts = 0
        self.rust_hover_deadline = 0.0
        if self.case["languageId"] == "rust":
            self.rust_hover_deadline = self.started + 60
        self._set_cursor(self.positions[0])
        self.observed["profile_config"] = "omnilsp_acceptance.sublime-settings"
        self._wait_for_session_ready(view, time.monotonic() + 45)

    def _assign_case_syntax(self, view):
        syntax_file, expected_scope = _syntax_by_case[self.case["name"]]
        syntax_path = "Packages/omnilsp_acceptance/" + syntax_file
        state = {
            "requestedPath": syntax_path,
            "expectedScope": expected_scope,
            "loaded": False,
            "assignmentRequested": False,
            "assigned": False,
        }
        self.observed["syntaxRegistration"] = state
        syntax = sublime.syntax_from_path(syntax_path)
        if syntax is None:
            raise RuntimeError("Sublime did not load the acceptance syntax resource: " + json.dumps(state, sort_keys=True))
        state["loaded"] = True
        state["loadedPath"] = str(getattr(syntax, "path", ""))
        state["loadedScope"] = str(getattr(syntax, "scope", ""))
        if state["loadedPath"] != syntax_path or state["loadedScope"] != expected_scope:
            raise RuntimeError("Sublime loaded an unexpected acceptance syntax: " + json.dumps(state, sort_keys=True))

        view.assign_syntax(syntax)
        state["assignmentRequested"] = True
        state["settingsPathAfterRequest"] = str(view.settings().get("syntax") or "")

    def _session_startup_state(self, view):
        from LSP.plugin.core.registry import windows
        from LSP.plugin.core.settings import client_configs

        with _lock:
            plugins = list(_session_plugins)
            opened_notifications = list(_client_notifications)
        matching = []
        window = view.window()
        for plugin in plugins:
            session = plugin.weaksession()
            if not session:
                continue
            session_window = getattr(session, "window", None)
            same_window = bool(session_window and _same_window(session_window, window))
            state = getattr(session, "state", None)
            matching.append({
                "config": getattr(getattr(session, "config", None), "name", None),
                "state": getattr(state, "name", str(state)),
                "pluginActivated": True,
                "initialized": bool(getattr(plugin, "_acceptance_initialized", False)),
                "ended": getattr(plugin, "_acceptance_ended", None),
                "windowMatchesFixture": same_window,
            })
        matching_sessions = [item for item in matching if item["windowMatchesFixture"]]
        view_active = view.settings().get("lsp_active") is True
        registration_name = self.result.get("pluginRegistration", {}).get(
            "registrationName", "omnilsp_acceptance")
        config = client_configs.all.get(registration_name)
        syntax = view.syntax()
        syntax_scope = getattr(syntax, "scope", "") if syntax else ""
        selector = str(getattr(config, "selector", "")) if config else ""
        try:
            selector_score = sublime.score_selector(syntax_scope, selector) if selector else 0
        except Exception:
            selector_score = 0
        scheme = urlparse(self.uri).scheme
        config_matches = bool(config and config.enabled and scheme in config.schemes and selector_score > 0)
        manager = windows.lookup(window)
        view_listener_registered = bool(manager and manager.listener_for_view(view))
        did_open = any(
            event.get("method") == "textDocument/didOpen" and _same_uri(event.get("uri"), self.uri)
            for event in opened_notifications
        )
        initialized = any(item["initialized"] for item in matching_sessions)
        return {
            "pluginInstancesTotal": len(matching),
            "pluginInstancesInWindow": len(matching_sessions),
            "sessions": matching,
            "configEnabled": bool(config and config.enabled),
            "selector": selector,
            "syntaxScope": syntax_scope,
            "selectorScore": selector_score,
            "scheme": scheme,
            "configMatchesFixture": config_matches,
            "lspWindowManagerAvailable": manager is not None,
            "viewListenerRegistered": view_listener_registered,
            "serverInitialized": initialized,
            "viewAttached": view_active,
            "documentDidOpenHookObserved": did_open,
            "ready": config_matches and manager is not None and view_listener_registered
            and initialized and view_active and did_open,
        }

    def _wait_for_session_ready(self, view, deadline):
        if self.view is not view:
            return
        try:
            if not view.is_valid():
                raise RuntimeError("Sublime closed the generated fixture before its LSP session became ready")
            state = self._session_startup_state(view)
            self.observed["session_startup"] = state
            if not state["configMatchesFixture"]:
                raise RuntimeError("registered Sublime LSP configuration does not match the fixture view: " + json.dumps(state, sort_keys=True))
            if state["ready"]:
                self.started = time.monotonic()
                self.rust_hover_attempts = 0
                self.rust_hover_deadline = self.started + 60 if self.case["languageId"] == "rust" else 0.0
                sublime.set_timeout(self._start_hover_request, 0)
                return
            if any(item.get("ended") for item in state["sessions"] if item["windowMatchesFixture"]):
                raise RuntimeError("Sublime LSP session ended before startup completed: " + json.dumps(state, sort_keys=True))
            if time.monotonic() >= deadline:
                raise RuntimeError("Sublime LSP did not reach initialized, attached, didOpen state within 45 seconds: " + json.dumps(state, sort_keys=True))
            sublime.set_timeout(lambda: self._wait_for_session_ready(view, deadline), 100)
        except Exception as exc:
            self._case_failed(exc)

    def _start_hover_request(self):
        global _active_hover_capture, _hover_capture_sequence
        try:
            _install_hover_popup_consumption_hook()
            self.view.hide_popup()
            with _lock:
                _hover_capture_sequence += 1
                self.hover_popup_mark = len(_hover_popup_events)
                self.hover_capture_token = str(self.case_epoch) + ":" + str(_hover_capture_sequence)
                _active_hover_capture = {
                    "viewId": self.view.id(),
                    "caseEpoch": self.case_epoch,
                    "captureToken": self.hover_capture_token,
                    "candidate": self.symbol,
                }
            self._command_response(
                "lsp_hover", "textDocument/hover", {}, self._on_hover,
                timeout=25 if self.case["languageId"] == "rust" else 90)
        except Exception as exc:
            self._case_failed(exc)

    def _set_cursor(self, point):
        selection = self.view.sel()
        selection.clear()
        selection.add(sublime.Region(point))

    def _command_response(self, command, method, args, callback, timeout=25):
        with _lock:
            response_mark = len(_responses)
            request_mark = len(_client_requests)
        history = self.observed.setdefault("lsp_commands", [])
        operation = {
            "command": command,
            "method": method,
            "commandInvocationAttempted": True,
            "commandInvocationReturned": False,
            "requestPreSendHookObserved": False,
            "successfulResponseHookObserved": False,
        }
        history.append(operation)
        if len(history) > 16:
            del history[:-16]
        self.view.run_command(command, args)
        operation["commandInvocationReturned"] = True
        self._wait_response(method, response_mark, request_mark, operation, callback,
                            time.monotonic() + timeout, time.monotonic() + 5)

    def _wait_response(self, method, response_mark, request_mark, operation, callback, deadline, dispatch_deadline):
        with _lock:
            responses = list(_responses[response_mark:])
            requests = list(_client_requests[request_mark:])
        response = next((event for event in responses if event.get("method") == method), None)
        request = next((event for event in requests if event.get("method") == method
                        and (not event.get("view_path") or os.path.normcase(event["view_path"]) == os.path.normcase(self.case_path))), None)
        operation["requestPreSendHookObserved"] = request is not None
        operation["successfulResponseHookObserved"] = response is not None
        if response:
            try:
                callback(response)
            except Exception as exc:
                self._case_failed(exc)
            return
        if not request and time.monotonic() >= dispatch_deadline:
            self._case_failed(RuntimeError(
                "Sublime command " + operation["command"] + " was invoked for " + method
                + " but LSP.plugin.on_pre_send_request_async did not observe that request; "
                + "command invocation and session readiness were observed separately"
            ))
            return
        if time.monotonic() >= deadline:
            self._case_failed(RuntimeError(
                "Sublime command " + operation["command"] + " invoked " + method
                + (" and the LSP pre-send request hook observed it" if request else " without an LSP pre-send request hook")
                + ", but no successful LspPlugin.on_server_response_async response was observed"
            ))
            return
        sublime.set_timeout(lambda: self._wait_response(
            method, response_mark, request_mark, operation, callback, deadline, dispatch_deadline), 100)

    def _on_hover(self, response):
        result = _response_result(response)
        text = _contents_text(result)
        self.observed["hoverResponse"] = text[:4096]
        if self.symbol not in text and self.case["languageId"] == "rust":
            if time.monotonic() < self.rust_hover_deadline:
                self.rust_hover_attempts += 1
                self.observed["rust_readiness_probe"] = {
                    "source": "Sublime LSP hover result",
                    "attempts": self.rust_hover_attempts,
                    "deadline_seconds": 60,
                    "last_result": text,
                }
                delay = min(500, 50 * self.rust_hover_attempts)
                sublime.set_timeout(self._retry_rust_hover, delay)
                return
        if self.symbol not in text:
            raise RuntimeError("Sublime hover result did not describe " + self.symbol)
        self.hover_consumer_deadline = time.monotonic() + _NATIVE_CONSUMER_TIMEOUT_SECONDS
        if self.rust_hover_attempts:
            self.observed["rust_readiness_probe"]["positive_result"] = True
        self._wait_hover_consumed()

    def _wait_hover_consumed(self):
        with _lock:
            consumed = next((event for event in _hover_popup_events[self.hover_popup_mark:]
                             if _hover_popup_consumption_matches(
                                 event, self.view.id(), self.case_epoch, self.hover_capture_token, self.symbol)), None)
        if consumed:
            global _active_hover_capture
            with _lock:
                if (_active_hover_capture
                        and _active_hover_capture.get("captureToken") == self.hover_capture_token):
                    _active_hover_capture = None
            self.observed["hover"] = self.observed.get("hoverResponse", "")
            self.observed["hoverConsumer"] = {
                "api": consumed["api"],
                "viewId": consumed["viewId"],
                "caseEpoch": consumed["caseEpoch"],
                "popupVisible": consumed["popupVisible"],
                "contentMatchesCandidate": consumed["contentMatchesCandidate"],
                "renderedContent": consumed["content"][:4096],
            }
            self.observed["client_api"] = "LSP.plugin.hover.show_lsp_popup returned with the native popup visible"
            self._sync_edit()
            return
        if time.monotonic() >= self.hover_consumer_deadline:
            with _lock:
                attempts = list(_hover_popup_events[self.hover_popup_mark:])
            summary = {
                "rendererHookInstalled": True,
                "rendererCalls": len(attempts),
                "captureViewCalls": sum(event.get("capturedViewId") == self.view.id() for event in attempts),
                "matchingViewCalls": sum(event.get("viewId") == self.view.id()
                                          and event.get("capturedViewId") == self.view.id() for event in attempts),
                "candidateContentCalls": sum(event.get("contentMatchesCandidate") is True for event in attempts),
                "visibleCalls": sum(event.get("popupVisible") is True
                                    and event.get("viewId") == self.view.id()
                                    and event.get("capturedViewId") == self.view.id() for event in attempts),
            }
            self.observed["hoverConsumerObservation"] = summary
            self._case_failed(RuntimeError(
                "Sublime LSP returned the hover result but did not complete a matching visible native popup render "
                "within " + str(_NATIVE_CONSUMER_TIMEOUT_SECONDS) + " seconds: "
                + json.dumps(summary, sort_keys=True)
            ))
            return
        sublime.set_timeout(self._wait_hover_consumed, 50)

    def _retry_rust_hover(self):
        self._start_hover_request()

    def _sync_edit(self):
        comment = "\n# sublime-client-sync-probe\n" if self.case["languageId"] == "python" else "\n// sublime-client-sync-probe\n"
        with _lock:
            self.did_change_mark = len(_client_notifications)
            previous_versions = [event.get("version") for event in _client_notifications
                                 if event.get("method") == "textDocument/didChange"
                                 and _same_uri(event.get("uri"), self.uri)
                                 and type(event.get("version")) is int]
            self.did_change_previous_version = max(previous_versions) if previous_versions else None
        self.did_change_marker = comment.strip()
        self.did_change_deadline = time.monotonic() + _NATIVE_CONSUMER_TIMEOUT_SECONDS
        self.view.sel().clear()
        self.view.sel().add(sublime.Region(self.view.size()))
        self.view.run_command("insert", {"characters": comment})
        self.observed["buffer_edit"] = "Sublime View insert command"
        self._set_cursor(self.positions[1] + min(5, len(self.symbol)))
        self._wait_document_change_sent()

    def _wait_document_change_sent(self):
        with _lock:
            event = next((item for item in _client_notifications[self.did_change_mark:]
                          if _did_change_matches(item, self.uri, self.did_change_previous_version,
                                                 self.did_change_marker)), None)
        if event:
            self.observed["buffer_edit_sync"] = {
                "notification": "textDocument/didChange",
                "uri": event.get("uri"),
                "version": event.get("version"),
                "marker": self.did_change_marker,
            }
            self._start_completion_request()
            return
        if time.monotonic() >= self.did_change_deadline:
            self._case_failed(RuntimeError(
                "Sublime applied the fixture View insert but no matching didChange notification with an advanced "
                "document version and inserted marker reached LSP.plugin.on_pre_send_notification_async within "
                + str(_NATIVE_CONSUMER_TIMEOUT_SECONDS) + " seconds"
            ))
            return
        sublime.set_timeout(self._wait_document_change_sent, 50)

    def _start_completion_request(self):
        _install_completion_consumption_hook()
        global _active_completion_capture, _completion_capture_sequence
        with _lock:
            _completion_capture_sequence += 1
            self.completion_consumption_mark = len(_completion_consumptions)
            self.completion_capture_token = str(self.case_epoch) + ":" + str(_completion_capture_sequence)
            _active_completion_capture = {
                "candidate": self.symbol,
                "sessionName": self.session_name,
                "viewId": self.view.id(),
                "caseEpoch": self.case_epoch,
                "captureToken": self.completion_capture_token,
            }
        self._command_response("auto_complete", "textDocument/completion", {"api_completions_only": True}, self._on_completion)

    def _on_completion(self, response):
        result = _response_result(response)
        items = result.get("items", []) if isinstance(result, dict) else result
        if not isinstance(items, list) or any(not isinstance(item, dict) or not isinstance(item.get("label"), str)
                                               for item in items):
            raise RuntimeError("Sublime auto_complete returned a malformed completion item list")
        sorted_items = sorted(items, key=lambda item: item.get("sortText") or item["label"])
        matches = [(index, item) for index, item in enumerate(sorted_items)
                   if item["label"] == self.symbol]
        if len(matches) != 1:
            raise RuntimeError("Sublime auto_complete did not return exactly one item labeled " + self.symbol)
        self.completion_response_index, response_item = matches[0]
        if not self.session_name:
            raise RuntimeError("Sublime completion response cannot be bound to a configured session name")
        self.completion_response_item = response_item
        self.completion_consumption_deadline = time.monotonic() + _COMPLETION_CONSUMER_TIMEOUT_SECONDS
        self._wait_completion_consumed()

    def _wait_completion_consumed(self):
        with _lock:
            consumed = next((event for event in _completion_consumptions[self.completion_consumption_mark:]
                             if _completion_consumption_matches(
                                 event, self.symbol, self.completion_response_index, self.session_name,
                                 self.view.id(), self.case_epoch, self.completion_capture_token)), None)
        popup_visible = bool(getattr(self.view, "is_auto_complete_visible", lambda: False)())
        if consumed and popup_visible:
            global _active_completion_capture
            with _lock:
                if (_active_completion_capture
                        and _active_completion_capture.get("captureToken") == self.completion_capture_token):
                    _active_completion_capture = None
            self.observed["completion"] = {
                "candidate": self.symbol,
                "responseItem": _plain(self.completion_response_item),
                "clientConsumptionApi": "sublime.CompletionList.set_completions",
                "completionListTargetBound": True,
                "responseIndex": self.completion_response_index,
                "sessionName": self.session_name,
                "viewId": self.view.id(),
                "caseEpoch": self.case_epoch,
                "captureToken": self.completion_capture_token,
                "selection": consumed["selection"],
                "deliveredItem": consumed["item"],
                "completionPopupVisible": popup_visible,
            }
            self._set_cursor(self.positions[1])
            self._command_response("lsp_symbol_definition", "textDocument/definition", {}, self._on_definition)
            return
        if time.monotonic() >= self.completion_consumption_deadline:
            message = ("Sublime LSP returned the completion candidate but did not deliver a matching item through "
                       "the same-view CompletionList.set_completions consumer within "
                       + str(_COMPLETION_CONSUMER_TIMEOUT_SECONDS) + " seconds")
            if consumed:
                message = ("Sublime's bound completion consumer received the matching candidate, but its native "
                           "autocomplete popup was not visible within "
                           + str(_COMPLETION_CONSUMER_TIMEOUT_SECONDS) + " seconds")
            self._case_failed(RuntimeError(message))
            return
        sublime.set_timeout(self._wait_completion_consumed, 50)

    def _on_definition(self, response):
        result = _response_result(response)
        results = result if isinstance(result, list) else [result]
        expected_line = self.view.substr(sublime.Region(0, self.positions[0])).count("\n")
        matching = [item for item in results if (loc := _location_range(item))
                    and _same_uri(loc["uri"], self.uri) and loc["start"]["line"] == expected_line]
        if len(matching) != 1:
            raise RuntimeError("Sublime definition response did not target the fixture declaration")
        self.definition_expected = _location_range(matching[0])
        self.definition_source_view_id = self.view.id()
        self.definition_consumer_deadline = time.monotonic() + _NATIVE_CONSUMER_TIMEOUT_SECONDS
        self.observed["definitionResponse"] = {"line": expected_line, "count": len(results)}
        self._wait_definition_consumed()

    def _wait_definition_consumed(self):
        window = self.view.window()
        landing = _definition_landing_matches(window, self.definition_expected, self.definition_source_view_id)
        if landing:
            self.observed["definition"] = {
                "line": self.definition_expected["start"]["line"],
                "responseCount": self.observed["definitionResponse"]["count"],
                "clientConsumerApi": "LSP.plugin.locationpicker.open_location_async via Window.active_view",
                "landing": landing,
                "targetRange": self.definition_expected,
            }
            self.view = window.active_view()
            self._set_cursor(self.positions[1])
            self._command_response("lsp_symbol_references", "textDocument/references",
                                   {"include_declaration": True, "output_mode": "output_panel"}, self._on_references)
            return
        if time.monotonic() >= self.definition_consumer_deadline:
            self._case_failed(RuntimeError(
                "Sublime LSP returned the fixture declaration for definition but native open_location did not move "
                "the active fixture view and caret into its target range within "
                + str(_NATIVE_CONSUMER_TIMEOUT_SECONDS) + " seconds"
            ))
            return
        sublime.set_timeout(self._wait_definition_consumed, 50)

    def _on_references(self, response):
        result = _response_result(response)
        locations = result if isinstance(result, list) else []
        lines = [self.view.substr(sublime.Region(0, point)).count("\n") for point in self.positions]
        matching = [loc for loc in (_location_line(item) for item in locations) if loc and _same_uri(loc[0], self.uri)]
        if len(matching) < 2 or not all(any(line == found[1] for found in matching) for line in lines):
            raise RuntimeError("Sublime references response omitted the fixture declaration or use")
        self.reference_panel_lines = [self.view.substr(self.view.line(point)).strip() for point in self.positions]
        self.references_consumer_deadline = time.monotonic() + _NATIVE_CONSUMER_TIMEOUT_SECONDS
        self.observed["referencesResponse"] = {"count": len(locations), "lines": lines}
        self._wait_references_consumed()

    def _wait_references_consumed(self):
        window = self.view.window()
        consumer = _references_panel_consumption(window, self.symbol, self.reference_panel_lines)
        if consumer:
            self.observed["references"] = {
                "count": self.observed["referencesResponse"]["count"],
                "lines": self.observed["referencesResponse"]["lines"],
                "clientConsumerApi": "LSP references output.references panel",
                "panel": consumer["panel"],
                "renderedFixtureLines": consumer["lineCount"],
                "renderedContent": consumer["content"],
            }
            self._set_cursor(self.positions[0])
            self.source_before = self.view.substr(sublime.Region(0, self.view.size()))
            self._request_rename()
            return
        if time.monotonic() >= self.references_consumer_deadline:
            self._case_failed(RuntimeError(
                "Sublime LSP returned the fixture references but the visible output.references panel did not contain "
                "both fixture lines within " + str(_NATIVE_CONSUMER_TIMEOUT_SECONDS) + " seconds"
            ))
            return
        sublime.set_timeout(self._wait_references_consumed, 50)

    def _request_rename(self):
        with _lock:
            self.rename_mark = len(_rename_results)
            plugins = list(_session_plugins)
        matching_sessions = []
        for plugin in plugins:
            session = plugin.weaksession()
            if session and session.window == self.view.window():
                matching_sessions.append(session)
        if len(matching_sessions) != 1:
            raise RuntimeError("could not identify the single Sublime LSP session for rename")
        params = {
            "textDocument": {"uri": self.uri},
            "position": self._lsp_position(self.positions[0]),
            "newName": _rename_collision(self.case),
        }
        session = matching_sessions[0]

        def on_result(result):
            _record(_rename_results, {"result": result})

        def on_error(error):
            _record(_rename_results, {"error": error})

        # Exercise Sublime's native rename command. A second identical request
        # through the locked LSP Session captures the typed refusal because the
        # LspPlugin response hook only receives successful responses.
        self.view.run_command("lsp_symbol_rename", {"new_name": _rename_collision(self.case), "point": self.positions[0]})
        session.send_request(Request.rename(params, self.view), on_result, on_error)
        self._wait_rename(self.rename_mark, time.monotonic() + 25)

    def _lsp_position(self, point):
        prefix = self.view.substr(sublime.Region(0, point))
        line = prefix.count("\n")
        character = len(prefix.rsplit("\n", 1)[-1])
        return {"line": line, "character": character}

    def _wait_rename(self, mark, deadline):
        with _lock:
            events = list(_rename_results[mark:])
        if events:
            try:
                self._on_rename(events[0])
            except Exception as exc:
                self._case_failed(exc)
            return
        if time.monotonic() >= deadline:
            self._case_failed(RuntimeError("Sublime LSP Session did not complete textDocument/rename"))
            return
        sublime.set_timeout(lambda: self._wait_rename(mark, deadline), 100)

    def _on_rename(self, response):
        error = response.get("error")
        if not _expected_rename_refusal(self.case, error):
            raise RuntimeError("Sublime unsafe rename did not return the expected typed refusal: " + json.dumps(error))
        if self.view.substr(sublime.Region(0, self.view.size())) != self.source_before:
            raise RuntimeError("Sublime rename refusal changed the editor buffer")
        path = os.path.abspath(os.path.join(self.workspace, self.case["file"]))
        if Path(path).read_bytes() != self.disk_before:
            raise RuntimeError("Sublime rename refusal changed the fixture on disk")
        self.observed["rename_refusal"] = error
        self._check_diagnostics()

    def _diagnostics_for_view(self, after=0):
        with _lock:
            events = list(_notifications[after:])
        return [event.get("params", {}) for event in events
                if event.get("method") == "textDocument/publishDiagnostics"
                and _same_uri(event.get("params", {}).get("uri"), self.uri)]

    def _wait_diagnostics(self, mark, callback, deadline):
        updates = self._diagnostics_for_view(mark)
        if updates:
            try:
                callback(updates[-1])
            except Exception as exc:
                self._case_failed(exc)
            return
        if time.monotonic() >= deadline:
            self._case_failed(RuntimeError("Sublime did not receive textDocument/publishDiagnostics"))
            return
        sublime.set_timeout(lambda: self._wait_diagnostics(mark, callback, deadline), 100)

    def _check_diagnostics(self):
        language = self.case["languageId"]
        if language in ("c", "cpp"):
            updates = self._diagnostics_for_view(0)
            if updates:
                self._on_clean_diagnostics(updates[-1])
            else:
                with _lock:
                    mark = len(_notifications)
                self._wait_diagnostics(mark, self._on_clean_diagnostics, time.monotonic() + 20)
            return
        probe = "omnilspMissingSymbol"
        statement = {
            "go": "var _ = " + probe,
            "rust": "fn client_diagnostic_probe() { let _ = " + probe + "; }",
            "python": "client_diagnostic_probe = " + probe,
            "typescript": "export const clientDiagnosticProbe: number = " + probe + ";",
        }.get(self.case["family"])
        if language in ("typescriptreact",):
            statement = "export const clientDiagnosticProbe: number = " + probe + ";"
        elif language == "javascriptreact":
            statement = "export const clientDiagnosticProbe = " + probe + ";"
        elif language == "javascript":
            statement = "export const clientDiagnosticProbe = " + probe + ";"
        if not statement:
            raise RuntimeError("no diagnostic probe for " + language)
        with _lock:
            mark = len(_notifications)
        self.view.sel().clear()
        self.view.sel().add(sublime.Region(self.view.size()))
        self.view.run_command("insert", {"characters": "\n" + statement + "\n"})
        if language == "rust":
            self.view.run_command("save")
        self._wait_diagnostics(mark, lambda params: self._on_semantic_diagnostics(params, probe), time.monotonic() + 60)

    def _on_clean_diagnostics(self, params):
        diagnostics = params.get("diagnostics", [])
        if diagnostics:
            raise RuntimeError("clean C/C++ source produced diagnostics: " + json.dumps(diagnostics))
        self.observed["diagnostic_scope"] = "clean_source_no_false_positive"
        self.observed["deferred_capability"] = "DEF-CCLSDIAG"
        self._pass_case()

    def _on_semantic_diagnostics(self, params, probe):
        diagnostics = params.get("diagnostics", [])
        expected_source = {"go": "omnilsp-go", "rust": "rustc", "python": "Pyright", "typescript": "typescript"}[self.case["family"]]
        matches = [item for item in diagnostics if probe in str(item.get("message", "")) and item.get("source") == expected_source]
        if not matches:
            raise RuntimeError("Sublime diagnostic notification omitted the unresolved fixture symbol")
        diagnostic = matches[0]
        self.observed["diagnostic_scope"] = "semantic_unresolved_name"
        self.observed["diagnostic_count"] = len(diagnostics)
        self.observed["diagnostic_message"] = diagnostic.get("message", "")
        self._pass_case(diagnostic)

    def _pass_case(self, diagnostic=None):
        row = {
            "name": self.case["name"], "languageId": self.case["languageId"],
            "family": self.case["family"], "status": "passed", "observed": self.observed,
        }
        if diagnostic:
            row["diagnostic"] = {
                "diagnostic_scope": "semantic_unresolved_name", "severity": diagnostic.get("severity"),
                "source": diagnostic.get("source"), "code": diagnostic.get("code"),
                "message": diagnostic.get("message"), "range": diagnostic.get("range"),
            }
        self.rows.append(row)
        self.write()
        self._close_case()

    def _case_failed(self, error):
        if not self.case:
            self._fatal(error)
            return
        global _active_completion_capture, _active_hover_capture
        with _lock:
            if (_active_completion_capture
                    and _active_completion_capture.get("captureToken") == self.completion_capture_token):
                _active_completion_capture = None
            if (_active_hover_capture
                    and _active_hover_capture.get("captureToken") == self.hover_capture_token):
                _active_hover_capture = None
            stale_lists = [key for key, pending in _pending_completion_lists.items()
                           if pending.get("caseEpoch") == self.case_epoch]
            for key in stale_lists:
                del _pending_completion_lists[key]
        self.rows.append({
            "name": self.case.get("name"), "languageId": self.case.get("languageId"),
            "family": self.case.get("family"), "status": "failed", "error": str(error),
            "observed": self.observed,
        })
        self.write()
        self._close_case()

    def _close_case(self):
        if self.view and self.view.is_valid():
            self.view.set_scratch(True)
            window = self.view.window()
            if window:
                window.focus_view(self.view)
                window.run_command("close_file")
        self.view = None
        sublime.set_timeout(self._next_case, 150)

    def _finish(self):
        failed = any(row.get("status") == "failed" for row in self.rows)
        unverified = any(row.get("status") == "not_verified" for row in self.rows)
        self.result["status"] = "failed" if failed else "not_verified" if unverified else "passed"
        self.result["clientTestsCompleted"] = not failed
        if failed:
            self.result["error"] = str(sum(row.get("status") == "failed" for row in self.rows)) + " Sublime client case(s) failed"
        with _lock:
            session_plugins = list(_session_plugins)
        initialized_sessions = [plugin for plugin in session_plugins
                                if getattr(plugin, "_acceptance_initialized", False)]
        if not initialized_sessions:
            self.result["serverExitConfirmed"] = False
            ended_before_init = [plugin._acceptance_ended for plugin in session_plugins
                                 if getattr(plugin, "_acceptance_ended", None)]
            if ended_before_init:
                self.result["serverExitEvidence"] = "Sublime LSP session ended before initialization; graceful server shutdown was not observed: " + json.dumps(ended_before_init, sort_keys=True)
            elif session_plugins:
                self.result["serverExitEvidence"] = "no initialized Sublime LSP session was observed; graceful server shutdown was not observed"
            else:
                self.result["serverExitEvidence"] = "no Sublime LSP session was started; graceful server shutdown was not observed"
            self.write()
            sublime.set_timeout(lambda: sublime.run_command("exit"), 250)
            return
        self.result["serverExitConfirmed"] = False
        self.result["serverExitEvidence"] = "waiting for Sublime LSP session shutdown callback"
        self.finalizing = True
        self.write()
        window = sublime.active_window()
        if window:
            window.run_command("close_window")
        sublime.set_timeout(lambda: self._shutdown_timeout(time.monotonic() + 30), 50)

    def server_exited(self, exit_code, exception):
        if not self.finalizing:
            return
        # LSP 2.13 calls this hook with (None, None) for a healthy session and
        # sends the shutdown request immediately after this callback returns.
        # The Go runner separately proves the managed server process tree exits.
        confirmed = exit_code in (None, 0) and exception is None
        self.result["serverExitConfirmed"] = confirmed
        if exit_code is None and confirmed:
            self.result["serverExitEvidence"] = (
                "Sublime LSP session ended without an exception; the outer runner verifies managed process-tree exit"
            )
        elif confirmed:
            self.result["serverExitEvidence"] = (
                "Sublime LSP session transport closed with exit code 0; outer runner verifies managed process-tree exit"
            )
        else:
            self.result["serverExitEvidence"] = (
                "Sublime LSP session exit was not clean: exitCode=" + str(exit_code) + ", exception=" + str(exception)
            )
        if not confirmed and self.result.get("status") == "passed":
            self.result["status"] = "failed"
            self.result["error"] = "Sublime LSP session did not exit cleanly"
        self.write()
        sublime.set_timeout(lambda: sublime.run_command("exit"), 250)

    def _shutdown_timeout(self, deadline):
        if self.result.get("serverExitConfirmed"):
            sublime.set_timeout(lambda: sublime.run_command("exit"), 250)
            return
        if time.monotonic() >= deadline:
            self.result["status"] = "failed"
            self.result["serverExitEvidence"] = "Sublime LSP did not report a completed session shutdown before timeout"
            self.result["error"] = "Sublime LSP session shutdown was not confirmed"
            self.write()
            sublime.set_timeout(lambda: sublime.run_command("exit"), 250)
            return
        sublime.set_timeout(lambda: self._shutdown_timeout(deadline), 100)

    def _fatal(self, error):
        self.result["status"] = "failed"
        self.result["error"] = str(error)
        self.result["clientTestsCompleted"] = False
        self.result["serverExitConfirmed"] = False
        self.write()
        sublime.set_timeout(lambda: sublime.run_command("exit"), 250)


def plugin_loaded():
    global _driver, _registration_requested, _registration_error
    if _LSP_IMPORT_ERROR is not None:
        if os.environ.get("OMNILSP_CLIENT_RESULT"):
            sublime.set_timeout(lambda: sublime.run_command("exit"), 250)
        return
    try:
        OmniLspAcceptance.register()
        _registration_requested = True
    except Exception as exc:
        _registration_error = (type(exc).__name__ + ": " + str(exc))[:1024]
    if os.environ.get("OMNILSP_CLIENT_RESULT"):
        _driver = _AcceptanceRun()
        sublime.set_timeout(_driver.start, 1500)


def plugin_unloaded():
    if _LSP_IMPORT_ERROR is None:
        OmniLspAcceptance.unregister()
