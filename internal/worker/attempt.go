package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"syscall"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/remote"
	"github.com/Topher-the-Geek/vamos/internal/run"
	"github.com/Topher-the-Geek/vamos/internal/sched"
	"github.com/Topher-the-Geek/vamos/internal/tracing"
)

// The pace of an attempt's reporting.
const (
	// eventBatch is how long output is gathered before it is sent, so that a chatty step doesn't make a request
	// per line.
	eventBatch = 200 * time.Millisecond
	// fenceFraction is the part of the lease after which an attempt whose lease couldn't be renewed is killed,
	// so that it has stopped before the lease expires at the coordinator, measured on this machine's monotonic clock.
	fenceFraction = 0.8
	// killWait is how long a killed attempt's processes have to exit before the worker gives up waiting.
	killWait = 30 * time.Second
)

// attempt runs the assignment a: a subprocess in a cgroup of its own, whose output and spans are sent as events,
// whose lease is renewed by heartbeat, and whose ending is reported.
// Once ctx ends, the coordinator asks for a kill, or the lease can't be renewed in time, everything in the cgroup is
// killed, the attempt's container removed, and nothing reported: the coordinator has stopped counting the attempt.
func (w *Worker) attempt(ctx context.Context, reg remote.Registration, a *remote.Assignment, assigned time.Time) {
	id := a.Attempt
	outcome, err := w.execute(ctx, reg, a, assigned)
	if err != nil {
		w.logf("attempt %s: %v", id, err)
	}
	if err := w.contain.kill(id, killWait); err != nil {
		w.logf("attempt %s: %v", id, err)
	}
	if err := run.RemoveAttempt(context.WithoutCancel(ctx), w.cfg.Store, id); err != nil {
		w.logf("attempt %s: removing its container: %v", id, err)
	}
	if outcome == nil {
		return // killed, or lost: there is nothing to report, and no one to report it to
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reg.Lease)
	defer cancel()
	if err := w.client.Report(rctx, id, *outcome); err != nil && !errors.Is(err, remote.ErrStaleAttempt) {
		w.logf("attempt %s: reporting: %v", id, err)
	}
}

// execute runs the subprocess of a and returns what it reported, or nil if it was killed or never started because
// its lease couldn't be renewed; assigned is when the assignment arrived.
func (w *Worker) execute(ctx context.Context, reg remote.Registration, a *remote.Assignment, assigned time.Time) (
	*remote.AttemptReport, error) {
	cgroup, err := w.contain.create(a.Attempt)
	if err != nil {
		return failed(err), nil
	}
	defer cgroup.Close()
	input, err := json.Marshal(run.AttemptInput{
		Attempt:         a.Attempt,
		Request:         a.Request,
		Push:            a.Push,
		Coordinator:     w.cfg.Coordinator,
		CacheRepository: reg.CacheRepository,
		PullThrough:     reg.PullThrough,
		Trace:           a.Trace,
	})
	if err != nil {
		return failed(err), nil
	}
	// The coordinator started the lease before sending the assignment, which may have been delayed for any part
	// of it: nothing starts until a renewal has been answered, whose send the fence then counts from.
	renewed, clock, err := w.renewFirst(ctx, reg, a.Attempt, assigned)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := w.cfg.Child(cctx)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stderr = w.cfg.Log
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cgroup.Fd())}
	cmd.Cancel = func() error { return w.contain.kill(a.Attempt, killWait) }
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return failed(err), nil
	}
	if err := cmd.Start(); err != nil {
		return failed(fmt.Errorf("starting the attempt: %w", err)), nil
	}

	// The lease is renewed until the subprocess ends; a kill from the coordinator, or a lease that can't be
	// renewed in time, ends it.
	killed := make(chan error, 1)
	beat, stopBeat := context.WithCancel(cctx)
	go w.heartbeat(beat, reg, a.Attempt, renewed, clock, func(why error) {
		killed <- why
		cancel()
	})

	// What the subprocess prints is sent in batches, and its last line is its report.
	report, readErr := w.relay(cctx, a.Attempt, stdout)
	waitErr := cmd.Wait()
	stopBeat()
	select {
	case why := <-killed:
		return nil, why
	default:
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	switch {
	case report != nil:
		return report, nil
	case readErr != nil:
		return failed(fmt.Errorf("reading the attempt's output: %w", readErr)), nil
	case waitErr != nil:
		return failed(fmt.Errorf("the attempt's process ended without a report: %w", waitErr)), nil
	}
	return failed(errors.New("the attempt's process ended without a report")), nil
}

// failed returns a report of a failure that is the worker's, not the action's.
func failed(err error) *remote.AttemptReport {
	return &remote.AttemptReport{Failure: sched.FailureOf(err)}
}

// renewFirst renews the lease of the attempt id before its subprocess starts, and returns when the renewing request
// was sent, from which the fence counts, and the estimate of the clock's offset its answer gives.
// The coordinator started the first lease on assigning the attempt, which may have reached this worker late, so
// assigned, when it arrived, bounds nothing that runs; only a renewal's send, which the coordinator received later,
// does. A request that gets no answer is sent again every heartbeat interval until a lease has passed since assigned,
// by when the coordinator has lost the attempt; an answer that the attempt is stale, or asks for the kill, ends it.
func (w *Worker) renewFirst(ctx context.Context, reg remote.Registration, id string, assigned time.Time) (
	time.Time, *tracing.Offset, error) {
	for {
		hctx, cancel := context.WithTimeout(ctx, reg.Heartbeat)
		sent := time.Now()
		a, err := w.client.Heartbeat(hctx, id, remote.Heartbeat{})
		received := time.Now()
		cancel()
		switch {
		case err == nil && a.Kill:
			return time.Time{}, nil, errors.New("the coordinator asked for the attempt to be killed before it started")
		case err == nil:
			return sent, estimateOffset(sent, received, a.Clock, nil), nil
		case ctx.Err() != nil:
			return time.Time{}, nil, ctx.Err()
		case !remote.IsTransport(err):
			return time.Time{}, nil, fmt.Errorf("the coordinator no longer counts the attempt: %w", err)
		case time.Since(assigned) >= reg.Lease:
			return time.Time{}, nil, fmt.Errorf("the lease couldn't be renewed before the attempt started: %w", err)
		}
		select {
		case <-time.After(time.Until(sent.Add(reg.Heartbeat))):
		case <-ctx.Done():
			return time.Time{}, nil, ctx.Err()
		}
	}
}

// heartbeat renews the attempt's lease every heartbeat interval until ctx ends, and calls kill, once, when the
// coordinator asks for a kill or counts the attempt stale, or when fenceFraction of the lease has passed since the
// last renewal, whatever a request still in flight might yet answer, so that the attempt has stopped before its lease
// expires at the coordinator. A request that gets no answer within a heartbeat interval is given up; the next
// interval sends another while the fence hasn't passed.
// The coordinator renews on receiving a request, at some point after it was sent, so the fence counts from the send,
// not from the answer, which a slow reply would push past the lease; renewed is the send of the renewal renewFirst
// made, and clock its estimate of the clock's offset.
// Each answer's clock refines the estimate of how far this clock is ahead of the coordinator's, which the next
// heartbeat carries, so that the coordinator shifts the attempt's spans onto its own clock.
func (w *Worker) heartbeat(ctx context.Context, reg remote.Registration, id string, renewed time.Time,
	clock *tracing.Offset, kill func(error)) {
	ticker := time.NewTicker(reg.Heartbeat)
	defer ticker.Stop()
	fence := time.Duration(float64(reg.Lease) * fenceFraction)
	fencer := time.NewTimer(time.Until(renewed.Add(fence))) // on this machine's monotonic clock
	defer fencer.Stop()
	type answer struct {
		sent, received time.Time
		answer         remote.HeartbeatAnswer
		err            error
	}
	var inFlight chan answer
	var lastErr error
	for {
		select {
		case <-ctx.Done():
			return
		case <-fencer.C:
			why := "no answer came"
			if lastErr != nil {
				why = lastErr.Error()
			}
			kill(fmt.Errorf("the lease couldn't be renewed for %s: %s", fence.Round(time.Second), why))
			return
		case <-ticker.C:
			if inFlight != nil {
				continue // the last request hasn't answered: the fence decides
			}
			inFlight = make(chan answer, 1)
			go func(ch chan<- answer, clock *tracing.Offset) {
				hctx, cancel := context.WithTimeout(ctx, reg.Heartbeat)
				defer cancel()
				sent := time.Now()
				a, err := w.client.Heartbeat(hctx, id, remote.Heartbeat{Clock: clock})
				ch <- answer{sent: sent, received: time.Now(), answer: a, err: err}
			}(inFlight, clock)
		case ans := <-inFlight:
			inFlight = nil
			switch {
			case ans.err == nil && ans.answer.Kill:
				kill(errors.New("the coordinator asked for the attempt to be killed"))
				return
			case ans.err == nil:
				// A timer that has fired is drained first, so that an answer that came after the fence doesn't
				// leave its expiry behind; a fence already passed fires at once.
				if !fencer.Stop() {
					select {
					case <-fencer.C:
					default:
					}
				}
				fencer.Reset(max(time.Until(ans.sent.Add(fence)), 0))
				clock = estimateOffset(ans.sent, ans.received, ans.answer.Clock, clock)
			case ctx.Err() != nil:
				return
			case !remote.IsTransport(ans.err):
				kill(fmt.Errorf("the coordinator no longer counts the attempt: %w", ans.err))
				return
			default:
				lastErr = ans.err
			}
		}
	}
}

// estimateOffset returns the better of best and the estimate one heartbeat gives of how far this clock is ahead of
// the coordinator's: a request sent at sent, answered with the coordinator's clock reading remote, whose answer came
// at received, puts the coordinator's clock at remote − (sent + received)/2 ahead of this one's, give or take half
// the round trip, and the estimate with the smaller uncertainty is kept.
func estimateOffset(sent, received, remote time.Time, best *tracing.Offset) *tracing.Offset {
	if remote.IsZero() {
		return best
	}
	roundTrip := received.Sub(sent)
	estimate := &tracing.Offset{Offset: -remote.Sub(sent.Add(roundTrip / 2)), Uncertainty: roundTrip / 2}
	if best != nil && best.Uncertainty <= estimate.Uncertainty {
		return best
	}
	return estimate
}

// The bounds of an attempt's events as they cross in JSON, where output is base64.
const (
	// maxBatch is the most one batch of events may hold, encoded, well within what the transport takes.
	maxBatch = 4 << 20
	// maxChunk is the most bytes of output one event carries, so that a long write splits across batches.
	maxChunk = 1 << 20
	// maxPending is the most that waits to be sent, encoded: beyond it the subprocess's output isn't read, so that
	// its writes block while the coordinator is slow, rather than the worker's memory growing.
	maxPending = 4 * maxBatch
)

// relay reads the subprocess's lines from r: output and spans, which it sends to the coordinator in batches of at
// most maxBatch, and the report, which it returns, once the stream ends. It stops reading while maxPending waits.
func (w *Worker) relay(ctx context.Context, id string, r io.Reader) (*remote.AttemptReport, error) {
	q := newEventQueue()
	send := func() {
		for {
			batch, ok := q.take()
			if !ok {
				return
			}
			sctx, cancel := context.WithTimeout(ctx, w.lease())
			err := w.client.Events(sctx, id, batch)
			cancel()
			if err != nil && ctx.Err() == nil {
				w.logf("attempt %s: sending events: %v", id, err)
			}
		}
	}
	ticker := time.NewTicker(eventBatch)
	defer ticker.Stop()
	stop := make(chan struct{})
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		for {
			select {
			case <-ticker.C:
				send()
			case <-stop:
				send()
				return
			}
		}
	}()
	var report *remote.AttemptReport
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), run.MaxLineSize)
	for scanner.Scan() {
		var line run.AttemptLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			w.logf("attempt %s printed a line that isn't one of its own: %q", id, scanner.Bytes())
			continue
		}
		switch {
		case line.Output != nil:
			q.addOutput(line.Output.Stream, line.Output.Data)
		case len(line.Spans) > 0:
			q.addSpans(line.Spans)
		case line.Report != nil:
			report = line.Report
		}
	}
	close(stop)
	<-flushed
	return report, scanner.Err()
}

// eventQueue holds an attempt's events until they are sent, in the order they came.
type eventQueue struct {
	mu     sync.Mutex
	taken  *sync.Cond // signalled when a batch is taken, making room
	events []queued
	size   int // the encoded size of events
}

// queued is one event waiting, output or a span, and its encoded size.
type queued struct {
	output *remote.OutputEvent
	span   *tracing.Span
	size   int
}

// newEventQueue returns an empty queue.
func newEventQueue() *eventQueue {
	q := &eventQueue{}
	q.taken = sync.NewCond(&q.mu)
	return q
}

// addOutput queues a write of data to stream, in events of at most maxChunk bytes.
func (q *eventQueue) addOutput(stream byte, data []byte) {
	var events []queued
	for len(data) > 0 {
		chunk := data[:min(len(data), maxChunk)]
		data = data[len(chunk):]
		events = append(events, queued{output: &remote.OutputEvent{Stream: stream, Data: chunk},
			size: base64.StdEncoding.EncodedLen(len(chunk)) + 32})
	}
	q.add(events)
}

// addSpans queues spans.
func (q *eventQueue) addSpans(spans []tracing.Span) {
	events := make([]queued, len(spans))
	for i := range spans {
		data, _ := json.Marshal(spans[i]) // decoded from JSON, so it encodes
		events[i] = queued{span: &spans[i], size: len(data) + 1}
	}
	q.add(events)
}

// add queues events once fewer than maxPending bytes wait.
func (q *eventQueue) add(events []queued) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.size >= maxPending {
		q.taken.Wait()
	}
	for _, e := range events {
		q.events = append(q.events, e)
		q.size += e.size
	}
}

// take removes the oldest events, up to maxBatch encoded, or one if it alone is larger, and reports whether there
// were any.
func (q *eventQueue) take() (remote.AttemptEvents, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var batch remote.AttemptEvents
	n, size := 0, 0
	for n < len(q.events) && (n == 0 || size+q.events[n].size <= maxBatch) {
		e := q.events[n]
		if e.output != nil {
			batch.Output = append(batch.Output, *e.output)
		} else {
			batch.Spans = append(batch.Spans, *e.span)
		}
		size += e.size
		n++
	}
	clear(q.events[:n]) // so that what was sent can be collected
	q.events = q.events[n:]
	q.size -= size
	q.taken.Broadcast()
	return batch, n > 0
}
