"""Check native CPA credentials, routing and cooldowns with a loopback mock.

No real credentials are accepted or loaded. The existing CPA is not modified.
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
from concurrent.futures import ThreadPoolExecutor

from check_plugin import check_plugin


def run(args):
    root = Path(args.work_dir).resolve()
    root.mkdir(parents=True, mode=0o700, exist_ok=False)
    library = Path(args.library).resolve()
    check_plugin(library, args.version)
    auths = root / "auths"
    plugins = auths / "plugins/linux/amd64"
    data = auths / "plugin-data/mistral-bridge"
    plugins.mkdir(parents=True)
    data.mkdir(parents=True)
    shutil.copy2(library, plugins / "mistral-bridge.so")
    captures = []
    arrival_times = []
    capture_lock = threading.Lock()

    class Upstream(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            credential = self.headers.get("Authorization")
            assert credential in {"Bearer mock-one", "Bearer mock-two", "Bearer mock-other"}, "non-test credential"
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            model = body["model"]
            with capture_lock:
                captures.append((model, credential))
                arrival_times.append(time.monotonic())
                attempt = captures.count((model, credential))
            limited = (model.startswith("limit-") and attempt == 1) or (model == "shared-upstream" and credential != "Bearer mock-other")
            if limited:
                payload = b'{"message":"mock rate limit"}'
                self.send_response(503 if model in {"limit-transient", "shared-upstream"} else 429)
                if model not in {"limit-default", "limit-transient", "shared-upstream"}:
                    self.send_header("Retry-After", "12")
                content_type = "application/json"
            else:
                tokens = {"mistral-small-2603": 500000, "mistral-large-2512": 800000}.get(model, 8)
                usage = {"prompt_tokens": tokens - 1, "completion_tokens": 1, "total_tokens": tokens}
                if body.get("stream"):
                    events = [
                        {"id": "mock", "object": "chat.completion.chunk", "model": model, "choices": [{"index": 0, "delta": {"content": "OK"}}]},
                        {"id": "mock", "object": "chat.completion.chunk", "model": model, "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}], "usage": usage},
                    ]
                    payload = ("".join("data: " + json.dumps(e) + "\n\n" for e in events) + "data: [DONE]\n\n").encode()
                    content_type = "text/event-stream"
                else:
                    payload = json.dumps({"id": "mock", "object": "chat.completion", "model": model, "choices": [{"index": 0, "message": {"role": "assistant", "content": "OK"}, "finish_reason": "stop"}], "usage": usage}).encode()
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
        "access": {"api-keys": ["rate-limit-test-client"]},
        "routing": {"strategy": "weighted-round-robin", "retry": {"request-retry": 0, "max-retry-interval": 0}, "cooldown": {"transient-error-cooldown-seconds": 2}},
        "api-keys": {"openai-compatibility": [{"name": "mock-other-provider", "base-url": f"http://127.0.0.1:{mock.server_port}/v1", "keys": [{"api-key": "mock-other"}], "models": [{"name": "shared-upstream", "alias": "shared"}]}]},
        "management": {"allow-remote": False, "secret-key": "rate-limit-test-management", "disable-control-panel": True},
        "plugins": {"enabled": True, "dir": "/root/.cli-proxy-api/plugins", "configs": {"mistral-bridge": {"enabled": True}}},
    }
    config_path = root / "config.yaml"
    config_path.write_text(json.dumps(config), encoding="utf-8")
    models = {"plain":"plain-upstream", "normal":"limit-normal", "stream":"limit-stream", "transient":"limit-transient", "disable":"limit-disable", "rps":"glm-5-2", "burst":"deepseek-v4-1-flash-1017", "tpm":"mistral-small-2603", "stream-tpm":"mistral-large-2512", "shared":"shared-upstream"}
    state = {
        "keys": [{"id": name, "label": name, "secret": "mock-" + name, "base_url": f"http://127.0.0.1:{mock.server_port}/v1", "enabled": True, "priority": priority}
                 for name, priority in [("one", 0), ("two", 0)]],
        "models": [{"id": name, "client_model": name, "upstream_model": model, "key_id": "" if name in {"plain", "shared"} else "one", "enabled": True} for name, model in models.items()],
        "catalog": [],
    }
    (data / "state.json").write_text(json.dumps(state), encoding="utf-8")
    container = "mistralbridge-native-" + uuid.uuid4().hex[:8]
    report = {"image": args.image, "version": args.version, "binary_sha256": hashlib.sha256(library.read_bytes()).hexdigest(), "checks": [], "real_credentials_used": False}
    # Do not use environment proxies for local checks.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def http(path, body=None, method=None):
        headers = {"Authorization": "Bearer " + ("rate-limit-test-management" if "management" in path else "rate-limit-test-client"), "Content-Type": "application/json"}
        request = urllib.request.Request(f"http://127.0.0.1:{port}" + path, headers=headers, method=method, data=None if body is None else json.dumps(body).encode())
        try:
            with opener.open(request, timeout=15) as response:
                return response.status, response.read(), dict(response.headers)
        except urllib.error.HTTPError as error:
            return error.code, error.read(), dict(error.headers)

    def chat(model, expected=200, stream=False, reaches=True):
        before = len(captures)
        status, response, headers = http("/v1/chat/completions", {"model": model, "messages": [{"role": "user", "content": "Mock test"}], "max_tokens": 16, "stream": stream})
        assert status == expected, f"{model}: expected {expected}, got {status}: {response[:300]!r}"
        assert len(captures) == before + int(reaches), f"{model}: unexpected upstream requests: {len(captures)-before}"
        if stream and expected == 200:
            assert b"[DONE]" in response
        return response, headers

    def passed(name):
        report["checks"].append(name)
        print("PASS " + name, flush=True)

    try:
        subprocess.run(["docker", "run", "-d", "--name", container, "--network", "host", "-v", f"{auths}:/root/.cli-proxy-api", "-v", f"{config_path}:/CLIProxyAPI/config.yaml", args.image], check=True, stdout=subprocess.DEVNULL)
        for _ in range(100):
            try:
                status, response, headers = http("/v1/models")
                if status == 200 and any(m["id"] == "rps" for m in json.loads(response).get("data", [])):
                    break
            except (OSError, urllib.error.URLError):
                pass
            time.sleep(.2)
        else:
            raise AssertionError("CPA did not publish test models")
        def api(path, body=None, method=None):
            status, raw, _ = http(path, body, method)
            assert status == 200, f"{path}: {status} {raw[:300]!r}"
            return json.loads(raw)

        native = [f for f in api("/v0/management/auth-files")["files"] if f.get("provider") == "mistral-bridge"]
        assert len(native) == 2, native
        names = {key: "mistral-bridge-" + hashlib.sha256(key.encode()).hexdigest()[:32] + ".json" for key in ["one", "two"]}
        entries = {key: next(f for f in native if f["name"] == name) for key, name in names.items()}
        for name in names.values():
            doc = json.loads((auths / name).read_text())
            assert "api_key" not in doc and "secret" not in doc
        passed("legacy keys automatically become separate CPA credentials without copying secrets")
        def assert_display_names(expected):
            rows = {f["name"]: f for f in api("/v0/management/auth-files")["files"] if f.get("provider") == "mistral-bridge"}
            for key, label in expected.items():
                row = rows[names[key]]
                # CPAMP prefers email/account over label when displaying a credential.
                displayed = row.get("email") or row.get("account") or row.get("label") or row.get("note") or row["name"]
                assert displayed == label, (key, displayed, label)
                # CPA's file watcher classifies plugin files as OAuth on reload;
                # either path must leave the display label free of fake account data.
                assert row.get("account", "") == "", "internal marker exposed as account"
        assert_display_names({"one":"one", "two":"two"})
        passed("CPA account fields do not shadow plugin credential names")

        for key, weight in [("one", 3), ("two", 1)]:
            api("/v0/management/auth-files/fields", {"name": names[key], "weight": weight}, "PATCH")
        start = len(captures)
        for _ in range(40):
            chat("plain")
        counts = {key: sum(c == "Bearer mock-" + key for _, c in captures[start:]) for key in names}
        assert counts == {"one": 30, "two": 10}, counts
        report["weighted_counts"] = counts
        passed("CPA weighted-round-robin selects credentials in a 3:1 ratio")
        assert_display_names({"one":"one", "two":"two"})

        # CPA's cooldown reset must immediately work; no private 429 timer remains.
        for model, streaming in [("normal", False), ("stream", True)]:
            _, headers = chat(model, 429, streaming)
            assert any(k.lower() == "retry-after" for k in headers), headers
            before = len(captures)
            for _ in range(5):
                status, _, _ = http("/v1/chat/completions", {"model": model, "messages": [{"role":"user","content":"mock"}], "stream": streaming})
                assert status == 429, status
            assert len(captures) == before
            api("/v8/management/routing/cooldown/reset", {"auth_index": entries["one"]["auth_index"]}, "POST")
            chat(model, stream=streaming)
            passed(model + " 429 is tracked by CPA, sends Retry-After and honors native reset")

        chat("transient", 503)
        before = len(captures)
        status, _, _ = http("/v1/chat/completions", {"model":"transient", "messages":[]})
        assert status >= 400 and len(captures) == before
        time.sleep(2.2)
        chat("transient")
        passed("CPA custom 2-second transient cooldown applies to plugin failures")

        # Use the native auth document API, preserving fields CPA does not expose in its form.
        path = auths / names["one"]
        doc = json.loads(path.read_text())
        doc["disable_cooling"] = True
        api("/v0/management/auth-files?name=" + names["one"], doc, "POST")
        chat("disable", 429)
        chat("disable")
        passed("native disable_cooling overrides upstream 429 cooldown")
        doc["disable_cooling"] = False
        api("/v0/management/auth-files?name=" + names["one"], doc, "POST")

        chat("rps")
        queued_at = time.monotonic()
        chat("rps")
        assert time.monotonic() - queued_at >= 1.9
        passed("0.5 RPS bursts queue for two seconds instead of returning a false 429")

        before = len(captures)
        time_before = len(arrival_times)
        def concurrent_burst(_):
            status, response, _ = http("/v1/chat/completions", {"model":"burst", "messages":[{"role":"user","content":"mock"}], "max_tokens":16})
            return status, response
        with ThreadPoolExecutor(max_workers=12) as workers:
            outcomes = list(workers.map(concurrent_burst, range(12)))
        assert all(status == 200 for status, _ in outcomes), outcomes
        burst_times = arrival_times[time_before:]
        assert len(captures) == before + 12 and len(burst_times) == 12
        burst_span = max(burst_times)-min(burst_times)
        assert burst_span < .5, burst_span
        passed("12-request concurrent burst consumes idle RPS capacity without paced latency")
        for model, streaming in [("tpm", False), ("stream-tpm", True)]:
            chat(model, stream=streaming)
            time.sleep(2.1)
            chat(model, 429, streaming, reaches=False)
        passed("normal and streaming usage trigger TPM admission and CPA cooldown")

        start = len(captures)
        for _ in range(8):
            status, response, _ = http("/v1/chat/completions", {"model":"shared", "messages":[{"role":"user","content":"mock"}]})
            assert status == 200, response[:300]
        shared = captures[start:]
        assert any(c == "Bearer mock-other" for _, c in shared), shared
        assert any(c in {"Bearer mock-one", "Bearer mock-two"} for _, c in shared), shared
        passed("same model participates in CPA routing and failover alongside another provider")

        # Saving plugin settings must preserve native scheduling overrides.
        api("/v0/management/plugins/mistral-bridge/keys", {"id":"one", "label":"Renamed", "enabled":True, "priority":0, "base_url":f"http://127.0.0.1:{mock.server_port}/v1"}, "PATCH")
        assert json.loads(path.read_text())["weight"] == 3
        passed("plugin key edits preserve CPA weight and policy metadata")
        assert_display_names({"one":"Renamed", "two":"two"})
        passed("plugin rename remains the CPA display name after native metadata updates")
        # Restart with persisted files: native options must survive bootstrap.
        subprocess.run(["docker", "restart", container], check=True, stdout=subprocess.DEVNULL)
        for _ in range(100):
            try:
                status, response, _ = http("/v1/models")
                if status == 200 and any(m["id"] == "plain" for m in json.loads(response).get("data", [])):
                    break
            except (OSError, urllib.error.URLError):
                pass
            time.sleep(.2)
        else:
            raise AssertionError("CPA did not recover after restart")
        assert json.loads(path.read_text())["weight"] == 3
        chat("plain")
        passed("restart preserves native weight and restores credential-backed models")
        assert_display_names({"one":"Renamed", "two":"two"})
        passed("credential display names survive restart and completion")
        api("/v0/management/auth-files/status", {"name":names["one"], "disabled":True}, "PATCH")
        plugin_keys = api("/v0/management/plugins/mistral-bridge/state")["keys"]
        assert not next(k for k in plugin_keys if k["id"] == "one")["enabled"]
        api("/v0/management/plugins/mistral-bridge/keys/toggle", {"id":"one"}, "PATCH")
        assert not json.loads(path.read_text())["disabled"]
        passed("native disable state is reflected in the console and can be re-enabled there")

        api("/v0/management/plugins/mistral-bridge/keys/delete", {"id":"two"}, "DELETE")
        assert not (auths / names["two"]).exists()
        passed("deleting a plugin key disables and removes its native credential")
        report["mock_requests"] = len(captures)
    finally:
        logs = subprocess.run(["docker", "logs", container], capture_output=True, text=True)
        (root / "cpa.log").write_text(logs.stdout + logs.stderr, encoding="utf-8")
        subprocess.run(["docker", "rm", "-f", container], check=False, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        mock.shutdown()
        (root / "report.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--library", required=True)
    parser.add_argument("--version", default="0.4.0")
    parser.add_argument("--image", default="eceasy/cli-proxy-api:v8.1.2")
    parser.add_argument("--work-dir", required=True)
    run(parser.parse_args())
