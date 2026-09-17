package syncer

import "time"

// SetAfter replaces the timer source Loop waits on, so a test can drive
// the ticker without sleeping through a real interval.
func SetAfter(e *Engine, after func(time.Duration) <-chan time.Time) {
	e.after = after
}
