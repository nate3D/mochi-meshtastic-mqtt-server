---
description: "Use when working on deployment, Docker, docker-compose, Traefik routing, environment variables, config.yaml, credentials, the server entrypoint (cmd/meshtastic-mqtt/main.go), or any ops/infrastructure concern for this broker."
applyTo: "{Dockerfile.meshtastic,docker-compose.yml,config.yaml,.env.example,cmd/meshtastic-mqtt/**}"
---

# Deployment and Operations

## Stack

- **Broker**: `cmd/meshtastic-mqtt` binary inside `Dockerfile.meshtastic` (multi-stage, Go builder → Alpine runtime)
- **Reverse proxy**: Traefik (external, already running as a container on the same host)
- **Network**: `servers_net` — macvlan, external, assigns static LAN IPs
- **Broker LAN IP**: `192.168.20.35`

## TLS Architecture

Traefik terminates TLS. The broker never handles certs directly:

```
Meshtastic device / client
    → Traefik :8883 (MQTTS, TLS terminated)
    → broker :1883 (plain TCP)

Phone app / web client
    → Traefik :443 /mqtt (WSS, TLS terminated)
    → broker :1884 (plain WS)
```

Do not add TLS listener config unless the user explicitly wants to bypass Traefik.

## Traefik Labels

The compose file uses two Traefik routers:

| Router | Protocol | Entrypoint | Rule | Backend |
|---|---|---|---|---|
| `mqtt` | TCP | `${TRAEFIK_MQTT_ENTRYPOINT}` (port 8883) | `HostSNI(*)` | `:1883` |
| `mqtt-ws` | HTTP | `websecure` | `Host + PathPrefix(/mqtt)` | `:1884` |

`HostSNI(*)` catches all TCP on that entrypoint — only use this if the entrypoint is dedicated to MQTT.

## Environment Variables

All secrets live in `.env` (gitignored). Never hardcode in `docker-compose.yml` or `config.yaml`.

| Variable | Purpose |
|---|---|
| `BROKER_IPV4` | Static IP on macvlan network |
| `BROKER_USERNAME` | MQTT auth username |
| `BROKER_PASSWORD_HASH` | bcrypt hash (cost 12) of MQTT password |
| `LOG_LEVEL` | `DEBUG` (default), `INFO`, `WARN`, `ERROR` |
| `TRAEFIK_INSTANCE` | Traefik instance label value |
| `TRAEFIK_DOMAIN` | FQDN for TLS cert and routing rule |
| `TRAEFIK_MQTT_ENTRYPOINT` | Traefik entrypoint name for raw MQTT TCP |
| `TRAEFIK_CERT_RESOLVER` | Traefik cert resolver name |

## config.yaml

Bind-mounted read-only at `/app/config.yaml`. Changes take effect on `docker compose restart` — no rebuild needed.

```yaml
tcp_addr: ":1883"
ws_addr:  ":1884"

meshtastic:
  credentials:           # optional; primary credential comes from env vars
    - username: foo
      password_hash: "$2a$12$..."
  channels:
    - name: MediumFast
      psk_base64: AQ==   # [0x01] = well-known default key
  rate_limits:
    packets_per_window: 100
    window_secs: 60
    dedup_window_secs: 60
  require_decryptable: false
  allow_json: false
```

## Credential Management

Passwords are stored as bcrypt hashes only. Never store plaintext.

```bash
# Generate a hash
go run ./cmd/genhash <password>
# Paste output into .env as BROKER_PASSWORD_HASH
```

Primary credential: `BROKER_USERNAME` + `BROKER_PASSWORD_HASH` env vars (loaded in `main.go`).
Additional accounts: add under `credentials:` in `config.yaml`.

## Common Operations

```bash
# Initial deploy
docker compose up -d --build

# Config change only (no rebuild)
docker compose restart meshtastic-mqtt

# Code change
docker compose up -d --build

# Live logs
docker compose logs -f

# Rebuild protobufs after Meshtastic proto schema update
export PATH="$PATH:$(go env GOPATH)/bin"
buf generate
go mod tidy
docker compose up -d --build
```

## Dockerfile Notes

- Builder image: `golang:1.26-alpine` — must match the `go` directive in `go.mod`
- `gen/` is copied before `go mod download` so the replace directive resolves inside the build context
- Runtime image: `alpine:3.21` with `ca-certificates` (needed for outbound TLS if added later)
- Binary: `/app/meshtastic-mqtt`, config expected at `/app/config.yaml`
- When the Go version in `go.mod` changes, update `FROM golang:X.Y-alpine` in `Dockerfile.meshtastic`

## cmd/meshtastic-mqtt/main.go Conventions

- `serverConfig` struct drives YAML parsing — add new top-level config keys here
- `meshtasticConfig` mirrors `meshhook.Config` with YAML tags and base64 PSK strings
- Credentials loaded from env vars take precedence and are appended after YAML credentials
- Log level resolved from `LOG_LEVEL` env var at startup before any config loading
- Listeners only added when their address config field is non-empty
- Graceful shutdown: `signal.Notify` on `SIGINT`/`SIGTERM` → `server.Close()`
