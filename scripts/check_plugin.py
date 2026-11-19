"""Load the built dynamic library through CPA's C ABI and check its registration."""

import ctypes as c
import json
from pathlib import Path
import sys


class Buffer(c.Structure):
    _fields_ = [("ptr", c.c_void_p), ("length", c.c_size_t)]


Call = c.CFUNCTYPE(c.c_int, c.c_char_p, c.c_void_p, c.c_size_t, c.POINTER(Buffer))
Free = c.CFUNCTYPE(None, c.c_void_p, c.c_size_t)
Shutdown = c.CFUNCTYPE(None)


class PluginAPI(c.Structure):
    _fields_ = [("abi_version", c.c_uint32), ("call", Call), ("free_buffer", Free), ("shutdown", Shutdown)]


def check_plugin(path, version):
    library = c.CDLL(str(Path(path).resolve()))
    initialize = library.cliproxy_plugin_init
    initialize.argtypes = [c.c_void_p, c.POINTER(PluginAPI)]
    initialize.restype = c.c_int
    api = PluginAPI()
    if initialize(None, c.byref(api)) != 0 or api.abi_version != 1:
        raise RuntimeError("Plugin initialization or ABI version check failed")
    if not api.call or not api.free_buffer or not api.shutdown:
        raise RuntimeError("Plugin did not expose the required C ABI callbacks")
    response = Buffer()
    try:
        code = api.call(b"plugin.register", None, 0, c.byref(response))
        if code != 0 or not response.ptr or not response.length:
            raise RuntimeError("Plugin registration call failed")
        envelope = json.loads(c.string_at(response.ptr, response.length))
        if not envelope.get("ok"):
            raise RuntimeError(f"Registration returned an error: {envelope}")
        registration = envelope["result"]
        if registration["schema_version"] != 10:
            raise RuntimeError("Unexpected plugin schema version")
        if registration["metadata"]["Version"] != version:
            raise RuntimeError("Embedded plugin version differs from the build/tag version")
        if registration["metadata"]["Name"] != "Mistral Bridge":
            raise RuntimeError("Unexpected plugin name")
        print(f"C ABI load and registration passed: {Path(path).name} ({version})")
    finally:
        if response.ptr:
            api.free_buffer(response.ptr, response.length)
        api.shutdown()


if __name__ == "__main__":
    check_plugin(sys.argv[1], sys.argv[2])
