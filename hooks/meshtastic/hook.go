package meshtastic

import (
	"bytes"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"
	generated "github.com/meshtastic/go/generated"
	"google.golang.org/protobuf/proto"
)

const hookID = "meshtastic"

// Hook is a mochi-mqtt Hook that provides Meshtastic-aware packet validation,
// decryption checking, deduplication, per-node rate limiting, and
// username/password authentication.
//
// Attach it to a server before calling Serve():
//
//	server.AddHook(&meshtastic.Hook{}, &meshtastic.Config{...})
type Hook struct {
	mqtt.HookBase
	config  *Config
	creds   map[string][]byte // username → bcrypt hash bytes
	pskMap  map[string][]byte // channel name → expanded PSK bytes
	dedup   *DedupCache
	limiter *RateLimiter
	blocked map[int32]struct{}
	allowed map[int32]struct{}
	regions map[string]struct{}
}

// ID returns the unique identifier for this hook.
func (h *Hook) ID() string {
	return hookID
}

// Provides declares which hook events this hook handles.
func (h *Hook) Provides(b byte) bool {
	return bytes.Contains([]byte{
		mqtt.OnConnectAuthenticate,
		mqtt.OnACLCheck,
		mqtt.OnPublish,
	}, []byte{b})
}

// Init configures the hook from Config. Called automatically by AddHook.
func (h *Hook) Init(config any) error {
	if _, ok := config.(*Config); !ok && config != nil {
		return mqtt.ErrInvalidConfigType
	}

	if config == nil {
		config = new(Config)
	}

	h.config = config.(*Config)

	// Build username → bcrypt hash lookup table.
	h.creds = make(map[string][]byte, len(h.config.Credentials))
	for _, c := range h.config.Credentials {
		h.creds[c.Username] = []byte(c.PasswordHash)
	}

	// Build channel name → PSK lookup table.
	h.pskMap = make(map[string][]byte, len(h.config.Channels))
	for _, ch := range h.config.Channels {
		key, err := expandPSK(ch.PSK)
		if err != nil {
			return err
		}
		h.pskMap[ch.Name] = key
	}

	// Deduplication cache.
	dedupWindow := 60 * time.Second
	if h.config.RateLimits.DedupWindowSecs > 0 {
		dedupWindow = time.Duration(h.config.RateLimits.DedupWindowSecs) * time.Second
	}
	h.dedup = NewDedupCache(dedupWindow)

	// Per-node rate limiter.
	rWindow := 60 * time.Second
	if h.config.RateLimits.WindowSecs > 0 {
		rWindow = time.Duration(h.config.RateLimits.WindowSecs) * time.Second
	}
	h.limiter = NewRateLimiter(h.config.RateLimits.PacketsPerWindow, rWindow)

	// Build portnum sets for O(1) lookup.
	h.blocked = make(map[int32]struct{}, len(h.config.BlockedPortNums))
	for _, p := range h.config.BlockedPortNums {
		h.blocked[p] = struct{}{}
	}
	h.allowed = make(map[int32]struct{}, len(h.config.AllowedPortNums))
	for _, p := range h.config.AllowedPortNums {
		h.allowed[p] = struct{}{}
	}

	// Build region allowlist set.
	h.regions = make(map[string]struct{}, len(h.config.AllowedRegions))
	for _, r := range h.config.AllowedRegions {
		h.regions[r] = struct{}{}
	}

	authMode := "open (no credentials set)"
	if len(h.creds) > 0 {
		authMode = "password required"
	}
	h.Log.Info("meshtastic hook initialised",
		"auth", authMode,
		"channels", len(h.config.Channels),
		"require_decryptable", h.config.RequireDecryptable,
		"dedup_window_secs", dedupWindow.Seconds(),
		"rate_limit", h.config.RateLimits.PacketsPerWindow,
	)

	return nil
}

// Stop cleans up the background dedup eviction goroutine.
func (h *Hook) Stop() error {
	if h.dedup != nil {
		h.dedup.Stop()
	}
	return nil
}

// OnConnectAuthenticate returns true if the connecting client supplies a
// username and password that match a configured credential.
// If no credentials are configured, all connections are accepted.
func (h *Hook) OnConnectAuthenticate(cl *mqtt.Client, pk packets.Packet) bool {
	if len(h.creds) == 0 {
		return true
	}

	username := string(pk.Connect.Username)
	hash, ok := h.creds[username]
	if !ok {
		h.Log.Warn("auth: unknown username",
			"client", cl.ID, "username", username, "remote", cl.Net.Remote)
		return false
	}

	if err := bcrypt.CompareHashAndPassword(hash, pk.Connect.Password); err != nil {
		h.Log.Warn("auth: wrong password",
			"client", cl.ID, "username", username, "remote", cl.Net.Remote)
		return false
	}

	return true
}

// OnACLCheck enforces Meshtastic-specific topic access rules:
//
//   - Only topics under "msh/" are permitted.
//   - JSON publish paths (msh/+/2/json/#) are blocked unless Config.AllowJSON is set.
//   - Region allowlist is enforced for writes.
//
// Subscriptions to "msh/#" and higher-level wildcards are always permitted.
func (h *Hook) OnACLCheck(cl *mqtt.Client, topic string, write bool) bool {
	if !IsMeshtasticTopic(topic) {
		h.Log.Warn("ACL: rejected non-msh topic", "client", cl.ID, "topic", topic)
		return false
	}

	// Subscriptions only need to match the msh/ prefix.
	if !write {
		return true
	}

	// For publishes: validate topic structure first.
	pt, reason := ParseTopic(topic)
	if reason != "" {
		h.Log.Warn("ACL: rejected malformed publish topic",
			"client", cl.ID, "topic", topic, "reason", reason)
		return false
	}

	// Block JSON publishes unless explicitly allowed.
	if (pt.Type == "json") && !h.config.AllowJSON {
		h.Log.Warn("ACL: rejected JSON publish",
			"client", cl.ID, "topic", topic)
		return false
	}

// Enforce region allowlist (compare only the first segment of the root,
	// e.g. "US" from "US/memphismesh.com").
	if len(h.regions) > 0 {
		regionCode := pt.Root
		if i := strings.Index(pt.Root, "/"); i >= 0 {
			regionCode = pt.Root[:i]
		}
		if _, ok := h.regions[regionCode]; !ok {
			h.Log.Warn("ACL: rejected disallowed region",
				"client", cl.ID, "topic", topic, "region", regionCode)
			return false
		}
	}

	return true
}

// OnPublish intercepts every outbound publish packet and applies Meshtastic
// packet validation, deduplication, rate limiting, and portnum filtering.
//
// The order of checks is:
//  1. Topic must be a valid Meshtastic protobuf topic (e / c type).
//  2. Payload must deserialise as a ServiceEnvelope with valid fields.
//  3. The packet must not already have been seen (deduplication).
//  4. The sending node must not exceed the configured rate limit.
//  5. If the channel has a configured PSK, the packet must be decryptable
//     (only when Config.RequireDecryptable is true).
//  6. If the decrypted portnum is in BlockedPortNums, the packet is dropped.
//  7. If AllowedPortNums is non-empty and the portnum is not in the list, drop.
func (h *Hook) OnPublish(cl *mqtt.Client, pk packets.Packet) (packets.Packet, error) {
	topic := pk.TopicName

	// Only intercept Meshtastic protobuf topics; pass JSON and other topics through.
	pt, reason := ParseTopic(topic)
	if reason != "" {
		// Not a parseable Meshtastic topic — defer to ACL; don't block here.
		return pk, nil
	}

	if pt.Type == "json" || pt.Type == "map" {
		// JSON and map report topics are not ServiceEnvelope protobufs; skip envelope validation.
		return pk, nil
	}

	// Deserialise ServiceEnvelope.
	var env generated.ServiceEnvelope
	if err := proto.Unmarshal(pk.Payload, &env); err != nil {
		h.Log.Warn("rejected: failed to parse ServiceEnvelope",
			"client", cl.ID, "topic", topic, "error", err)
		return pk, packets.ErrRejectPacket
	}

	if !IsValidEnvelope(&env) {
		h.Log.Warn("rejected: invalid ServiceEnvelope",
			"client", cl.ID, "topic", topic,
			"channel_id", env.GetChannelId(),
			"gateway_id", env.GetGatewayId())
		return pk, packets.ErrRejectPacket
	}

	meshPkt := env.GetPacket()
	from := meshPkt.GetFrom()
	pktID := meshPkt.GetId()

	// Deduplication: drop packets seen recently from another gateway.
	if h.dedup.IsDuplicate(from, pktID) {
		h.Log.Debug("dropped: duplicate packet",
			"from", from, "id", pktID, "topic", topic)
		return pk, packets.CodeSuccessIgnore
	}

	// Per-node rate limiting.
	if !h.limiter.Allow(from) {
		h.Log.Warn("dropped: node exceeded rate limit",
			"from", from, "topic", topic)
		return pk, packets.CodeSuccessIgnore
	}

	// Channel PSK decryption/validation.
	psk, channelKnown := h.pskMap[pt.Channel]
	if channelKnown {
		plaintext, err := TryDecrypt(meshPkt, psk)
		if err != nil {
			if h.config.RequireDecryptable {
				h.Log.Warn("rejected: packet not decryptable with known PSK",
					"client", cl.ID, "topic", topic, "channel", pt.Channel)
				return pk, packets.ErrRejectPacket
			}
			// Decryption failed but we're not enforcing it; forward anyway.
			return pk, nil
		}

		// Parse the decrypted Data payload for portnum filtering.
		var data generated.Data
		if err := proto.Unmarshal(plaintext, &data); err == nil {
			if drop, reason := h.filterPortnum(data.GetPortnum()); drop {
				h.Log.Debug("dropped: portnum filtered",
					"portnum", data.GetPortnum(), "reason", reason,
					"from", from, "topic", topic)
				return pk, packets.CodeSuccessIgnore
			}
		}
	}

	return pk, nil
}

// filterPortnum returns (true, reason) if the packet should be dropped based
// on the blocked/allowed portnum lists.
func (h *Hook) filterPortnum(portnum generated.PortNum) (bool, string) {
	pn := int32(portnum)

	if _, blocked := h.blocked[pn]; blocked {
		return true, "portnum in blocklist"
	}

	if len(h.allowed) > 0 {
		if _, ok := h.allowed[pn]; !ok {
			return true, "portnum not in allowlist"
		}
	}

	return false, ""
}
