# Mistral Bridge

CLIProxyAPI native plugin for adapting chat-completion requests to the Mistral API. The plugin includes Mistral API-key management, model discovery and aliases, reasoning-level mapping, and metadata-only request logs.

## Build

Build the shared library for the Linux architecture used by the CPA container. The plugin needs to be compiled for the same operating system and architecture as CPA.

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=1 go build -buildmode=c-shared -o mistral-bridge.so .
```

Copy `mistral-bridge.so` into the CPA plugin directory, then enable global plugin loading and this plugin in `config.yaml`:

```yaml
plugins:
  enabled: true
  configs:
    mistral-bridge:
      enabled: true
```

The CPA build must support native plugins and schema version 10. Restart CPA after installing or replacing the shared library.

## Use

Open the **Mistral Bridge** resource from the CPA or CPAMP panel. An embedded page reuses the saved same-origin panel session. When opened directly, enter the current panel's management key once for the browser session, then add a Mistral API key and pull the model catalog. Add or edit the mappings you want to publish. Clients use the configured request model name directly:

```text
<client-model-name>
```

The panel refreshes CPA’s model registration after mapping changes and confirms the published revision. Catalog pulls update the available upstream choices. Credentials use the highest enabled priority, with round-robin selection within that priority. An explicit credential binding takes precedence.

Credential cards query Mistral’s account and billing endpoints through CPA’s configured HTTP proxy. API keys show the API budget and Vibe keys show the Vibe budget. Budgets are shared by the organization. Unavailable quotas show the upstream error and the timestamp of the last successful result.

The request adapter accepts the CPA chat-completions executor format. It moves assistant `reasoning_content`, `reasoning`, and `reasoning_details` into Mistral typed thinking parts, drops blank reasoning placeholders, maps `reasoning_effort`, and converts Mistral thinking/text response parts to OpenAI-compatible content and reasoning fields. Streaming deltas are normalized as they arrive.

The plugin stores its state under CPA's configured auth directory at `plugin-data/mistral-bridge/state.json`. When that directory is unavailable, it uses `$HOME/.cli-proxy-api/plugin-data/mistral-bridge/state.json`. Set `MISTRAL_BRIDGE_DATA_DIR` to choose a different directory. The file contains upstream API keys and is created with owner-only permissions. Mount its parent directory on persistent storage when using Docker.

Request logs retain the most recent 10,000 requests with request/upstream model names, mapped reasoning effort, masked credential identity, result, time to first streaming output, duration, and token counts. Search, filters, time ranges, statistics and pagination run on the server. Cache hit rate uses requests with upstream cache data. Token speed divides successful streaming output tokens by generation time after the first token. Historical entries with missing fields display an em dash. The panel follows CPAMP’s light and dark theme selection.

## Verify

Run the focused protocol tests and build the plugin for the deployment target:

```sh
go test ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=1 go build -buildmode=c-shared -o mistral-bridge.so .
```
