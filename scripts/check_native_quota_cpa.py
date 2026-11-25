"""Exercise CPA native quota discovery and refresh with fake keys and a TLS mock.

All upstream HTTP is handled by a local CONNECT proxy. It never opens an
outbound connection. The temporary CA is trusted only inside the test container.
"""

import argparse
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import shutil
import socket
import ssl
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
    cert, key = root / "mock.crt", root / "mock.key"
    subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-keyout", str(key), "-out", str(cert), "-subj", "/CN=quota-mock", "-addext", "subjectAltName=DNS:api.mistral.ai,DNS:console.mistral.ai"], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.load_cert_chain(cert, key)
    calls = []
    scenario = {"vibe_percent": 25, "partial": False, "offline": False}

    class MockTLS(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_GET(self):
            if self.headers.get("Authorization") not in {"Bearer mock-one", "Bearer mock-direct"}:
                self.send_error(403)
                return
            calls.append(self.path)
            status = 200
            if self.path == "/api/vibe/whoami":
                body = {"plan_type": "api", "plan_name": "PRO"}
            elif self.path == "/v1/billing/budget":
                body = {
                    "vibe_budget": None if scenario["partial"] else {"initial_budget": 200, "usage_percentage": scenario["vibe_percent"], "currency": "EUR", "reset_at": "2026-12-01T00:00:00Z"},
                    "api_budget": {"initial_budget": 20, "usage_percentage": 50, "currency": "EUR", "reset_at": "2026-12-01T00:00:00Z"},
                }
                if scenario["offline"]:
                    status, body = 503, {"error": "mock offline"}
            elif self.path == "/v1/billing/subscription":
                body = {"plan_name": "PRO", "payg_enabled": False, "vibe_payg_enabled": True}
            else:
                self.send_error(404)
                return
            payload = json.dumps(body).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

    class MockProxy(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_CONNECT(self):
            if self.path not in {"api.mistral.ai:443", "console.mistral.ai:443"}:
                self.send_error(403)
                return
            self.send_response(200)
            self.end_headers()
            self.wfile.flush()
            with tls.wrap_socket(self.connection, server_side=True) as conn:
                MockTLS(conn, self.client_address, self.server)
            self.close_connection = True

    proxy = ThreadingHTTPServer(("127.0.0.1", 0), MockProxy)
    threading.Thread(target=proxy.serve_forever, daemon=True).start()
    with socket.socket() as reserved:
        reserved.bind(("127.0.0.1", 0))
        port = reserved.getsockname()[1]
    config = {
        "config-version": 8,
        "server": {"host": "127.0.0.1", "port": port},
        "access": {"api-keys": ["mock-client"]},
        "management": {"allow-remote": False, "secret-key": "mock-management", "disable-control-panel": True},
        "plugins": {"enabled": True, "dir": "/root/.cli-proxy-api/plugins", "configs": {"mistral-bridge": {"enabled": True}}},
    }
    (root / "config.yaml").write_text(json.dumps(config))
    state = {
        "keys": [{"id": "one", "label": "Quota Mock", "secret": "mock-one", "base_url": "https://api.mistral.ai/v1", "enabled": True, "channel": "vibe"}],
        "models": [{"id": "model", "client_model": "quota-test", "upstream_model": "mock", "enabled": True}],
        "catalog": [], "proxy_url": f"http://127.0.0.1:{proxy.server_port}",
    }
    (data / "state.json").write_text(json.dumps(state))
    container = "mistralbridge-quota-" + uuid.uuid4().hex[:8]
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    report = {"image": args.image, "binary_sha256": hashlib.sha256(library.read_bytes()).hexdigest(), "checks": [], "real_credentials_used": False}

    def http(path, body=None, method=None):
        request = urllib.request.Request(f"http://127.0.0.1:{port}" + path, headers={"Authorization": "Bearer mock-management", "Content-Type": "application/json"}, data=None if body is None else json.dumps(body).encode(), method=method)
        try:
            with opener.open(request, timeout=10) as response:
                return response.status, json.load(response)
        except urllib.error.HTTPError as error:
            return error.code, json.load(error)

    def api(path, body=None, method=None):
        status, response = http(path, body, method)
        assert status == 200, (path, status, response)
        return response

    def passed(name):
        report["checks"].append(name)
        print("PASS " + name, flush=True)

    try:
        subprocess.run(["docker", "run", "-d", "--name", container, "--network", "host", "-e", "SSL_CERT_FILE=/mock.crt", "-v", f"{cert}:/mock.crt:ro", "-v", f"{auths}:/root/.cli-proxy-api", "-v", f"{root / 'config.yaml'}:/CLIProxyAPI/config.yaml", args.image], check=True, stdout=subprocess.DEVNULL)
        for _ in range(100):
            try:
                files = api("/v0/management/auth-files")["files"]
                native = next((f for f in files if f.get("provider") == "mistral-bridge"), None)
                if native:
                    break
            except (OSError, urllib.error.URLError):
                pass
            time.sleep(.2)
        else:
            raise AssertionError("native auth not registered")
        assert native["supports_quota"] and native["quota_provider"] == "mistral-bridge", native
        providers = api("/v0/management/quota/providers")["providers"]
        provider = next(p for p in providers if p["provider"] == "mistral-bridge")
        assert not provider["supports_reset"]
        assert not calls, "capability discovery sent upstream requests"
        passed("native credential advertises quota support without querying upstream")
        body = {"auth_index": native["auth_index"]}
        quota = api("/v0/management/quota/fetch", body, "POST")
        assert quota["subscription"]["plan"] == "PRO"
        assert len(quota["groups"]) == 2 and len(quota["summary"]) == 6
        assert quota["groups"][0]["displayName"] == "Vibe · 当前渠道"
        bucket = quota["groups"][0]["buckets"][0]
        assert bucket["remainingFraction"] == .75 and bucket["resetTime"] == "2026-12-01T00:00:00Z"
        assert "按量付费已开启" in bucket["description"]
        assert quota["summary"][0]["value"] == 150 and quota["summary"][0]["currency"] == "EUR"
        assert len(calls) == 3 and "mock-one" not in json.dumps(quota)
        passed("fresh billing maps both pools, plan, money, percentage, PAYG and reset time")
        assert api("/v0/management/plugins/mistral-bridge/quota", body, "POST") == quota
        console = api("/v0/management/plugins/mistral-bridge/keys/quota", {"id": "one"}, "POST")
        assert console["quotas"]["vibe"]["remaining"] == 150 and len(calls) == 3
        passed("native and plugin quota endpoints share the same five-minute cache")
        scenario["vibe_percent"] = 40
        api("/v0/management/plugins/mistral-bridge/keys/quota", {"id": "one", "force": True}, "POST")
        quota = api("/v0/management/quota/fetch", body, "POST")
        assert quota["groups"][0]["buckets"][0]["remainingFraction"] == .6 and len(calls) == 6
        passed("forced plugin refresh updates native quota immediately")
        scenario["partial"] = True
        api("/v0/management/plugins/mistral-bridge/keys/quota", {"id": "one", "force": True}, "POST")
        quota = api("/v0/management/quota/fetch", body, "POST")
        assert "历史数据" in quota["groups"][0]["displayName"] and quota["summary"][0]["value"] == 120
        assert "本次刷新失败" in quota["groups"][0]["buckets"][0]["description"]
        passed("partial failures retain and label last successful pool values")
        api("/v0/management/plugins/mistral-bridge/keys/channel", {"id": "one", "channel": "studio"}, "PATCH")
        api("/v0/management/auth-files/status", {"name": native["name"], "disabled": True}, "PATCH")
        quota = api("/v0/management/quota/fetch", body, "POST")
        assert quota["groups"][0]["displayName"] == "Studio / API · 当前渠道"
        passed("channel changes and disabled credentials remain inspectable natively")
        scenario["offline"] = True
        api("/v0/management/plugins/mistral-bridge/keys/quota", {"id": "one", "force": True}, "POST")
        quota = api("/v0/management/quota/fetch", body, "POST")
        assert all("历史数据" in g["displayName"] for g in quota["groups"])
        assert all("HTTP 503" in g["buckets"][0]["description"] for g in quota["groups"])
        passed("upstream outages show dated historical values instead of fabricated quota")
        scenario.update(offline=False, partial=False)
        api("/v0/management/auth-files?name=direct-quota.json", {"type": "mistral-bridge", "api_key": "mock-direct", "channel": "studio", "label": "Direct Mock"}, "POST")
        direct = next(f for f in api("/v0/management/auth-files")["files"] if f["name"] == "direct-quota.json")
        quota = api("/v0/management/quota/fetch", {"auth_index": direct["auth_index"]}, "POST")
        assert quota["groups"][0]["displayName"] == "Studio / API · 当前渠道" and quota["summary"][0]["value"] == 10
        passed("standalone CPA credential queries its own billing data")
        before = len(calls)
        status, _ = http("/v0/management/plugins/mistral-bridge/quota/reset", body, "POST")
        assert status >= 400 and len(calls) == before
        passed("unsupported quota reset is rejected without upstream mutation")
        report["mock_billing_requests"] = len(calls)
        (root / "quota-example.json").write_text(json.dumps(quota, indent=2, ensure_ascii=False))
    finally:
        logs = subprocess.run(["docker", "logs", container], capture_output=True, text=True)
        (root / "cpa.log").write_text(logs.stdout + logs.stderr)
        subprocess.run(["docker", "rm", "-f", container], check=False, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        proxy.shutdown()
        (root / "report.json").write_text(json.dumps(report, indent=2) + "\n")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--library", required=True)
    parser.add_argument("--version", default="0.5.0")
    parser.add_argument("--image", default="eceasy/cli-proxy-api:v8.0.78")
    parser.add_argument("--work-dir", required=True)
    run(parser.parse_args())
