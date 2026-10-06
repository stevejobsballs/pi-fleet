// Package hlc implements a hybrid logical clock (DESIGN.md §3.4).
//
// A timestamp packs wall-clock milliseconds into the high 48 bits and a
// logical counter into the low 16. Values from one Clock never go
// backwards, even when the wall clock does, so they give a node's events
// a stable order that still roughly tracks real time.
package hlc

import (
	"sync"
	"time"
)

const counterBits = 16

// Clock issues monotonically increasing HLC timestamps. It is safe for
// concurrent use.
type Clock struct {
	mu   sync.Mutex
	now  func() time.Time
	last int64
}

// New returns a clock reading wall time from now (time.Now if nil) that
// will never issue a value at or below last, typically the highest HLC
// already persisted.
func New(now func() time.Time, last int64) *Clock {
	if now == nil {
		now = time.Now
	}
	return &Clock{now: now, last: last}
}

// Now returns the next timestamp.
func (c *Clock) Now() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	phys := c.now().UnixMilli() << counterBits
	if phys > c.last {
		c.last = phys
	} else {
		c.last++
	}
	return c.last
}

// Observe advances the clock past a timestamp seen from another node so
// that later local events order after it.
func (c *Clock) Observe(remote int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if remote > c.last {
		c.last = remote
	}
}

// Physical returns the wall-clock part of a timestamp.
func Physical(ts int64) time.Time {
	return time.UnixMilli(ts >> counterBits).UTC()
}
