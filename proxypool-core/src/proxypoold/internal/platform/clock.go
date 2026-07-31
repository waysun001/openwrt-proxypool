package platform

import "time"

// Clock allows the engine to use deterministic time in tests without putting
// test-only lifecycle methods into production components.
type Clock interface {
	Now() time.Time
	Monotonic() time.Duration
	NewTimer(time.Duration) Timer
}

type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

type RealClock struct{}

var realClockEpoch = time.Now()

func (RealClock) Now() time.Time {
	return time.Now()
}

// Monotonic returns process-local elapsed time. realClockEpoch contains Go's
// monotonic component, so wall-clock corrections cannot move this value.
func (RealClock) Monotonic() time.Duration {
	return time.Since(realClockEpoch)
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
