package hlc

import (
	"testing"
	"time"
)

func TestMonotonicWhenWallClockGoesBackwards(t *testing.T) {
	wall := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	c := New(func() time.Time { return wall }, 0)

	a := c.Now()
	b := c.Now() // same millisecond: counter increments
	wall = wall.Add(-time.Hour)
	d := c.Now() // wall clock stepped back: still increases

	if !(a < b && b < d) {
		t.Fatalf("not monotonic: %d %d %d", a, b, d)
	}
	if got := Physical(a); !got.Equal(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("Physical = %v", got)
	}
}

func TestStartsAfterPersistedValue(t *testing.T) {
	wall := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	persisted := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli() << counterBits
	c := New(func() time.Time { return wall }, persisted)
	if got := c.Now(); got <= persisted {
		t.Fatalf("Now() = %d, want > %d", got, persisted)
	}
}

func TestObserve(t *testing.T) {
	wall := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := New(func() time.Time { return wall }, 0)
	remote := wall.Add(time.Minute).UnixMilli() << counterBits
	c.Observe(remote)
	if got := c.Now(); got <= remote {
		t.Fatalf("Now() after Observe = %d, want > %d", got, remote)
	}
}
