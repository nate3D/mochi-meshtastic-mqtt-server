---
description: "Use when working with Meshtastic protobuf types, regenerating Go bindings from the BSR, adding new proto-derived types to the hook, understanding ServiceEnvelope/MeshPacket/Data/PortNum structures, or modifying gen/ or buf.gen.yaml."
applyTo: "{gen/**,buf.gen.yaml}"
---

# Meshtastic Protobuf Bindings

## Why a Local gen/ Directory

Meshtastic declares `go_package = "github.com/meshtastic/go/generated"` in their `.proto` files but **does not publish a Go module** at that path. The official `buf.gen.yaml` in `meshtastic/protobufs` only generates TypeScript.

Workaround:
1. `buf generate` pulls from `buf.build/meshtastic/protobufs` (BSR) using `buf.gen.yaml` at the repo root
2. Output lands in `gen/` as `package generated`
3. `gen/go.mod` declares `module github.com/meshtastic/go/generated`
4. The root `go.mod` has: `replace github.com/meshtastic/go/generated => ./gen`

## Regenerating

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
# Install tools if needed:
go install github.com/bufbuild/buf/cmd/buf@latest
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest

buf generate          # pulls from BSR, writes gen/*.pb.go
cd gen && go mod tidy # update gen/go.sum
cd .. && go mod tidy  # update root go.sum
```

## buf.gen.yaml

```yaml
version: v2
inputs:
  - module: buf.build/meshtastic/protobufs
plugins:
  - remote: buf.build/protocolbuffers/go
    out: gen
    opt:
      - module=github.com/meshtastic/go/generated
```

The `module=` opt strips the `meshtastic/` path prefix from output filenames, placing all `.pb.go` files flat in `gen/`.

## Key Types (all in package `generated`)

### ServiceEnvelope (`gen/mqtt.pb.go`)
Top-level MQTT payload for `e` and `c` topic types.
```go
type ServiceEnvelope struct {
    Packet    *MeshPacket
    ChannelId string      // channel name, e.g. "MediumFast"
    GatewayId string      // sender node hex ID, e.g. "!abcd1234"
}
```

### MeshPacket (`gen/mesh.pb.go`)
```go
type MeshPacket struct {
    From           uint32                      // sender node number
    To             uint32                      // destination (0xFFFFFFFF = broadcast)
    Id             uint32                      // packet ID (unique per From)
    PayloadVariant isMeshPacket_PayloadVariant // oneof: Decoded *Data | Encrypted []byte
    HopLimit       uint32
    HopStart       uint32
    // ... other fields
}
// Accessors:
pk.GetFrom()      uint32
pk.GetId()        uint32
pk.GetDecoded()   *Data    // nil if encrypted
pk.GetEncrypted() []byte   // nil if decoded
```

### Data (`gen/mesh.pb.go`)
The decrypted inner payload, obtained after `TryDecrypt` + `proto.Unmarshal`.
```go
type Data struct {
    Portnum PortNum // application layer port
    Payload []byte  // application-specific bytes
}
```

### PortNum (`gen/portnums.pb.go`)
```go
const (
    PortNum_UNKNOWN_APP                PortNum = 0
    PortNum_TEXT_MESSAGE_APP           PortNum = 1
    PortNum_POSITION_APP               PortNum = 3
    PortNum_NODEINFO_APP               PortNum = 4
    PortNum_ROUTING_APP                PortNum = 5
    PortNum_TELEMETRY_APP              PortNum = 67
    PortNum_MAP_REPORT_APP             PortNum = 73
    // ... many more
)
```

### MapReport (`gen/mqtt.pb.go`)
Published to `msh/<ROOT>/2/map/` topics (not wrapped in ServiceEnvelope).
```go
type MapReport struct {
    LongName         string
    ShortName        string
    HwModel          HardwareModel
    FirmwareVersion  string
    Region           Config_LoRaConfig_RegionCode
    HasDefaultChannel bool
    LatitudeI        int32   // multiply by 1e-7 for degrees
    LongitudeI       int32
    NumOnlineLocalNodes uint32
    // ...
}
```

## Import Path

Always import as:
```go
import generated "github.com/meshtastic/go/generated"
```

and use `proto.Unmarshal` from `google.golang.org/protobuf/proto`.

## go.mod Gotcha

The `gen/go.mod` `go` directive and root `go.mod` `go` directive must both be compatible with the Docker builder image version. When `go mod tidy` bumps either directive, update `FROM golang:X.Y-alpine` in `Dockerfile.meshtastic` to match.
