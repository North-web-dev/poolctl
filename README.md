# poolctl

[![CI](https://github.com/North-web-dev/poolctl/actions/workflows/ci.yml/badge.svg)](https://github.com/North-web-dev/poolctl/actions/workflows/ci.yml) [![Go Report Card](https://goreportcard.com/badge/github.com/North-web-dev/poolctl)](https://goreportcard.com/report/github.com/North-web-dev/poolctl) [![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE) [![Release](https://img.shields.io/github/v/release/North-web-dev/poolctl?sort=semver)](https://github.com/North-web-dev/poolctl/releases)


A small daemon + CLI for managing a pool of tokens or accounts: health checks,
rotation, per-token cooldown, optional refresh, and an HTTP API that hands out a
healthy token on demand. It can also run as a **passthrough reverse proxy** that
injects a pooled key into every request and rotates keys on `429`/`401`
— point an OpenAI/Anthropic SDK at it and you get a load-balancing, self-healing
key gateway with no client changes.

It is generic — a "token" is any opaque string (API key, cookie, OAuth access
token) and validation is whatever you configure (an HTTP request or a shell
command). Point it at a list, tell it how to check one, and clients ask the
daemon for the next usable token instead of juggling the list themselves.

## Why

If you rotate many API keys (scraping, LLM calls), run pools of accounts, or
share cookies across workers, you keep re-implementing the same logic: skip the
rate-limited ones, drop the dead ones, spread load, re-check periodically.
`poolctl` is that logic as one binary — use it two ways:

- **Broker** (`poolctl serve`): clients `GET /take` a healthy token and
  `POST /release` it, reporting success or failure.
- **Gateway** (`poolctl proxy`): clients speak the upstream API directly and
  `poolctl` injects the key, forwards, and retries with a different key on a
  rate-limit or auth failure — transparent to the client.

## Install

```sh
go install github.com/North-web-dev/poolctl@latest
# or
git clone https://github.com/North-web-dev/poolctl && cd poolctl && go build -o poolctl .
```

## Quickstart

`tokens.txt` — one token per line (`id,token` or just `token`):

```
key1,sk-aaaa
key2,sk-bbbb
sk-cccc
```

`pool.yaml`:

```yaml
tokens_file: tokens.txt
rotation: lru
cooldown_sec: 30
check:
  type: http
  url: "https://api.example.com/me"
  headers: { Authorization: "Bearer {token}" }
  success_status: [200]
server:
  addr: ":8787"
```

Run the daemon and take a token:

```sh
poolctl serve -c pool.yaml
curl -s localhost:8787/take          # {"id":"key1","token":"sk-aaaa"}
```

Report success/failure so the pool can cool down or retire it:

```sh
curl -s localhost:8787/release -d '{"id":"key1","ok":true}'
curl -s localhost:8787/release -d '{"id":"key2","ok":false}'   # -> marked dead
```

One-shot check of the whole list without running the daemon:

```sh
poolctl check -c pool.yaml
```

## LLM / API-gateway mode

Add an `upstream` block and run `poolctl proxy`. Every incoming request borrows
a healthy token, has it injected as a header, and is forwarded to `base_url`;
the response is streamed straight back (SSE token-by-token). If the upstream
returns a retryable status the request is retried with a *different* token:
`429`/`5xx` cools the key down briefly, `401`/`403` quarantines it until a
health check revives it.

```yaml
tokens_file: keys.txt          # one API key per line, optional ,weight
rotation: weighted             # spread load by each key's quota
cooldown_sec: 20
check:
  type: http
  url: "https://api.openai.com/v1/models"
  headers: { Authorization: "Bearer {token}" }
  success_status: [200]
upstream:
  enabled: true
  listen: ":8080"
  base_url: "https://api.openai.com"
  auth_header: "Authorization"
  auth_template: "Bearer {token}"
  retry_on: [401, 403, 429, 500, 502, 503, 504]
  max_retries: 2
  quarantine_sec: 300
metrics:
  addr: ":9090"                # Prometheus scrape endpoint
```

```sh
poolctl proxy -c pool.yaml
# point any client at the proxy — no API key needed client-side:
curl -s localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'
```

```python
# OpenAI SDK — just change base_url; the pooled key is injected upstream.
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="unused")
```

In proxy mode the control API (`/take`, `/status`, `/reload`) still runs on
`server.addr`, so you can watch the pool while it serves traffic.

## HTTP API

| Method | Path       | Body / result |
|--------|------------|---------------|
| GET    | `/take`    | `{"id","token"}`; `503` if none available |
| POST   | `/release` | `{"id","ok":true|false}`; `ok:false` retires the token |
| GET    | `/status`  | counters + per-token `status`/`last_check`/`last_used`/`requests`/`errors` |
| POST   | `/reload`  | re-read `tokens_file` |
| GET    | `/metrics` | Prometheus text (tokens by status, request/error/retry counters) |

Set `server.api_key` to require an `X-API-Key` header on every call. `/metrics`
is always unauthenticated so a scraper can reach it.

## Config reference

| Key | Default | Description |
|-----|---------|-------------|
| `tokens_file` | — (required) | file with one token per line (`id,token` or `token`) |
| `rotation` | `lru` | `lru`, `round_robin`, `random`, or `weighted` |
| `cooldown_sec` | `30` | rest period after a token is taken and after a failure |
| `recheck_interval_sec` | `300` | background health re-check interval |
| `check.type` | `http` | `http` or `command` |
| `check.url` / `method` / `headers` | — | HTTP check; `{token}` is substituted |
| `check.success_status` | `[200]` | status codes that count as healthy |
| `check.cmd` / `success_output` | — | command check; healthy if stdout contains `success_output` |
| `refresh.enabled` | `false` | try refreshing a token when its check fails |
| `refresh.url` / `method` / `headers` / `body` | — | refresh request; `{token}` substituted |
| `refresh.token_field` | `access_token` | JSON field with the new token |
| `proxy` | — | proxy URL for http checks |
| `server.addr` | `:8787` | daemon listen address |
| `server.api_key` | — | if set, required in `X-API-Key` |
| `upstream.enabled` | `false` | enable the passthrough proxy (`poolctl proxy`) |
| `upstream.listen` | `:8080` | proxy listen address |
| `upstream.base_url` | — | upstream to forward to, e.g. `https://api.openai.com` |
| `upstream.auth_header` / `auth_template` | `Authorization` / `Bearer {token}` | how the token is injected |
| `upstream.retry_on` | `[401,403,429,500,502,503,504]` | upstream statuses that trigger a retry |
| `upstream.max_retries` | `2` | extra attempts (each with a different token) |
| `upstream.quarantine_sec` | `300` | rest after a `401`/`403` (hard) failure |
| `upstream.cooldown_sec` | `cooldown_sec` | rest after a `429`/`5xx` (soft) failure |
| `upstream.timeout_sec` | `60` | per-attempt time-to-first-byte timeout (streaming is unbounded) |
| `metrics.addr` | — | if set, a dedicated Prometheus `/metrics` listener |
| `state_file` | `pool_state.json` | persisted per-token health/cooldown |

For `weighted` rotation, give each token a third CSV field (`id,token,weight`);
higher-weight keys receive proportionally more traffic. A missing weight is `1`.

State (health, cooldown, per-token request/error counts) is persisted to
`state_file` and restored on start; token *values* always come from
`tokens_file`, never the state file.

## Command checks

For services without a simple HTTP probe, validate with any command:

```yaml
check:
  type: command
  cmd: "curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer {token}' https://api.example.com/me"
  success_output: "200"
```

## License

MIT

## Disclaimer

Provided **as is, without warranty of any kind**. `poolctl` only manages
credentials you supply and runs the health checks you configure. You are
responsible for holding those credentials lawfully and for complying with the
Terms of Service of any endpoint you check against. The authors accept no
liability for misuse.
