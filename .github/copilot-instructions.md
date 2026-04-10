# Meshtastic MQTT Broker — Project Guidelines

## What This Project Is

This is a fork of [mochi-mqtt/server](https://github.com/mochi-mqtt/server) — a high-performance, fully MQTT v5 compliant broker written in Go. This fork extends mochi-mqtt with a **Meshtastic-specific hook** that makes the broker natively aware of the Meshtastic mesh networking protocol, serving as a drop-in replacement for the official `meshtastic/mqtt` C# boilerplate.

The goal is to build a production-quality, self-hosted Meshtastic MQTT broker with Go and best practices: packet validation, PSK-based decryption, deduplication, rate limiting, per-node access control, and structured observability — all as a composable mochi-mqtt hook.

## Repository Structure

```
/                           # mochi-mqtt/server upstream code (DO NOT refactor)
├── hooks/meshtastic/       # Meshtastic hook — the primary focus of this fork
│   ├── hook.go             # Hook entry point: OnConnectAuthenticate, OnACLCheck, OnPublish
│   ├── config.go           # Config, ChannelConfig, Credential, RateLimitConfig structs
│   ├── validate.go         # ParseTopic (handles variable-length root topics), IsValidEnvelope
│   ├── crypto.go           # TryDecrypt: AES-CTR with nonce=(packetId LE ∥ from LE), PSK expansion
│   ├── dedup.go            # DedupCache: (From, PacketID) sliding window, prevents multi-gateway dupes
│   └── ratelimit.go        # RateLimiter: per-node sliding window packet counter
├── cmd/meshtastic-mqtt/    # Server entrypoint for the Meshtastic broker
│   └── main.go             # Config loading, hook wiring, TCP/TLS/WS listeners, graceful shutdown
├── cmd/genhash/            # CLI utility: bcrypt hash generator for config credentials
├── gen/                    # Generated Go protobuf bindings from buf.build/meshtastic/protobufs
│   ├── go.mod              # module github.com/meshtastic/go/generated
│   └── *.pb.go             # All Meshtastic protobuf types (ServiceEnvelope, MeshPacket, Data, PortNum, etc.)
├── buf.gen.yaml            # buf generate config — regenerate with: buf generate
├── config.yaml             # Runtime config (mounted into container, not baked in)
├── Dockerfile.meshtastic   # Multi-stage Docker build for cmd/meshtastic-mqtt
└── docker-compose.yml      # Production deployment with Traefik labels
```

## Key Architecture Decisions

### Meshtastic Protocol Facts
- **Topic format**: `msh/<ROOT>/2/e/<CHANNEL>/<NODEID>` where ROOT can be multi-segment (e.g. `msh/US/memphismesh.com/2/e/LongFast/!abcd1234`)
- **Topic types**: `e` (encrypted protobuf), `c` (legacy pre-2.3 encrypted), `json` (plaintext JSON), `map` (MapReport proto)
- **Payload**: `ServiceEnvelope` protobuf wrapping an encrypted `MeshPacket`; `Decoded` field must always be nil on inbound publishes
- **Encryption**: AES-128/256-CTR; nonce = `packetId` (uint64 LE) ∥ `from` (uint64 LE); PSK `[0x01]` expands to the well-known 16-byte default key
- **Deduplication**: Multiple gateway nodes may relay the same packet; identify by `(MeshPacket.From, MeshPacket.Id)`

### Hook Pipeline (OnPublish order)
1. Parse and validate topic with `ParseTopic` (scans for `/2/` marker, not positional)
2. Skip non-ServiceEnvelope types (`json`, `map`)
3. Unmarshal `ServiceEnvelope`, call `IsValidEnvelope`
4. Deduplicate via `DedupCache`
5. Rate-limit via `RateLimiter`
6. If channel PSK is known: `TryDecrypt` → parse `Data` → portnum filter
7. Return `packets.ErrRejectPacket` (hard reject) or `packets.CodeSuccessIgnore` (silent drop)

### Credential Security
- Passwords stored as **bcrypt hashes** (cost 12) only — never plaintext
- Primary credential injected via `BROKER_USERNAME` / `BROKER_PASSWORD_HASH` env vars
- Additional static accounts can be listed in `config.yaml` under `credentials:`
- Generate a hash: `go run ./cmd/genhash <password>`

### Protobuf Generation
- No official Go bindings exist from Meshtastic — `go_package = github.com/meshtastic/go/generated` is declared in the .proto files but the module is not published
- Workaround: `buf generate` pulls from `buf.build/meshtastic/protobufs` → local `gen/` directory
- `go.mod` uses a `replace` directive: `github.com/meshtastic/go/generated => ./gen`
- To regenerate after proto schema updates: `buf generate` (requires `buf` CLI and `protoc-gen-go`)

## Build and Test

```bash
# Build all packages
go build ./...

# Run all tests
go test ./...

# Generate a credential hash
go run ./cmd/genhash <password>

# Regenerate protobuf bindings
export PATH="$PATH:$(go env GOPATH)/bin"
buf generate

# Run locally (uses config.yaml in cwd)
go run ./cmd/meshtastic-mqtt

# Docker
docker compose up -d --build
docker compose logs -f
```

## Deployment

- Containerised via `Dockerfile.meshtastic`, orchestrated by `docker-compose.yml`
- TLS terminated by **Traefik** upstream; broker runs plain TCP on `:1883` and plain WS on `:1884`
- macvlan network `servers_net` assigns a static LAN IP (`192.168.20.35`)
- All secrets (`BROKER_PASSWORD_HASH`, etc.) live in `.env` (gitignored); see `.env.example`
- `config.yaml` is bind-mounted at runtime — changes take effect on `docker compose restart`, no rebuild needed
- Log level controlled by `LOG_LEVEL` env var: `DEBUG` (default), `INFO`, `WARN`, `ERROR`

## Conventions

- **Upstream files are not modified** — all Meshtastic functionality is additive, living entirely in `hooks/meshtastic/` and `cmd/meshtastic-mqtt/`
- Hook returns `packets.ErrRejectPacket` for hard failures (malformed payload, auth); `packets.CodeSuccessIgnore` for policy drops (dedup, rate limit, portnum filter)
- ACL violations log at `WARN`; debug-level tracing at `DEBUG`
- `ParsedTopic.Root` contains the full variable root (e.g. `US/memphismesh.com`); region code is the first segment before any `/`
- All new Meshtastic topic types discovered in production should be added to `ParseTopic` and the `OnPublish` skip list before adding handler logic

## Upstream Relationship

This fork tracks `mochi-mqtt/server` main. When pulling upstream changes:
- Never modify files outside `hooks/meshtastic/`, `cmd/meshtastic-mqtt/`, `cmd/genhash/`, `gen/`, `buf.gen.yaml`, `Dockerfile.meshtastic`, `docker-compose.yml`, `config.yaml`, `.env*`
- The `go.mod` replace directive and the `google.golang.org/protobuf` version bump must be preserved after any upstream merge
