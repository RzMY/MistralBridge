"""Exercise new -> v0.5.0 -> new through real C ABI libraries in subprocesses.

Usage: python scripts/check_telemetry_upgrade.py NEW_LIBRARY OLD_LIBRARY
All data is synthetic and kept in a temporary directory.
"""

import base64
import ctypes as c
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

from check_plugin import Buffer, PluginAPI


def worker(library_path, directory, phase):
    os.environ["MISTRAL_BRIDGE_DATA_DIR"] = directory
    library = c.CDLL(str(Path(library_path).resolve()))
    initialize = library.cliproxy_plugin_init
    initialize.argtypes = [c.c_void_p, c.POINTER(PluginAPI)]
    initialize.restype = c.c_int
    api = PluginAPI()
    assert initialize(None, c.byref(api)) == 0

    def call(method, value):
        raw = json.dumps(value).encode()
        response = Buffer()
        try:
            api.call(method.encode(), raw, len(raw), c.byref(response))
            return json.loads(c.string_at(response.ptr, response.length))
        finally:
            if response.ptr:
                api.free_buffer(response.ptr, response.length)

    def management(method, path, body=None):
        value = {"Method": method, "Path": "/plugins/mistral-bridge" + path}
        if body is not None:
            value["Body"] = base64.b64encode(json.dumps(body).encode()).decode()
        reply = call("management.handle", value)
        assert reply["ok"], reply
        result = reply["result"]
        return result["StatusCode"], json.loads(base64.b64decode(result["Body"]))

    try:
        if phase == "new-initial":
            status, data = management("GET", "/stats")
            assert status == 200 and data["stats"]["requests"] == 10005, data
            status, _ = management("PATCH", "/telemetry/settings", {"max_entries": 20})
            assert status == 200
            assert not call("executor.execute", {"Model": "missing"})["ok"]
            assert management("GET", "/stats")[1]["stats"]["requests"] == 10006
        elif phase == "old-write":
            assert management("GET", "/stats")[0] == 404
            assert management("PATCH", "/debug", {"enabled": False})[0] == 200
            assert not call("executor.execute", {"Model": "old-missing"})["ok"]
        elif phase == "new-verify":
            status, data = management("GET", "/stats")
            assert status == 200 and data["stats"]["requests"] == 10007, data
            assert management("GET", "/telemetry/settings")[1]["max_entries"] == 20
        elif phase == "old-clear":
            assert management("DELETE", "/logs")[0] == 200
        elif phase == "new-after-clear":
            data = management("GET", "/stats")[1]
            assert data["stats"]["requests"] == 10007 and data["incomplete"], data
            assert management("GET", "/logs")[1]["total"] == 0
        else:
            raise ValueError(phase)
    finally:
        api.shutdown()


def check_upgrade(new_library, old_library):
    with tempfile.TemporaryDirectory(prefix="mistral-upgrade-") as directory:
        root = Path(directory)
        (root / "state.json").write_text(json.dumps({"keys": [], "models": [], "catalog": []}))
        now = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
        with (root / "requests.jsonl").open("w", encoding="utf-8") as output:
            for i in range(10005):
                output.write(json.dumps({"id": f"{i:024x}", "model": "legacy", "status": "ok", "created_at": now, "input_tokens": 10, "output_tokens": 5}) + "\n")

        def run(library, phase):
            subprocess.run([sys.executable, __file__, "--worker", str(Path(library).resolve()), directory, phase], check=True, env={**os.environ, "MISTRAL_BRIDGE_DATA_DIR": directory})
            print(f"Passed: {phase}", flush=True)

        run(new_library, "new-initial")
        ledger = root / "telemetry" / "snapshot.json"
        digest = hashlib.sha256(ledger.read_bytes()).digest()
        # Trigger the old implementation's actual >16 MiB compaction path.
        with (root / "requests.jsonl").open("ab") as output:
            output.write((b" " * 1023 + b"\n") * 16385)
        run(old_library, "old-write")
        assert (root / "requests.jsonl").stat().st_size < 16 * 1024 * 1024
        assert hashlib.sha256(ledger.read_bytes()).digest() == digest
        run(new_library, "new-verify")
        run(new_library, "new-verify")
        run(old_library, "old-clear")
        run(new_library, "new-after-clear")
        print("C ABI upgrade/downgrade verification passed.")


if __name__ == "__main__":
    if sys.argv[1] == "--worker":
        worker(*sys.argv[2:])
    else:
        check_upgrade(*sys.argv[1:])
