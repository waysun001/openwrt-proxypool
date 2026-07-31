package platform

import "time"

// Clock allows the engine to use deterministic time in tests without putting
// test-only lifecycle methods into production components.
type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}

type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

type RealClock struct{}

func (RealClock) Now() time.Time {
	return time.Now()
}

func (RealClock) NewTimer(delay time.Duration) Timer {
	return realTimer{timer: time.NewTimer(delay)}
}

type realTimer struct {
	timer *time.Timer
}

func (timer realTimer) C() <-chan time.Time {
	return timer.timer.C
}

func (timer realTimer) Stop() bool {
	return timer.timer.Stop()
}
