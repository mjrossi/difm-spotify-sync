package syncer

import "time"

// SetAfter replaces the timer source Loop waits on, so a test can drive
// the ticker without sleeping through a real interval.
func SetAfter(e *Engine, after func(time.Duration) <-chan time.Time) {
	e.after = after
}

// SetNow replaces the clock Loop stamps next_run with.
func SetNow(e *Engine, now func() time.Time) {
	e.now = now
}
