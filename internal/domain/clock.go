package domain

import "time"

// Clock abstracts time.Now so workers and rate limiters can be tested deterministically.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// RealClock returns a Clock backed by time.Now.
func RealClock() Clock { return realClock{} }

// FakeClock is a deterministic clock for tests. NOT goroutine-safe.
type FakeClock struct{ T time.Time }

func (f *FakeClock) Now() time.Time           { return f.T }
func (f *FakeClock) Advance(d time.Duration)  { f.T = f.T.Add(d) }
