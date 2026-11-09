# budget-guard

Per-client-key budgets for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI), expressed as
**percentages of your subscription pool's limit windows**.

If several machines or CLIs share a pool of subscriptions behind CLIProxyAPI, budget-guard lets you
give each one its own API key and cap it, for example *"the laptop may use 30% of the pool's 5-hour
window and 20% of the weekly one; CI only runs while the pool is under 70%"*. It covers every window
the providers report:

- **Claude** (Claude Code OAuth accounts): 5-hour, weekly, and per-model weekly limits (e.g. `7d:fable`)
- **Codex** (ChatGPT OAuth accounts): 5-hour, weekly, and additional per-model limits

It ships as a native CLIProxyAPI plugin with a web page in the management panel (menu **Budgets**)
where keys, budgets and settings are configured.

## Two kinds of limit

- **own_share**: the key's own usage, as a % of the *whole pool's* window (all accounts of that
  provider, weighted by plan size). This is an estimate, see below.
- **pool_ceiling**: stop serving the key once the pool's *total* usage reaches the %. This is exact:
  it uses the providers' own utilization numbers, including usage outside the proxy.

A key over budget gets an error in its client's own protocol (Anthropic, OpenAI or Gemini error
shape) with a readable message and `Retry-After` set to when the blocking window frees up. Nothing
is sent upstream. Keys without a budget (e.g. your admin key) are never limited.

The reset time is also sent in the form each agent reads, so agents that pause on usage limits
(such as T3 Code) can resume on their own:

- **Claude Code** ignores rate-limit headers unless it's logged in to a subscription. Claude-format
  requests therefore get a gateway-style `503` whose body reads
  `[budget-guard] [429]: {...rate_limit_error...} (reset after 1h2m3s)`, with `X-Should-Retry: false`.
  When the reset time is unknown it's a plain `429`.
- **Codex** gets `429` with error type `usage_limit_reached` and `resets_at`, plus `x-codex-*`
  rate-limit headers showing the blocking window at 100%.

## How own share is estimated

Subscriptions report usage per account, not per request. budget-guard therefore weights each
request's tokens by API list price (only the ratios matter: Opus vs Haiku, output vs cache reads).
It then learns, per account and window, the *capacity*: the API-equivalent cost that fills 100%.

Readings come from the rate-limit headers of every response and from polling each account's usage
endpoint. A calibration segment runs between two "edges" (readings where the whole-number
percentage just rose). Each segment gives `capacity ≥ 100 · proxy_cost / Δutilization`, because usage
outside the proxy can only make the rise bigger. The estimate is the highest of these bounds after
skipping the top fifth. In simulations with 16–33% of usage outside the proxy it lands within ±10%
of the true capacity. Longer windows can also be derived from a calibrated 5-hour window, because
both rise together.

Own share is only **enforced** once capacity is calibrated (3 or more segments). Until then the page
shows "calibrating" and only pool ceilings apply.

A window only limits the pool when every account has it. If an account's usage endpoint stops
reporting a window (a plan without a 5-hour limit, say), the window is dropped from that account
after two polls in a row. While such an account can serve a request, budgets on that window aren't
enforced for it. Once the account is out of another limit that applies (its weekly window at 100%,
say), they are again, until it's back; a refused request's `Retry-After` counts its return. The
status lists these accounts per pool window (`uncapped`, and `uncapped_out` with `uncapped_back_at`
for those out of quota), and the page marks the windows "not enforced" or "enforced for now".

## Install

Requires CLIProxyAPI v8 with plugin support. It was built against v8.0.48, plugin C ABI v1, RPC
schema v9.

```sh
git clone https://github.com/rodrigohpalmeirim/cpa-budget-guard && cd cpa-budget-guard
./build.sh /path/to/cliproxyapi/plugins/linux/amd64   # tests, builds in Docker, installs
```

`build.sh` builds `dist/budget-guard.so` inside `golang:1.26-bookworm`, which matches the official
`eceasy/cli-proxy-api` image (Debian bookworm, glibc, linux/amd64). For another platform, build with
`CGO_ENABLED=1 go build -buildmode=c-shared -o budget-guard.so .` on a matching system.

Enable plugins in CLIProxyAPI's `config.yaml` and restart:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    budget-guard:
      enabled: true
      priority: 100
      state_file: "/CLIProxyAPI/plugins/budget-guard-state.json"  # calibration + accounting
```

Then open the management panel → **Budgets**, and add a key per machine. The page writes through
CLIProxyAPI's v8 management API (`/v8/management/config/plugins/configs/budget-guard` and
`/v8/management/config/access/api-keys`) and the plugin reloads automatically. Its **Connect** dialog
gives ready-to-paste settings for Claude Code, Codex CLI and OpenAI-compatible clients.

### Configuring by hand

```yaml
    budget-guard:
      # ...
      poll_interval: 3m          # per-account usage endpoint polling
      request_log_days: 90       # per-request log next to the state file; -1 disables
      calibration_step: 3        # % rise that closes a calibration segment
      keys:
        - name: laptop
          key: "sk-cpa-..."      # must also be listed in access.api-keys
          budgets:
            claude:
              own_share:    {5h: 30, 7d: 20, "7d:fable": 10}
              pool_ceiling: {"*": 90}   # "*" = every window without an explicit value
            codex:
              own_share: {"*": 50}
      pricing:                   # optional weight overrides, first match wins (USD per 1M tokens)
        - {provider: claude, match: opus, input: 5, output: 25, cache_read: 0.5, cache_write: 6.25}
```

`cache_write` prices 5-minute cache writes. 1-hour writes (`cache_write_1h`, default twice `input`) are
told apart only when CLIProxyAPI reports the TTL split to plugins (`UsageDetail.CacheCreation1hTokens`);
on older versions every write is priced as 5-minute.

## Status API

- `GET /v0/management/budget-guard/status`: JSON with every key's standing per window, every account's
  windows, and calibration state. Each account window also lists the budgeted keys' cost in its current
  period (`key_cost_usd`), which is what its reset frees up. Requires the management key.
- `GET /v0/management/budget-guard/stats?period=1h|5h|24h|7d|30d|90d`: the request log summarised (see above).
- `GET /v0/management/budget-guard/costs?period=24h|7d|30d|90d&step=300|900|3600`: the spend in the
  request log, for charting. `rows` hold one time bucket and combination of key, model, account and
  agent (main thread or subagent) each, with tokens and cost per billing type (input, output, cache
  reads, cache writes, 1-hour cache writes) and what the requests would have cost with no caching;
  `columns` names the fields. Also the previous period's totals per key, and the costliest
  conversations (a session with the subagents it started, from the hashed session ids).
- `/v0/resource/plugins/budget-guard/status`: the web page. It's static HTML; CLIProxyAPI serves
  plugin resources without authentication, so the page fetches all data from the endpoint above.

## Request log and usage stats

Every request through the proxy is appended to a JSON-lines log next to the state file
(`budget-guard-requests/requests-YYYY-MM-DD.jsonl`, one file per UTC day, kept for
`request_log_days`, default 90; `-1` turns it off). A line holds the key name, account, model, token
counts (input, output, cache reads and writes), API-equivalent cost, and for each limit window the
utilization the response headers reported and the capacity estimate the request was booked against.
It holds no prompts, responses or keys. Expect a few hundred bytes per request.

The **Usage** section of the page summarises it for a period (1h to 30d), per key, account and model:

- **Cache hit**: the share of prompt tokens read from cache. If conversations hop between accounts
  (broken session affinity), the hit rate drops and the write share rises.
- **Estimate check**: for each account window, the rise the capacity estimate predicts from the
  proxy's own requests, against the rise the provider reported over the same requests. Usage outside
  the proxy only adds to the reported rise, so up to 100% is expected; well above 100% means the
  capacity is underestimated.

The same data is at `GET /v0/management/budget-guard/stats?period=24h` (management key).

## Usage limits in T3 Code

T3 Code can show a CLIProxyAPI pool's limits (Settings → Providers → CLI Proxy → Add hub). It calls
`GET /v0/management/auth-files` and `POST /v0/management/api-call`, which need the management key, and
that key can read and change everything, including the accounts' tokens. budget-guard answers the
same two calls for a budgeted client key instead:

- `GET /v0/management/budget-guard/hub/auth-files`: the Claude and Codex accounts (id, auth_index,
  provider, email, ChatGPT account ID).
- `POST /v0/management/budget-guard/hub/api-call`: the provider usage endpoints only (Claude
  `/api/oauth/usage`, Codex `/wham/usage` and rate-limit reset credits). Any other URL gets 403.

These are management routes, so the request still needs the management key. Put a reverse proxy in
front that injects it and passes the client's own `Authorization` header in `X-Budget-Guard-Key`, which
the plugin checks against the budgeted keys. For nginx, place this before any other management
location:

```nginx
location ~ ^/v0/management/(?<hub>auth-files|api-call)$ {
  proxy_set_header Authorization "Bearer <management key>";
  proxy_set_header X-Budget-Guard-Key $http_authorization;
  proxy_pass http://cliproxyapi:8317/v0/management/budget-guard/hub/$hub;
}
```

In T3, set the hub URL to the proxy's public address and enter the machine's client key in the
**Management key** field. If the management panel is served from the same host, keep its own
`auth-files` calls on the real endpoint, e.g. by routing on a session cookie.

## Layout

| Path | Contents |
| --- | --- |
| `main.go` | C ABI glue and host callbacks |
| `plugin.go` | RPC handlers: request interceptor, usage hook, management routes, usage poller |
| `hub.go` | Usage-limit endpoints for T3 Code, authenticated by a client key |
| `guard/` | Windows, calibration, pricing, enforcement and the request log: pure Go with unit tests |
| `status.html` | The embedded web page; it uses the management panel's design tokens and follows its theme |
| `e2e/ui_test.py` | Playwright test that drives the page against a live proxy |

## Caveats

- Own share is an estimate (typically within ±10% once calibrated) and needs some traffic through
  the proxy before it is enforced.
- The plugin interface changes between CLIProxyAPI versions. After upgrading, check
  `docker logs <container> | grep budget-guard` and rebuild if the plugin failed to load.
- Using subscription accounts through a proxy may be against your provider's terms. That's your
  call; this plugin only limits usage.

## License

MIT
