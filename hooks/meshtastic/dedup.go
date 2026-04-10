package meshtastic

import (
	"sync"
	"time"
)

// dedupKey combines a node-from address and a packet ID into a single uint64
// used as a map key, avoiding allocations.
func dedupKey(from, id uint32) uint64 {
	return uint64(from)<<32 | uint64(id)
}

// DedupCache tracks recently seen (From, PacketID) pairs so that duplicate
// publishes from multiple gateway nodes can be detected and dropped.
// All methods are safe for concurrent use.
type DedupCache struct {
	mu      sync.Mutex
	seen    map[uint64]int64 // dedupKey → unix timestamp of first observation
	ttl     time.Duration
	closeCh chan struct{}
}

// NewDedupCache creates a DedupCache with the given window duration.
// It starts a background goroutine that periodically evicts expired entries;
// call Stop to shut it down cleanly.
func NewDedupCache(window time.Duration) *DedupCache {
	d := &DedupCache{
		seen:    make(map[uint64]int64),
		ttl:     window,
		closeCh: make(chan struct{}),
	}
	go d.evictLoop()
	return d
}

// Stop shuts down the background eviction goroutine.
func (d *DedupCache) Stop() {
	close(d.closeCh)
}

// IsDuplicate returns true if this (from, id) pair has been seen within the
// dedup window. If it has not been seen before it is recorded and false is
// returned so the caller can forward the packet.
func (d *DedupCache) IsDuplicate(from, id uint32) bool {
	key := dedupKey(from, id)
	now := time.Now().Unix()

	d.mu.Lock()
	defer d.mu.Unlock()

	if _, exists := d.seen[key]; exists {
		return true
	}

	d.seen[key] = now
	return false
}

// evictLoop runs in a goroutine and removes entries older than the TTL every
// half-TTL interval to bound memory growth.
func (d *DedupCache) evictLoop() {
	ticker := time.NewTicker(d.ttl / 2)
	defer ticker.Stop()

	for {
		select {
		case <-d.closeCh:
			return
		case <-ticker.C:
			d.evict()
		}
	}
}

func (d *DedupCache) evict() {
	cutoff := time.Now().Add(-d.ttl).Unix()

	d.mu.Lock()
	defer d.mu.Unlock()

	for key, ts := range d.seen {
		if ts < cutoff {
			delete(d.seen, key)
		}
	}
}
