package supervisor

import "time"

type clock interface {
	Now() time.Time
	NewTimer(time.Duration) timer
	NewTicker(time.Duration) ticker
}

type timer interface {
	C() <-chan time.Time
	Stop()
}

type ticker interface {
	C() <-chan time.Time
	Stop()
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(duration time.Duration) timer {
	return realTimer{Timer: time.NewTimer(duration)}
}

func (realClock) NewTicker(duration time.Duration) ticker {
	return realTicker{Ticker: time.NewTicker(duration)}
}

type realTimer struct{ *time.Timer }

func (timer realTimer) C() <-chan time.Time { return timer.Timer.C }
func (timer realTimer) Stop()               { timer.Timer.Stop() }

type realTicker struct{ *time.Ticker }

func (ticker realTicker) C() <-chan time.Time { return ticker.Ticker.C }
func (ticker realTicker) Stop()               { ticker.Ticker.Stop() }
