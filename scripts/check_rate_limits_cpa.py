"""Check cooldowns in an isolated CPA container using only a loopback mock.

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
    capture_lock = threading.Lock()

    class Upstream(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            credential = self.headers.get("Authorization")
            assert credential in {"Bearer mock-one", "Bearer mock-two"}, "non-test credential"
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            model = body["model"]
            with capture_lock:
                captures.append((model, credential))
                attempt = captures.count((model, credential))
            limited = model.startswith("limit-") and attempt == 1
            if limited:
                payload = b'{"message":"mock rate limit"}'
                self.send_response(429)
                if model != "limit-default":
                    self.send_header("Retry-After", "3")
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
        "management": {"allow-remote": False, "secret-key": "rate-limit-test-management", "disable-control-panel": True},
        "plugins": {"enabled": True, "dir": "/root/.cli-proxy-api/plugins", "configs": {"mistral-bridge": {"enabled": True}}},
    }
    config_path = root / "config.yaml"
    config_path.write_text(json.dumps(config), encoding="utf-8")
    models = {
        "rps": "glm-5-2", "rps-alias": "glm-5-2", "tpm": "mistral-small-2603",
        "stream-tpm": "mistral-large-2512", "normal": "limit-normal", "stream": "limit-stream",
        "default": "limit-default", "fallback": "limit-fallback", "bound": "limit-fallback",
        "unrelated": "unrelated-model",
    }
    state = {
        "keys": [{"id": name, "label": name, "secret": "mock-" + name, "base_url": f"http://127.0.0.1:{mock.server_port}/v1", "enabled": True, "priority": priority}
                 for name, priority in [("one", 10), ("two", 0)]],
        "models": [{"id": name, "client_model": name, "upstream_model": model, "key_id": "" if name == "fallback" else "one", "enabled": True} for name, model in models.items()],
        "catalog": [],
    }
    (data / "state.json").write_text(json.dumps(state), encoding="utf-8")
    container = "mistralbridge-rate-limits-" + uuid.uuid4().hex[:8]
    report = {"image": args.image, "version": args.version, "binary_sha256": hashlib.sha256(library.read_bytes()).hexdigest(), "checks": [], "real_credentials_used": False}
    # Do not use environment proxies for local checks.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def http(path, body=None):
        headers = {"Authorization": "Bearer rate-limit-test-client", "Content-Type": "application/json"}
        request = urllib.request.Request(f"http://127.0.0.1:{port}" + path, headers=headers, data=None if body is None else json.dumps(body).encode())
        try:
            with opener.open(request, timeout=15) as response:
                return response.status, response.read()
        except urllib.error.HTTPError as error:
            return error.code, error.read()

    def chat(model, expected=200, stream=False, reaches=True):
        before = len(captures)
        status, response = http("/v1/chat/completions", {"model": model, "messages": [{"role": "user", "content": "Mock test"}], "max_tokens": 16, "stream": stream})
        assert status == expected, f"{model}: expected {expected}, got {status}: {response[:300]!r}"
        assert len(captures) == before + int(reaches), f"{model}: unexpected upstream requests: {len(captures)-before}"
        if stream and expected == 200:
            assert b"[DONE]" in response
        return response

    def passed(name):
        report["checks"].append(name)
        print("PASS " + name, flush=True)

    try:
        subprocess.run(["docker", "run", "-d", "--name", container, "--network", "host", "-v", f"{auths}:/root/.cli-proxy-api", "-v", f"{config_path}:/CLIProxyAPI/config.yaml", args.image], check=True, stdout=subprocess.DEVNULL)
        for _ in range(100):
            try:
                status, response = http("/v1/models")
                if status == 200 and any(m["id"] == "rps" for m in json.loads(response).get("data", [])):
                    break
            except (OSError, urllib.error.URLError):
                pass
            time.sleep(.2)
        else:
            raise AssertionError("CPA did not publish test models")
        chat("rps")
        for _ in range(5):
            chat("rps-alias", 429, reaches=False)
        chat("unrelated")
        time.sleep(2.1)
        chat("rps")
        passed("0.5 RPS, shared aliases, model isolation and recovery")

        for model, stream in [("normal", False), ("stream", True)]:
            chat(model, 429, stream)
            for _ in range(10):
                chat(model, 429, stream, reaches=False)
            time.sleep(3.1)
            chat(model, stream=stream)
            passed(model + " 429 respects Retry-After, blocks repeats and recovers")

        chat("fallback", 429)
        chat("bound", 429, reaches=False)
        chat("fallback", 429)
        assert captures[-1] == ("limit-fallback", "Bearer mock-two")
        chat("fallback", 429, reaches=False)
        time.sleep(3.1)
        chat("fallback")
        assert captures[-1] == ("limit-fallback", "Bearer mock-one")
        passed("priority fallback, explicit binding, exhausted pool and primary recovery")

        chat("default", 429)
        chat("tpm")
        chat("stream-tpm", stream=True)
        time.sleep(2.1)
        for model, stream in [("default", False), ("tpm", False), ("stream-tpm", True)]:
            chat(model, 429, stream, reaches=False)
        passed("missing Retry-After and actual usage trigger cooldown in both completion paths")
        # Wait out the actual minute window, without a live service or real key.
        for _ in range(2):
            time.sleep(29.5)
        for model, stream in [("default", False), ("tpm", False), ("stream-tpm", True)]:
            chat(model, stream=stream)
        passed("default 429 cooldown and both TPM windows recover after one minute")
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
