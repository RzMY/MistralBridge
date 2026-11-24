"""Refresh the bundled field projection from Mistral's official OpenAPI spec.

Maintainer-only command; requires PyYAML. Runtime filtering is entirely offline.
Only structural keywords needed for extra-field filtering are retained.
"""

import datetime
import hashlib
import json
from pathlib import Path
import urllib.request

import yaml


SOURCE = "https://docs.mistral.ai/openapi.yaml"


def main():
    raw = urllib.request.urlopen(SOURCE, timeout=30).read()
    components = yaml.safe_load(raw)["components"]["schemas"]
    projected = {}

    def project(schema):
        if not isinstance(schema, dict):
            return schema
        result = {}
        for key in ["$ref", "type", "const", "enum", "discriminator"]:
            if key in schema:
                result[key] = schema[key]
        if "$ref" in schema:
            include(schema["$ref"].split("/")[-1])
        if "discriminator" in schema:
            for reference in schema["discriminator"].get("mapping", {}).values():
                include(reference.split("/")[-1])
        for key in ["anyOf", "oneOf"]:
            if key in schema:
                result[key] = [project(item) for item in schema[key]]
        for key in ["items", "additionalProperties"]:
            if key in schema:
                result[key] = project(schema[key])
        if "properties" in schema:
            result["properties"] = {key: project(value) for key, value in schema["properties"].items()}
        return result

    def include(name):
        if name not in projected:
            projected[name] = {}
            projected[name] = project(components[name])

    include("ChatCompletionRequest")
    output = {
        "source": SOURCE,
        "retrieved_on": datetime.date.today().isoformat(),
        "source_sha256": hashlib.sha256(raw).hexdigest(),
        "root": "ChatCompletionRequest",
        "schemas": projected,
    }
    path = Path(__file__).resolve().parent.parent / "request_schema.json"
    path.write_text(json.dumps(output, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(f"Updated {path.name}: {len(projected)} schema definitions")


if __name__ == "__main__":
    main()
