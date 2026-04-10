---
description: "Use when working on the Meshtastic hook: adding new topic types, modifying packet validation, changing ACL rules, updating encryption/decryption logic, adding portnum filters, modifying rate limiting or deduplication, implementing new OnPublish/OnACLCheck/OnConnectAuthenticate behaviour, or extending hooks/meshtastic/ in any way."
applyTo: "hooks/meshtastic/**"
---

# Meshtastic Hook Development

## Package Layout

All Meshtastic logic lives exclusively in `hooks/meshtastic/`. Each file has a single responsibility:

| File | Responsibility |
|---|---|
| `hook.go` | `Hook` struct, `Provides()`, `Init()`, `Stop()`, `OnConnectAuthenticate`, `OnACLCheck`, `OnPublish` |
| `config.go` | `Config`, `ChannelConfig`, `Credential`, `RateLimitConfig` structs |
| `validate.go` | `ParseTopic`, `ParsedTopic`, `IsValidEnvelope`, `IsMeshtasticTopic` |
| `crypto.go` | `TryDecrypt`, `expandPSK`, `buildNonce` |
| `dedup.go` | `DedupCache` — sliding window (From, PacketID) map |
| `ratelimit.go` | `RateLimiter` — per-node sliding window counter |

## Hook Interface Contract

The hook embeds `mqtt.HookBase`. Every hook method handled must be declared in `Provides()`:

```go
func (h *Hook) Provides(b byte) bool {
    return bytes.Contains([]byte{
        mqtt.OnConnectAuthenticate,
        mqtt.OnACLCheck,
        mqtt.OnPublish,
    }, []byte{b})
}
```

Return values from `OnPublish`:
- `return pk, nil` — pass through (forward to subscribers)
- `return pk, packets.ErrRejectPacket` — hard reject (logged as WARN, publisher receives error)
- `return pk, packets.CodeSuccessIgnore` — silent drop (publisher gets success, packet not forwarded)

## Topic Parsing

`ParseTopic` scans for the `/2/` protocol version marker — **do not use positional indexing**. The root is variable-length:

```
msh/US/2/e/MediumFast/!abcd1234           → Root="US"
msh/US/memphismesh.com/2/e/MediumFast/!abcd1234  → Root="US/memphismesh.com"
```

`ParsedTopic.Root` is the full root. Region code = `Root` up to the first `/`.

Valid topic types: `e` (encrypted proto), `c` (legacy encrypted), `json`, `map`.

`json` and `map` topics must be skipped in `OnPublish` — they are not `ServiceEnvelope` payloads.

## OnPublish Pipeline Order

Always maintain this order — each step is a gate:
1. `ParseTopic` — if not a valid msh topic, pass through unchanged
2. Skip `json` / `map` types (not ServiceEnvelope)
3. `proto.Unmarshal` → `ServiceEnvelope` — reject if fails
4. `IsValidEnvelope` — reject if invalid (nil packet, zero ID/From, non-nil Decoded, empty encrypted)
5. `DedupCache.IsDuplicate` — silent drop if seen
6. `RateLimiter.Allow` — silent drop if over limit
7. PSK decrypt (if channel known) → portnum filter

## Envelope Validation Rules

`IsValidEnvelope` enforces:
- `ChannelId` non-empty
- `GatewayId` non-empty
- `Packet` non-nil
- `Packet.Id > 0`
- `Packet.From > 0`
- `Packet.Decoded == nil` (we never accept pre-decrypted payloads from clients)
- `len(Packet.Encrypted) > 0`

## Encryption

`TryDecrypt` uses AES-CTR. Nonce layout (16 bytes):
```
[0:8]  = packetId as uint64 little-endian
[8:16] = from    as uint64 little-endian
```

PSK `[0x01]` (base64 `AQ==`) is the Meshtastic default key sentinel — `expandPSK` converts it to the 16-byte well-known key. Other PSKs must be 16 or 32 bytes (AES-128 or AES-256).

## ACL Rules

`OnACLCheck` (write=true = publish, write=false = subscribe):
- Non-`msh/` topics → WARN + deny
- Malformed msh topics → WARN + deny
- `json` publish while `Config.AllowJSON=false` → WARN + deny
- Region not in `AllowedRegions` (if set) → WARN + deny
- Subscriptions: only `msh/` prefix required, always permit

## Authentication

`OnConnectAuthenticate`: if `Config.Credentials` is empty, all connections permitted. Otherwise username must be in the map and `bcrypt.CompareHashAndPassword` must pass. Failed attempts log at WARN with client ID, username, and remote address.

## Adding a New Topic Type

1. Add the type string to the `ParseTopic` type check in `validate.go`
2. If it is not a `ServiceEnvelope` payload, add it to the skip condition at the top of `OnPublish` in `hook.go`
3. Add handler logic after the skip block if processing is needed
4. Update `IsJSONTopic` or add an equivalent helper if ACL needs special-casing

## Adding a New Config Option

1. Add field to the appropriate struct in `config.go`
2. Read it in `Init()` in `hook.go` — build any lookup maps at init time, not per-packet
3. Add the YAML key to `config.yaml` with a comment
4. If it comes from an env var, add loading logic in `cmd/meshtastic-mqtt/main.go`
