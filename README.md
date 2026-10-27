# oc2cpa — OpenCode free models for CLIProxyAPI

[English](README.md) | [简体中文](README.zh-CN.md)

An external [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin
(c-shared library) that turns **OpenCode Zen free models** into a first-class
CLIProxyAPI provider.

The plugin runs inside the CLIProxyAPI host process and talks to the upstream
directly — no standalone proxy process, no extra HTTP hop. It is a native Go
port of the original Node.js [zhuweiyou/oc2api](https://github.com/zhuweiyou/oc2api)
server, renamed and repackaged as **oc2cpa**.

> 📌 OpenCode keeps shipping free models. `deepseek-v4.1-flash-free` and
> `glm-5.3-flash-free` are the latest, both with a 200,000-token context and
> image input. See [Free models](#free-models).

## What the plugin does

- **OpenAI chat-completions in/out.** Because the executor declares
  `chat-completions` as its input/output format, the host automatically
  translates Claude/Gemini-protocol clients into chat-completions before the
  request reaches this plugin.
- **Reserved gate tool.** The free tier requires a `read` tool. The plugin
  injects it, renames client tools into `oc2cpa_*` aliases (with `_1`, `_2`
  suffixes on collision), and restores the original names in responses —
  including names split across stream fragments.
- **Stream-only upstream.** The upstream always streams SSE; the aggregate
  executor reassembles it into one JSON response when the client asked for a
  non-streaming request. Streaming answers are passed through frame by frame,
  with tool-call fragment merging and index normalization.
- **Session affinity.** One synthetic `ses_…` session per auth holder, reused
  for 30 minutes, sent on all `x-opencode-session` family headers.
- **Real error semantics.** Only a true upstream rate limit is `429`
  (`rate_limit_error`); parameter problems stay `400`
  (`invalid_request_error`), auth failures stay `401`/`403`
  (`authentication_error`), and plugin-side stream faults use `502`
  (`upstream_stream_error`). Errors are never masked as fake quotas.
- **Transient-failure retry.** The free tier intermittently answers bare
  `5xx` or transport errors; the plugin retries such pre-content failures
  (3 attempts) before they surface, so a flaky moment does not cool the
  credential down in the host.
- **Watchdog timeouts.** First event within 30 s, body idle within 120 s, and
  a 1 s trailing window after `[DONE]` so late `usage` frames are still
  captured. The connect timeout only bounds dial + response headers — once
  the upstream starts streaming, generation is never cut short.
- **Synthetic auth.** A single `oc2cpa.json` auth file (auto-created on first
  registration) keeps the plugin usable immediately; optionally set a
  `proxy_url` inside it to give that credential its own exit IP. Without it
  the plugin falls back to the host's global `proxy-url`.

## Install

### Plugin store (recommended)

```yaml
plugins:
  enabled: true
  store-sources:
    - https://github.com/dingguotu/oc2cpa/releases/latest/download/registry.json
```

Then install `oc2cpa` from the CLIProxyAPI management UI.

### Manual

```bash
# linux/amd64 example
unzip oc2cpa_<version>_linux_amd64.zip     # contains oc2cpa.so at the zip root
cp oc2cpa.so /path/to/cliproxyapi/plugins/
```

Platform layout also works: `plugins/linux/amd64/oc2cpa.so`. The plugin id is
the library base name (`oc2cpa`).

### Configuration

```yaml
plugins:
  enabled: true
  configs:
    oc2cpa:
      enabled: true
      priority: 100
      upstream_base_url: https://opencode.ai/inference/v1
      connect_timeout_ms: 60000
      dynamic_models: false
```

| Key | Default | Meaning |
| --- | --- | --- |
| `upstream_base_url` | `https://opencode.ai/inference/v1` | Upstream base URL (a local relay can be put here) |
| `connect_timeout_ms` | `60000` | Milliseconds to wait for response headers before giving up (1–600000) |
| `dynamic_models` | `false` | Discover `-free` models from the upstream `/models` endpoint in addition to the built-in catalog |
| `usage_log` | `true` | Emit a per-request forwarding log (`[oc2cpa-usage]`) with model, latency and token usage into the host log |

No API key is needed — the plugin authorizes with the fixed upstream token the
free tier requires.

## Free models

The whitelist keeps `big-pickle` and every model whose id ends with `-free`.
Verified ones:

| Model | Context | Output | Image input |
| --- | --- | --- | --- |
| `big-pickle` | 200,000 | 32,000 | ❌ |
| `mimo-v2.6-flash-free` | 200,000 | 32,000 | ✅ |
| `deepseek-v4.1-flash-free` | 200,000 | 32,000 | ✅ |
| `glm-5.3-flash-free` | 200,000 | 32,000 | ✅ |

Data from [https://models.opencode.ai/api.json](https://models.opencode.ai/api.json)
(`limit` and `modalities`), verify there for new models or set
`dynamic_models: true` to pull them automatically.

## Usage logs

The plugin registers CLIProxyAPI's UsagePlugin capability. The host pushes one
usage record per request back into the plugin, which prints a single
`[oc2cpa-usage]` line into the host log:

```
[oc2cpa-usage] {"time":"2026-10-20T08:36:14+08:00","request_id":"...","provider":"oc2cpa","model":"glm-5.3-flash-free","auth_type":"oauth","api_key":"sk-mE279...","stream":true,"failed":false,"latency_ms":1318,"ttft_ms":1318,"input_tokens":247,"output_tokens":3,"total_tokens":250}
```

It carries the model, stream/failure status, total latency and TTFT, and the
`input / output / reasoning / cached / total` token counters (the client API
key is redacted to a short prefix). Failed requests include the upstream
`status` code instead of token counters. The same metrics are recorded into
CLIProxyAPI's own usage database, so the management UI statistics page shows
them as well. Set `usage_log: false` to silence the console lines.

## Usage panel

The plugin also exposes a small web dashboard through the CLIProxyAPI
management surface:

- Panel page: `/v0/resource/plugins/oc2cpa/panel` (filter bar with time
  range / model / status, aggregate cards, per-model token stats, and the
  recent request list, auto-refreshing every 10 s).
- Data APIs: `GET /v0/management/plugins/oc2cpa/usage/summary` and
  `GET /v0/management/plugins/oc2cpa/usage/records?limit=100` (management
  key required, the page asks for it once and stores it locally). Both
  endpoints accept the same filter parameters:

  | Parameter | Values | Default |
  |---|---|---|
  | `range` | `today` / `7d` / `30d` / `all` | `all` |
  | `model` | exact model id or alias | any model |
  | `status` | `ok` / `failed` / `all` | `all` |
  | `limit` (records only) | number of newest matches | `100` |

The panel statistics come from the plugin's in-memory window (the latest
5000 requests since host start); durable totals stay in CLIProxyAPI's own
usage database. The registered resource also appears as an "OC2CPA"
menu entry (with the OpenCode logo) in the management UI when the frontend
supports plugin resources.

## Development

Requires Go 1.26+. Build the shared library for the local machine:

```bash
go build -buildmode=c-shared -o dist/oc2cpa.dll .        # Windows
# go build -buildmode=c-shared -o dist/oc2cpa.so .        # linux
# go build -buildmode=c-shared -o dist/oc2cpa.dylib .     # darwin
```

Run the offline test suite:

```bash
go test ./...
go vet ./...
```

Cross-platform builds and releases are automated by
[`.github/workflows/build.yml`](.github/workflows/build.yml): a `v*` tag
produces `_linux_amd64`, `_linux_arm64`, `_darwin_amd64`, `_darwin_arm64`,
`_windows_amd64`, and `_windows_arm64` zips plus `checksums.txt` and
`registry.json`.

## History & credits

oc2cpa is a fork of [zhuweiyou/oc2api](https://github.com/zhuweiyou/oc2api) —
originally a standalone Node.js proxy, rewritten here as a pure Go c-shared
CLIProxyAPI plugin, renamed, and maintained in this repository. All credit for
the original protocol work and free-tier reverse engineering goes to
[zhuweiyou](https://github.com/zhuweiyou). Licensed under MIT, see
[LICENSE](LICENSE).
