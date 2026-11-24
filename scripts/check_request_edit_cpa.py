"""Exercise request editing in an isolated Linux CPA container.

Requires Docker, a built Linux library, and the selected CPA image. An optional
local plugin state supplies one credential for two small real-upstream probes.
The existing CPA service and its configuration are never modified.
"""

import argparse
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import shutil
import socket
import subprocess
import threading
import time
import urllib.error
import urllib.request
import uuid

from check_plugin import check_plugin


def run(args):
    root = Path(args.work_dir).resolve()
    root.mkdir(mode=0o700, parents=True, exist_ok=False)
    library = Path(args.library).resolve()
    check_plugin(library, args.version)
    auths = root / "auths"
    plugins = auths / "plugins/linux/amd64"
    data = auths / "plugin-data/mistral-bridge"
    plugins.mkdir(parents=True)
    data.mkdir(parents=True, mode=0o700)
    shutil.copy2(library, plugins / "mistral-bridge.so")
    captures = []
    capture_lock = threading.Lock()

    class Upstream(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            headers = {k.lower(): v for k, v in self.headers.items()}
            with capture_lock:
                captures.append({"body": body, "headers": headers})
            usage = {"prompt_tokens": 7, "completion_tokens": 1, "total_tokens": 8}
            if body.get("stream"):
                events = [
                    {"id": "mock", "object": "chat.completion.chunk", "model": "mock-upstream", "choices": [{"index": 0, "delta": {"content": "OK"}}]},
                    {"id": "mock", "object": "chat.completion.chunk", "model": "mock-upstream", "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}], "usage": usage},
                ]
                payload = ("".join("data: " + json.dumps(e) + "\n\n" for e in events) + "data: [DONE]\n\n").encode()
                content_type = "text/event-stream"
            else:
                payload = json.dumps({"id": "mock", "object": "chat.completion", "model": "mock-upstream", "choices": [{"index": 0, "message": {"role": "assistant", "content": "OK"}, "finish_reason": "stop"}], "usage": usage}).encode()
                content_type = "application/json"
            self.send_response(200)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

    mock = ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
    threading.Thread(target=mock.serve_forever, daemon=True).start()
    with socket.socket() as reserved:
        reserved.bind(("127.0.0.1", 0))
        port = reserved.getsockname()[1]
    config = {
        "config-version": 8,
        "server": {"host": "127.0.0.1", "port": port},
        "access": {"api-keys": ["request-edit-test-client"]},
        "management": {"allow-remote": False, "secret-key": "request-edit-test-management", "disable-control-panel": True},
        "plugins": {"enabled": True, "dir": "/root/.cli-proxy-api/plugins", "configs": {"mistral-bridge": {"enabled": True}}},
    }
    config_path = root / "config.yaml"
    config_path.write_text(json.dumps(config), encoding="utf-8")
    config_path.chmod(0o600)
    state = {
        "keys": [{"id": "mock-key", "label": "Mock upstream", "secret": "mock-secret", "base_url": f"http://127.0.0.1:{mock.server_port}/v1", "enabled": True, "channel": "vibe"}],
        "models": [{"id": "mock-model", "client_model": "request-edit-test", "upstream_model": "mock-upstream", "key_id": "mock-key", "enabled": True}],
        "catalog": [], "debug_mode": True,
    }
    state_path = data / "state.json"
    state_path.write_text(json.dumps(state), encoding="utf-8")
    state_path.chmod(0o600)
    container = "mistralbridge-request-edit-" + uuid.uuid4().hex[:8]
    subprocess.run(["docker", "run", "-d", "--name", container, "--network", "host", "-v", f"{auths}:/root/.cli-proxy-api", "-v", f"{config_path}:/CLIProxyAPI/config.yaml", args.image], check=True, stdout=subprocess.DEVNULL)
    report = {"image": args.image, "version": args.version, "binary_sha256": hashlib.sha256(library.read_bytes()).hexdigest(), "container": container, "port": port, "checks": []}
    api_root = "/v0/management/plugins/mistral-bridge"

    def http(path, body=None, method=None, management=True, headers=None):
        request_headers = {"Authorization": "Bearer " + ("request-edit-test-management" if management else "request-edit-test-client")}
        request_headers.update(headers or {})
        payload = None
        if body is not None:
            payload = json.dumps(body).encode()
            request_headers["Content-Type"] = "application/json"
        request = urllib.request.Request(f"http://127.0.0.1:{port}" + path, data=payload, method=method, headers=request_headers)
        try:
            with urllib.request.urlopen(request, timeout=60) as response:
                return response.status, response.read()
        except urllib.error.HTTPError as error:
            return error.code, error.read()

    def api(path, body=None, method=None):
        status, response = http(api_root + path, body, method)
        assert status == 200, f"{path}: expected HTTP 200, got {status}"
        return json.loads(response)

    def ready():
        for _ in range(80):
            try:
                status, response = http("/v1/models", management=False)
                if status == 200 and any(m["id"] == "request-edit-test" for m in json.loads(response).get("data", [])):
                    return
            except (OSError, urllib.error.URLError):
                pass
            time.sleep(0.25)
        raise AssertionError("isolated CPA did not publish the test model")

    def passed(name):
        report["checks"].append(name)
        print("PASS " + name, flush=True)

    def chat(stream=False, endpoint="chat/completions", extra=None, expected=200):
        body = {"model": "request-edit-test", "stream": stream, "seed": 42, "metadata": {"source": "rename", "drop": "delete", "rows": [{"id": 1}, {"id": 2}]}}
        messages = [{"role": "developer", "content": "Reply OK."}, {"role": "user", "content": "Test."}]
        if endpoint == "responses":
            body.update(input=messages, max_output_tokens=32)
        else:
            body.update(messages=messages, max_completion_tokens=32, store=False, stream_options={"include_usage": True})
        body.update(extra or {})
        before = len(captures)
        status, response = http("/v1/" + endpoint, body, management=False, headers={"X-Client-Tag": "client-tag", "X-Unmapped": "unmapped"})
        assert status == expected, f"{endpoint} stream={stream}: expected {expected}, got {status}"
        if expected != 200:
            assert len(captures) == before, "rejected edit still sent an upstream request"
            return None
        assert len(captures) == before + 1, "completion did not reach the mock upstream"
        if stream:
            marker = b"[DONE]" if endpoint == "chat/completions" else b"response.completed"
            assert marker in response, "SSE response did not finish"
        else:
            assert json.loads(response).get("object") == ("chat.completion" if endpoint == "chat/completions" else "response")
        return captures[-1]

    def publish_models():
        current = api("/state")
        api("/config", {"models_revision": current["models_revision"]}, "PATCH")

    try:
        ready()
        assert api("/state")["request_edit"] == {"builtin_enabled": True, "rules": []}
        passed("legacy state defaults to enabled builtin mappings")
        status, page = http("/v0/resource/plugins/mistral-bridge/console")
        assert status == 200 and "请求编辑".encode() in page and b"requestRuleForm" in page
        (root / "console.html").write_bytes(page)
        before = len(captures)
        preview = api("/request-edit/preview", {"body": {"messages": [], "seed": 9007199254740993, "max_completion_tokens": 64}, "headers": {"Authorization": "client-secret", "X-Tag": "tag"}}, "POST")
        assert preview["body"]["random_seed"] == 9007199254740993 and preview["body"]["max_tokens"] == 64
        assert "Authorization" not in preview["headers"] and len(captures) == before
        passed("management preview preserves integers and performs no upstream call")
        for stream in [False, True]:
            capture = chat(stream)
            body = capture["body"]
            assert body["model"] == "mock-upstream" and body["max_tokens"] == 32 and body["random_seed"] == 42
            assert body["messages"][0]["role"] == "system" and body["stream"] == stream
            assert "store" not in body and "stream_options" not in body and "seed" not in body
            assert capture["headers"]["authorization"] == "Bearer mock-secret" and "x-client-tag" not in capture["headers"]
            passed("native Chat aliases and transport, stream=" + str(stream))
        rules = [
            {"name": "temperature", "enabled": True, "action": "set", "target": "body", "path": "/temperature", "value_json": "0.3", "overwrite": True},
            {"name": "forward header", "enabled": True, "action": "copy", "source": "header", "from": "x-client-tag", "target": "header", "path": "X-Forwarded-Tag", "overwrite": True},
            {"name": "header to body", "enabled": True, "action": "copy", "source": "header", "from": "X-Client-Tag", "target": "body", "path": "/metadata/client_tag", "overwrite": True},
            {"name": "copy converted message", "enabled": True, "action": "copy", "source": "body", "from": "/messages/0/content", "target": "body", "path": "/metadata/first_message", "overwrite": True},
            {"name": "rename metadata", "enabled": True, "action": "move", "source": "body", "from": "/metadata/source", "target": "body", "path": "/metadata/renamed", "overwrite": True},
            {"name": "remove metadata", "enabled": True, "action": "delete", "target": "body", "path": "/metadata/drop"},
            {"name": "array member", "enabled": True, "action": "set", "target": "body", "path": "/metadata/rows/*/edited", "value_json": "true", "overwrite": True},
        ]
        saved = api("/request-edit", {"rules": rules}, "PATCH")
        assert all(rule["id"] for rule in saved["rules"])
        for endpoint in ["chat/completions", "responses"]:
            for stream in [False, True]:
                capture = chat(stream, endpoint)
                body = capture["body"]
                assert body["temperature"] == 0.3 and body["metadata"]["client_tag"] == "client-tag", "input headers are missing from the executor"
                assert body["metadata"]["first_message"] == body["messages"][0]["content"]
                if endpoint == "chat/completions":
                    assert body["metadata"]["renamed"] == "rename" and "source" not in body["metadata"] and "drop" not in body["metadata"]
                    assert all(item["edited"] for item in body["metadata"]["rows"])
                assert capture["headers"]["x-forwarded-tag"] == "client-tag" and "x-unmapped" not in capture["headers"]
                passed("ordered body/header rules via " + endpoint + ", stream=" + str(stream))
        override = {"name": "last temperature", "enabled": True, "action": "set", "target": "body", "path": "/temperature", "value_json": "0.7", "overwrite": True}
        saved = api("/request-edit", {"rules": [saved["rules"][0], override]}, "PATCH")
        assert chat()["body"]["temperature"] == 0.7
        saved["rules"].reverse()
        api("/request-edit", {"rules": saved["rules"]}, "PATCH")
        assert chat()["body"]["temperature"] == 0.3
        saved["rules"][-1]["enabled"] = False
        api("/request-edit", {"rules": saved["rules"]}, "PATCH")
        assert chat()["body"]["temperature"] == 0.7
        passed("reordering and individual rule toggles change actual requests")
        api("/request-edit", {"builtin_enabled": False}, "PATCH")
        capture = chat()
        assert capture["body"]["seed"] == 42 and capture["body"]["temperature"] == 0.7
        assert capture["body"]["messages"][0]["role"] == "developer"
        subprocess.run(["docker", "restart", container], check=True, stdout=subprocess.DEVNULL)
        ready()
        assert not api("/state")["request_edit"]["builtin_enabled"] and chat()["body"]["temperature"] == 0.7
        passed("independent builtin switch and custom rules survive a CPA restart")
        before = api("/state")["request_edit"]
        status, _ = http(api_root + "/request-edit", {"rules": [{"name": "invalid auth", "enabled": True, "action": "set", "target": "header", "path": "Authorization", "value_json": '"wrong"'}]}, "PATCH")
        assert status == 400 and api("/state")["request_edit"] == before
        passed("invalid rule is rejected without changing saved configuration")
        bad_parent = {"name": "bad parent", "enabled": True, "action": "set", "target": "body", "path": "/metadata/source/tag", "value_json": '"tag"', "overwrite": True}
        api("/request-edit", {"builtin_enabled": True, "rules": [bad_parent]}, "PATCH")
        chat(expected=400)
        passed("runtime path errors return HTTP 400 before upstream transport")
        api("/request-edit", {"rules": []}, "PATCH")

        if args.live_state:
            live_path = Path(args.live_state).resolve()
            live_hash = hashlib.sha256(live_path.read_bytes()).hexdigest()
            live = json.loads(live_path.read_text())
            mapping = next(m for m in live["models"] if m["enabled"])
            candidates = [k for k in live["keys"] if k["enabled"] and (not mapping.get("key_id") or k["id"] == mapping["key_id"])]
            credential = max(candidates, key=lambda k: k.get("priority", 0))
            original_proxy = api("/state").get("proxy_url", "")
            created_key = None
            created_model = None
            try:
                api("/proxy", {"proxy_url": live.get("proxy_url", "")}, "PATCH")
                created_key = api("/keys", {"label": "Temporary real upstream smoke", "secret": credential["secret"], "base_url": credential.get("base_url", "https://api.mistral.ai/v1"), "channel": credential.get("channel", "auto"), "enabled": True}, "POST")
                saved_models = api("/models", {"client_model": "request-edit-real-smoke", "upstream_model": mapping["upstream_model"], "key_id": created_key["id"], "enabled": True}, "POST")
                created_model = next(m for m in saved_models["models"] if m["client_model"] == "request-edit-real-smoke")
                publish_models()
                for stream in [False, True]:
                    body = {"model": "request-edit-real-smoke", "messages": [{"role": "user", "content": "Reply with only OK."}], "stream": stream, "max_completion_tokens": 16, "seed": 7, "store": False}
                    status, response = http("/v1/chat/completions", body, management=False)
                    assert status == 200, f"real upstream stream={stream}: HTTP {status}"
                    if stream:
                        assert b"[DONE]" in response
                        usage = [json.loads(line[5:].strip()).get("usage") for line in response.splitlines() if line.startswith(b"data:") and line[5:].strip() != b"[DONE]"]
                        usage = next((item for item in reversed(usage) if item), None)
                    else:
                        usage = json.loads(response).get("usage")
                    assert usage and usage.get("prompt_tokens", 0) > 0 and usage.get("total_tokens", 0) > 0, "native Mistral usage was not returned"
                    report.setdefault("real_upstream", []).append({"stream": stream, "http_status": status, "usage": usage})
                    passed("real Mistral completion and native usage, stream=" + str(stream))
            finally:
                if created_model:
                    api("/models/delete", {"id": created_model["id"]}, "DELETE")
                if created_key:
                    api("/keys/delete", {"id": created_key["id"]}, "DELETE")
                api("/proxy", {"proxy_url": original_proxy}, "PATCH")
                publish_models()
            assert hashlib.sha256(live_path.read_bytes()).hexdigest() == live_hash, "existing plugin state was modified"
            passed("live CPA plugin state remains unchanged")

        for _ in range(100):
            rows = [json.loads(line) for line in (data / "requests.jsonl").read_text().splitlines()]
            streamed = [row for row in rows if row.get("stream") and row.get("status") == "ok"]
            if streamed and all(row.get("usage_known") and row.get("total_tokens", 0) > 0 for row in streamed):
                break
            time.sleep(0.05)
        else:
            raise AssertionError("stream usage was not retained in plugin logs")
        for row in rows:
            if row.get("debug_available"):
                debug = json.loads((data / "debug-logs" / (row["id"] + ".json")).read_text())
                if debug["request"].get("method"):
                    assert debug["request"]["headers"].get("Authorization") == ["[redacted]"]
                else:
                    assert row.get("http_status") == 400, "missing request in a transport debug record"
        passed("stream token logs and edited request debug redaction")
        report["passed"] = True
    except Exception:
        report["passed"] = False
        raise
    finally:
        (root / "verification.json").write_text(json.dumps(report, indent=2), encoding="utf-8")
        mock.shutdown()
        mock.server_close()
        if not args.keep:
            subprocess.run(["docker", "rm", "-f", container], check=True, stdout=subprocess.DEVNULL)
        print("REPORT " + str(root / "verification.json"), flush=True)
        if args.keep:
            print(f"CONSOLE http://127.0.0.1:{port}/v0/resource/plugins/mistral-bridge/console", flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--library", required=True)
    parser.add_argument("--version", default="0.3.3")
    parser.add_argument("--work-dir", required=True, help="new directory for isolated fixtures")
    parser.add_argument("--image", default="eceasy/cli-proxy-api:v8.0.78")
    parser.add_argument("--live-state", help="optional local plugin state for two real-upstream requests")
    parser.add_argument("--keep", action="store_true", help="retain the isolated CPA container for console QA")
    run(parser.parse_args())
