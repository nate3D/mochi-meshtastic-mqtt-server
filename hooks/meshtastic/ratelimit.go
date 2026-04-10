package meshtastic

import (
	"sync"
	"time"
)

// nodeState holds the sliding-window packet timestamps for a single node.
type nodeState struct {
	mu         sync.Mutex
	timestamps []int64 // unix seconds of recent packets, oldest first
}

// RateLimiter enforces a per-node sliding window rate limit.
// All methods are safe for concurrent use.
type RateLimiter struct {
	nodes  sync.Map // uint32 (From) → *nodeState
	limit  int      // max packets per window
	window time.Duration
}

// NewRateLimiter creates a RateLimiter that allows at most limit packets per
// window for each unique node (identified by the From field).
// A limit of zero disables rate limiting (Allow always returns true).
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		limit:  limit,
		window: window,
	}
}

// Allow returns true if the node identified by fromNode is within its rate
// limit and records the current time as a new event.
// If the limiter was created with limit==0, Allow always returns true.
func (r *RateLimiter) Allow(fromNode uint32) bool {
	if r.limit == 0 {
		return true
	}

	v, _ := r.nodes.LoadOrStore(fromNode, &nodeState{})
	ns := v.(*nodeState)

	cutoff := time.Now().Add(-r.window).Unix()
	now := time.Now().Unix()

	ns.mu.Lock()
	defer ns.mu.Unlock()

	// Evict timestamps outside the window.
	i := 0
	for i < len(ns.timestamps) && ns.timestamps[i] < cutoff {
		i++
	}
	ns.timestamps = ns.timestamps[i:]

	if len(ns.timestamps) >= r.limit {
		return false
	}

	ns.timestamps = append(ns.timestamps, now)
	return true
}
