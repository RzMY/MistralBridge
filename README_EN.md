<div align="center">

<img src="logo.png" alt="Mistral Bridge" width="96" height="96">

# Mistral Bridge

Connect Mistral to CPA for OpenAI-compatible clients.

[![Build](https://github.com/RzMY/MistralBridge/actions/workflows/build.yml/badge.svg)](https://github.com/RzMY/MistralBridge/actions/workflows/build.yml)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![Platforms](https://img.shields.io/badge/Linux%20%7C%20macOS%20%7C%20Windows-64748b)](https://github.com/RzMY/MistralBridge/releases)

[简体中文](README.md) · **English**

[Releases](https://github.com/RzMY/MistralBridge/releases) · [Build artifacts](https://github.com/RzMY/MistralBridge/actions/workflows/build.yml) · [Report an issue](https://github.com/RzMY/MistralBridge/issues)

</div>

Mistral Bridge is a native dynamic-library plugin for CLIProxyAPI (CPA). It adapts Mistral requests, responses, and reasoning fields to OpenAI Chat Completions and provides a web console for credentials, model mappings, and request logs. Its protocol handling draws on magpie's public implementation. [CPA](https://github.com/router-for-me/CLIProxyAPI) · [magpie](https://github.com/yetone/magpie)

## Features

- **Protocol adaptation** converts assistant reasoning fields, Mistral thinking/text content, and streaming deltas, including reasoning in tool-call messages.
- **Credential management** adds, edits, and tests multiple keys, displays API or Vibe monthly budgets, and configures enabled state and scheduling priority.
- **Model mappings** fetch the upstream catalog and configure client-facing names and reasoning-effort mappings. Changes to mappings and their enabled state update CPA's model list.
- **Request logs** filter by model, result, credential, reasoning effort, and time range, with request counts, tokens, cache hit rate, and generation speed.
- **Panel integration** reuses saved same-origin CPA / CPAMP management sessions and follows CPAMP's light and dark themes. Upstream requests, catalog fetches, and quota queries use CPA's HTTP transport and proxy settings.

## Interface Preview

![Request Logs](imgs/logs.png)
![Credential Management](imgs/keys.png)
![Model Mappings](imgs/models.png)

## Installation and setup

CPA must support C ABI v1 and plugin schema version 10. The plugin has been verified with CPA `v8.0.78` and CPAMP.

### Online Installation

Install Mistral Bridge from the CLIProxyAPI repository (official repository) in the CPA plugin marketplace.

### Manual Installation

1. Download the appropriate ZIP from Releases or the artifacts of a successful Actions run. Select the operating system and architecture of **the host or container running CPA**.
2. Extract the library into the matching operating-system and architecture directory under CPA's plugin directory. For Linux amd64, use `plugins/linux/amd64/mistral-bridge.so`.
3. Enable the plugin in CPA's configuration and restart CPA.

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    mistral-bridge:
      enabled: true
```

| CPA platform | Supported architectures | Library inside ZIP |
| --- | --- | --- |
| Linux | amd64, arm64 | `mistral-bridge.so` |
| macOS | amd64, arm64 | `mistral-bridge.dylib` |
| Windows | amd64 | `mistral-bridge.dll` |

Linux release packages use Ubuntu 22.04 as their build baseline and support glibc 2.35 or newer, including the Debian Bookworm CPA image. For Docker, mount the plugin directory into the container at the location configured by `plugins.dir`.

### First use

1. Open **Mistral Bridge** from the CPA / CPAMP sidebar. The standalone console path is `/v0/resource/plugins/mistral-bridge/console`.
2. Go to **凭证管理 → 添加凭证** (Credentials → Add credential) and enter a Mistral key. The default API URL is `https://api.mistral.ai/v1`.
3. Open **模型映射** (Model mappings), click **拉取目录** (Fetch catalog), then **添加映射** (Add mapping). Select an upstream model, enter the request model name, and set its state to **已启用** (Enabled).
4. Wait for confirmation that CPA's model list has synchronized. Configure your client with CPA's API URL and client API key, then select the request model you configured.

A fresh installation starts with empty credentials, catalog, mappings, and logs. Fetching the catalog updates upstream choices. Enabled mappings determine which models clients can use. When opening the console directly, connect with the management key for that panel.

## Calling a model

For example, configure the following mapping.

| Request model | Upstream model |
| --- | --- |
| `mistral-small` | `mistral-small-latest` |

Send a request to CPA's `/v1/chat/completions` with `model` set to `mistral-small`.

```json
{
  "model": "mistral-small",
  "messages": [{"role": "user", "content": "Hello"}],
  "stream": true
}
```

Request model names are used directly in CPA's model list and API calls. With automatic credential selection, the highest-priority enabled keys are selected in round-robin order. An explicit credential binding uses that credential.

A reasoning-effort mapping can use `high=medium, low=none`. Leaving it empty forwards the requested effort. Supported values depend on the upstream model.

## Quotas, logs, and data

| Item | Behavior |
| --- | --- |
| Monthly quota | Reads the API or Vibe budget based on key type and displays remaining allowance, usage percentage, reset time, and refresh status. Keys in the same organization share the budget. |
| Unavailable quota | Displays the upstream status or error. A previous result retains its timestamp. Custom API URLs display quota lookup as unavailable. |
| Request details | Time, request model, upstream model, reasoning effort, result, masked credential, first-token time, total duration, and token usage. |
| Statistics | Apply filters to the most recent 10,000 retained requests, with paginated details. Missing historical fields display `—`. |
| Cache hit rate | Cached tokens divided by input tokens across requests that reported cache usage. |
| Average token speed | Output tokens from successful streaming requests divided by their combined generation time after the first token. Non-streaming requests display `—` for first-token time. |

State is stored at `<auth-dir>/plugin-data/mistral-bridge/state.json`, with request metadata in `requests.jsonl` in the same directory. When CPA's `auth-dir` is unavailable, the fallback is `$HOME/.cli-proxy-api/plugin-data/mistral-bridge`. Set `MISTRAL_BRIDGE_DATA_DIR` to use a different directory.

`state.json` contains upstream keys. The console displays masked values. Persist the plugin and data directories when using containers. To repeat first-time setup, stop CPA, back up and move this plugin's data directory, then start CPA again.

## Build from source

Use Go 1.26.11 and a C compiler for the target platform. On Linux amd64, run the following command.

```sh
CGO_ENABLED=1 GOAMD64=v1 CGO_CFLAGS="-O2 -g -march=x86-64" \
  go build -trimpath -buildvcs=false -buildmode=c-shared \
  -ldflags="-s -w -X main.pluginVersion=0.2.1" \
  -o build/mistral-bridge.so .
```

Other platforms build on native runners. CPU baselines are amd64 v1 and ARMv8.0. The workflow contains the test and packaging commands. [Build workflow](.github/workflows/build.yml)

```sh
go test -race -count=1 ./...
go vet ./...
node --check web/app.js
python3 -m unittest discover -s scripts -p 'test_*.py' -v
```

## Automated builds and releases

A push to `main` runs checks, tests, builds for five platforms, and dynamic-library loading checks. The resulting `mistral-bridge-dev-<commit>` Actions artifact is retained for 14 days.

Pushing a numeric version tag such as `v0.2.1` runs the same pipeline and creates a **Release draft** with generated notes after all checks pass. The tag sets the embedded plugin version. The draft includes five platform ZIPs and SHA-256 `checksums.txt`. Asset names follow `mistral-bridge_<version>_<goos>_<goarch>.zip`, with the corresponding dynamic library at the ZIP root.

```sh
git tag v0.2.1
git push origin v0.2.1
```

Replace the example version with the version you intend to release. Reruns update existing drafts and attachments, while published releases are protected. Review and publish the draft when ready. The store discovers versions through GitHub's latest-release API. The workflow uses the repository's automatic `GITHUB_TOKEN`. [Plugin store requirements](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store#release-requirements)

## Acknowledgements

Thanks to CPA for the plugin runtime, magpie for the Mistral protocol reference, and ClinePassBridge for management UI and documentation organization references. [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) · [magpie](https://github.com/yetone/magpie) · [ClinePassBridge](https://github.com/xiao-qiu-qiu/ClinePassBridge)

The project icon adds a bridge element to Mistral's pixel mark to identify this community plugin. The Mistral name and original mark belong to Mistral. [Brand assets](https://mistral.ai/brand/)
