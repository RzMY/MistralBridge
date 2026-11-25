"""Verify per-credential proxy pools and CPA ownership using loopback mocks only."""
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

    def handler(tag):
        class Mock(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def reply(self, body=None):
                credential = self.headers.get("Authorization")
                assert credential in {"Bearer mock-one", "Bearer mock-two"}
                with capture_lock:
                    captures.append((tag, credential, self.command))
                if body is None:
                    payload = b'{"data":[{"id":"mock"}]}'
                    content_type = "application/json"
                elif body.get("stream"):
                    payload = b'data: {"id":"mock","choices":[{"index":0,"delta":{"content":"OK"}}]}\n\ndata: {"id":"mock","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}\n\ndata: [DONE]\n\n'
                    content_type = "text/event-stream"
                else:
                    payload = b'{"id":"mock","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"total_tokens":1}}'
                    content_type = "application/json"
                self.send_response(200)
                self.send_header("Content-Type", content_type)
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            def do_GET(self):
                self.reply()

            def do_POST(self):
                self.reply(json.loads(self.rfile.read(int(self.headers["Content-Length"])) ))
        return Mock

    servers = [ThreadingHTTPServer(("127.0.0.1", 0), handler(tag)) for tag in ["pool-a", "pool-b", "native", "default", "direct"]]
    for server in servers:
        threading.Thread(target=server.serve_forever, daemon=True).start()
    urls = [f"http://127.0.0.1:{s.server_port}" for s in servers]
    with socket.socket() as reserved:
        reserved.bind(("127.0.0.1", 0))
        port = reserved.getsockname()[1]
    config = {"config-version": 8, "server": {"host": "127.0.0.1", "port": port}, "access": {"api-keys": ["mock-client"]}, "management": {"allow-remote": False, "secret-key": "mock-management", "disable-control-panel": True}, "routing": {"retry": {"request-retry": 0, "max-retry-interval": 0}}, "plugins": {"enabled": True, "dir": "/root/.cli-proxy-api/plugins", "configs": {"mistral-bridge": {"enabled": True}}}}
    (root / "config.yaml").write_text(json.dumps(config))
    state = {"keys": [{"id": key, "label": key, "secret": "mock-" + key, "enabled": True, "base_url": "http://mistral.invalid/v1"} for key in ["one", "two"]], "models": [{"id": key, "client_model": key, "upstream_model": "mock", "key_id": key, "enabled": True} for key in ["one", "two"]], "catalog": [], "proxy_url": urls[3]}
    (data / "state.json").write_text(json.dumps(state))
    container = "mistralbridge-proxy-pool-" + uuid.uuid4().hex[:8]
    report = {"image": args.image, "binary_sha256": hashlib.sha256(library.read_bytes()).hexdigest(), "checks": [], "real_credentials_used": False}
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def http(path, body=None, method=None):
        key = "mock-management" if "management" in path else "mock-client"
        req = urllib.request.Request(f"http://127.0.0.1:{port}" + path, headers={"Authorization": "Bearer " + key, "Content-Type": "application/json"}, data=None if body is None else json.dumps(body).encode(), method=method)
        try:
            with opener.open(req, timeout=12) as response:
                return response.status, response.read()
        except urllib.error.HTTPError as error:
            return error.code, error.read()

    def api(path, body=None, method=None):
        status, raw = http(path, body, method)
        assert status == 200, (path, status, raw[:300])
        return json.loads(raw)

    prefix = "/v0/management/plugins/mistral-bridge"

    def ready():
        for _ in range(100):
            try:
                if len(api("/v1/models").get("data", [])) >= 2:
                    return
            except (OSError, urllib.error.URLError, AssertionError):
                pass
            time.sleep(.2)
        raise AssertionError("CPA not ready")

    def chat(key, tag, stream=False):
        before = len(captures)
        status, raw = http("/v1/chat/completions", {"model": key, "messages": [{"role": "user", "content": "mock"}], "stream": stream})
        assert status == 200, (status, raw[:300])
        assert captures[before:] == [(tag, "Bearer mock-" + key, "POST")], captures[before:]
        if stream:
            assert b"[DONE]" in raw, raw[:1000]

    def passed(name):
        report["checks"].append(name)
        print("PASS " + name, flush=True)

    try:
        subprocess.run(["docker", "run", "-d", "--name", container, "--network", "host", "-v", f"{auths}:/root/.cli-proxy-api", "-v", f"{root / 'config.yaml'}:/CLIProxyAPI/config.yaml", args.image], check=True, stdout=subprocess.DEVNULL)
        ready()
        pool = [api(prefix + "/proxies", {"name": tag, "url": url}, "POST") for tag, url in zip(["A", "B"], urls[:2])]
        for i, key in enumerate(["one", "two"]):
            api(prefix + "/keys", {"id": key, "label": key, "enabled": True, "base_url": "http://mistral.invalid/v1", "proxy_mode": "pool", "proxy_id": pool[i]["id"]}, "PATCH")
        native = api("/v0/management/auth-files")["files"]
        names = {key: "mistral-bridge-" + hashlib.sha256(key.encode()).hexdigest()[:32] + ".json" for key in ["one", "two"]}
        for i, key in enumerate(["one", "two"]):
            assert json.loads((auths / names[key]).read_text())["proxy_url"] == urls[i]
            chat(key, "pool-" + ["a", "b"][i])
            chat(key, "pool-" + ["a", "b"][i], True)
            before = len(captures)
            api(prefix + "/keys/test", {"id": key}, "POST")
            assert captures[before:] == [("pool-" + ["a", "b"][i], "Bearer mock-" + key, "GET")]
        passed("distinct pool bindings reach the correct proxy for completion, stream and model catalog")
        assert http(prefix + "/proxies", {"id": pool[0]["id"]}, "DELETE")[0] == 409
        passed("bound proxies cannot be deleted silently")
        api("/v0/management/auth-files/fields", {"name": names["one"], "proxy_url": urls[2]}, "PATCH")
        chat("one", "native")
        before = len(captures)
        api(prefix + "/keys/test", {"id": "one"}, "POST")
        assert captures[before:][0][0] == "native"
        view = next(k for k in api(prefix + "/state")["keys"] if k["id"] == "one")
        assert view["proxy_mode"] == "native"
        api(prefix + "/keys", {"id": "one", "label": "Renamed", "enabled": True, "base_url": "http://mistral.invalid/v1"}, "PATCH")
        api(prefix + "/proxies", {"id": pool[0]["id"], "name": "A changed", "url": urls[1]}, "PATCH")
        chat("one", "native")
        passed("CPA override applies immediately and survives unrelated key and pool edits")
        api(prefix + "/keys", {"id": "two", "label": "two", "enabled": True, "base_url": "http://mistral.invalid/v1", "proxy_mode": "pool", "proxy_id": pool[0]["id"]}, "PATCH")
        api(prefix + "/proxies", {"id": pool[0]["id"], "name": "A", "url": urls[0]}, "PATCH")
        chat("two", "pool-a")
        passed("editing a pool updates credentials still owned by that pool")
        subprocess.run(["docker", "restart", container], check=True, stdout=subprocess.DEVNULL)
        ready()
        chat("one", "native")
        chat("two", "pool-a")
        passed("restart preserves CPA override and pool ownership independently")
        api(prefix + "/keys", {"id": "one", "label": "one", "enabled": True, "base_url": "http://mistral.invalid/v1", "proxy_mode": "inherit"}, "PATCH")
        chat("one", "default")
        assert "proxy_url" not in json.loads((auths / names["one"]).read_text())
        passed("explicit inherit removes native override and uses plugin default")
        api(prefix + "/keys", {"id": "one", "label": "one", "enabled": True, "base_url": urls[4] + "/v1", "proxy_mode": "direct"}, "PATCH")
        chat("one", "direct")
        assert json.loads((auths / names["one"]).read_text())["proxy_url"] == "direct"
        passed("direct bypasses both default and native proxies")
        with socket.socket() as closed:
            closed.bind(("127.0.0.1", 0))
            dead = f"http://127.0.0.1:{closed.getsockname()[1]}"
        api("/v0/management/auth-files/fields", {"name": names["two"], "proxy_url": dead}, "PATCH")
        before = len(captures)
        status, _ = http("/v1/chat/completions", {"model": "two", "messages": []})
        assert status >= 400 and len(captures) == before
        passed("failed selected proxy never falls back to another egress")
        api(prefix + "/keys", {"id": "two", "label": "two", "enabled": True, "base_url": "http://mistral.invalid/v1", "proxy_mode": "inherit"}, "PATCH")
        api(prefix + "/proxies", {"id": pool[0]["id"]}, "DELETE")
        api(prefix + "/proxies", {"id": pool[1]["id"]}, "DELETE")
        passed("unbinding permits safe pool removal")
        report["mock_requests"] = len(captures)
    finally:
        logs = subprocess.run(["docker", "logs", container], capture_output=True, text=True)
        (root / "cpa.log").write_text(logs.stdout + logs.stderr)
        subprocess.run(["docker", "rm", "-f", container], check=False, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        for server in servers:
            server.shutdown()
        (root / "report.json").write_text(json.dumps(report, indent=2) + "\n")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--library", required=True)
    parser.add_argument("--version", default="0.5.0")
    parser.add_argument("--image", default="eceasy/cli-proxy-api:v8.0.78")
    parser.add_argument("--work-dir", required=True)
    run(parser.parse_args())
