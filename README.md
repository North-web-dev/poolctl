# poolctl

A small daemon + CLI for managing a pool of tokens or accounts: health checks,
rotation, per-token cooldown, optional refresh, and an HTTP API that hands out a
healthy token on demand.

It is generic — a "token" is any opaque string (API key, cookie, OAuth access
token) and validation is whatever you configure (an HTTP request or a shell
command). Point it at a list, tell it how to check one, and clients ask the
daemon for the next usable token instead of juggling the list themselves.

## Why

If you rotate many API keys (scraping, LLM calls), run pools of accounts, or
share cookies across workers, you keep re-implementing the same logic: skip the
rate-limited ones, drop the dead ones, spread load, re-check periodically.
`poolctl` is that logic as one binary with a tiny HTTP API.

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

## HTTP API

| Method | Path       | Body / result |
|--------|------------|---------------|
| GET    | `/take`    | `{"id","token"}`; `503` if none available |
| POST   | `/release` | `{"id","ok":true|false}`; `ok:false` retires the token |
| GET    | `/status`  | counters + per-token `status`/`last_check`/`last_used` |
| POST   | `/reload`  | re-read `tokens_file` |

Set `server.api_key` to require an `X-API-Key` header on every call.

## Config reference

| Key | Default | Description |
|-----|---------|-------------|
| `tokens_file` | — (required) | file with one token per line (`id,token` or `token`) |
| `rotation` | `lru` | `lru`, `round_robin`, or `random` |
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
| `state_file` | `pool_state.json` | persisted per-token health/cooldown |

State (health, cooldown) is persisted to `state_file` and restored on start;
token *values* always come from `tokens_file`, never the state file.

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
