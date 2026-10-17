package tracing

import (
	"context"
	"time"
)

// Offset is how far another process's clock is ahead of this one's,
// as estimated from the round trips of calls asking it the time.
// The true offset is within Uncertainty of Offset.
type Offset struct {
	Offset      time.Duration
	Uncertainty time.Duration
}

// clockSamples is how many times EstimateOffset asks for the time.
// The shortest round trip of them gives the estimate,
// since the others' delays are less likely to be even both ways.
const clockSamples = 3

// EstimateOffset estimates how far the clock that remote reads is ahead of the clock now reads, as NTP does.
// A call sent at t0, answered with t1, and whose answer came at t3,
// puts the offset at t1 − (t0 + t3)/2, give or take half the round trip.
func EstimateOffset(ctx context.Context, now func() time.Time, remote func(context.Context) (time.Time, error)) (
	Offset, error) {
	var best Offset
	for i := range clockSamples {
		t0 := now()
		t1, err := remote(ctx)
		if err != nil {
			return Offset{}, err
		}
		t3 := now()
		roundTrip := t3.Sub(t0)
		if i > 0 && roundTrip/2 >= best.Uncertainty {
			continue
		}
		mid := t0.Add(roundTrip / 2)
		best = Offset{Offset: t1.Sub(mid), Uncertainty: roundTrip / 2}
	}
	return best, nil
}

// Shift returns spans, from a process whose clock is offset ahead of this one's,
// as this process's clock times them.
func Shift(spans []Span, offset Offset) []Span {
	shifted := make([]Span, len(spans))
	for i, s := range spans {
		s.Start = s.Start.Add(-offset.Offset)
		s.End = s.End.Add(-offset.Offset)
		shifted[i] = s
	}
	return shifted
}
