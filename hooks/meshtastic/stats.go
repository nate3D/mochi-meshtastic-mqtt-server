package meshtastic

import (
	"sync"
	"sync/atomic"
	"time"
)

// Stats tracks runtime metrics for the Meshtastic hook.
// All methods are safe for concurrent use.
type Stats struct {
	startTime time.Time

	// Packet pipeline counters — incremented by OnPublish.
	PacketsReceived        atomic.Int64 // Valid ServiceEnvelope packets accepted
	PacketsForwarded       atomic.Int64 // Packets that passed all filters
	PacketsDeduplicated    atomic.Int64 // Silent-dropped as duplicates
	PacketsRateLimited     atomic.Int64 // Silent-dropped by rate limiter
	PacketsRejected        atomic.Int64 // Hard-rejected (malformed / invalid envelope)
	PacketsPortnumFiltered atomic.Int64 // Silent-dropped by portnum filter
	MapReportsReceived     atomic.Int64 // map-type topic publishes

	// Connection counters — incremented by OnConnect / OnDisconnect.
	TotalConnections   atomic.Int64 // All-time MQTT connections
	CurrentConnections atomic.Int64 // Currently active MQTT connections

	// Unique-set tracking needs a mutex because map writes are not atomic.
	mu             sync.RWMutex
	uniqueNodes    map[uint32]struct{} // From node IDs seen in packet headers
	uniqueGateways map[string]struct{} // GatewayId strings seen in ServiceEnvelopes
}

// StatsSnapshot is a JSON-serializable point-in-time view of Stats.
type StatsSnapshot struct {
	UptimeSeconds          float64 `json:"uptime_seconds"`
	TotalConnections       int64   `json:"total_connections"`
	CurrentConnections     int64   `json:"current_connections"`
	UniqueNodes            int     `json:"unique_nodes"`
	UniqueGateways         int     `json:"unique_gateways"`
	PacketsReceived        int64   `json:"packets_received"`
	PacketsForwarded       int64   `json:"packets_forwarded"`
	PacketsDeduplicated    int64   `json:"packets_deduplicated"`
	PacketsRateLimited     int64   `json:"packets_rate_limited"`
	PacketsRejected        int64   `json:"packets_rejected"`
	PacketsPortnumFiltered int64   `json:"packets_portnum_filtered"`
	MapReportsReceived     int64   `json:"map_reports_received"`
}

func newStats() *Stats {
	return &Stats{
		startTime:      time.Now(),
		uniqueNodes:    make(map[uint32]struct{}),
		uniqueGateways: make(map[string]struct{}),
	}
}

func (s *Stats) recordNode(from uint32) {
	s.mu.Lock()
	s.uniqueNodes[from] = struct{}{}
	s.mu.Unlock()
}

func (s *Stats) recordGateway(gw string) {
	if gw == "" {
		return
	}
	s.mu.Lock()
	s.uniqueGateways[gw] = struct{}{}
	s.mu.Unlock()
}

// Snapshot returns a consistent point-in-time copy of all stats.
func (s *Stats) Snapshot() StatsSnapshot {
	s.mu.RLock()
	nodes := len(s.uniqueNodes)
	gateways := len(s.uniqueGateways)
	s.mu.RUnlock()

	return StatsSnapshot{
		UptimeSeconds:          time.Since(s.startTime).Seconds(),
		TotalConnections:       s.TotalConnections.Load(),
		CurrentConnections:     s.CurrentConnections.Load(),
		UniqueNodes:            nodes,
		UniqueGateways:         gateways,
		PacketsReceived:        s.PacketsReceived.Load(),
		PacketsForwarded:       s.PacketsForwarded.Load(),
		PacketsDeduplicated:    s.PacketsDeduplicated.Load(),
		PacketsRateLimited:     s.PacketsRateLimited.Load(),
		PacketsRejected:        s.PacketsRejected.Load(),
		PacketsPortnumFiltered: s.PacketsPortnumFiltered.Load(),
		MapReportsReceived:     s.MapReportsReceived.Load(),
	}
}
