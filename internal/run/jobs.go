package run

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/tracing"
)

// defaultRetention is how many of a job's events the executor keeps for watchers, by default:
// progress events are small, and a watcher that falls this far behind has stopped reading.
// Command output is in the log blobs too, so a gap loses no information that mattered.
const defaultRetention = 4096

// jobTable holds the jobs of a local executor, in memory, for as long as it lives.
type jobTable struct {
	retention int // events kept per job; defaultRetention if 0

	mu        sync.Mutex
	jobs      map[api.JobID]*job
	running   sync.WaitGroup // the jobs that haven't ended
	active    int            // how many jobs haven't ended
	exclusive *job           // the job that must run alone, a prune, while it runs
	closed    bool           // stop has been called: no job starts
}

// The errors that starting a job can meet.
var (
	errClosed  = errors.New("the executor has shut down")
	errPruning = errors.New("a prune is running on this executor; submit again when it has ended")
)

// job is one job of the executor: the events it retains, for watchers to read from any position they hold,
// and the context that cancels it.
// Events are appended under mu; changed is closed and replaced whenever one is, so that watchers can wait for it.
// The job retains the last retention events: older positions are expired, and a watch from one meets a gap.
type job struct {
	cancel    context.CancelFunc
	retention int

	mu      sync.Mutex
	first   int64       // the position of events[0]
	events  []api.Event // the retained events, the last of them the terminal event once done
	changed chan struct{}
	done    bool
	outcome api.Outcome // once done
}

// start adds a job that runs work in a goroutine of its own, with a context that only Cancel ends,
// and returns its handle.
// work reports its progress to the events it is given, and its outcome ends the job.
// An exclusive job, a prune, is refused while any job runs, and no job starts while one runs or after stop,
// all decided under one lock, so that a prune and another job never overlap on this executor.
func (t *jobTable) start(exclusive bool, work func(ctx context.Context, ev Events) api.Outcome) (api.JobID, error) {
	var b [8]byte
	rand.Read(b[:])
	id := api.JobID(hex.EncodeToString(b[:]))
	ctx, cancel := context.WithCancel(context.Background())
	retention := t.retention
	if retention <= 0 {
		retention = defaultRetention
	}
	j := &job{cancel: cancel, retention: retention, changed: make(chan struct{})}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.closed:
		cancel()
		return "", errClosed
	case t.exclusive != nil:
		cancel()
		return "", errPruning
	case exclusive && t.active > 0:
		cancel()
		return "", fmt.Errorf("%d other jobs are running on this executor; prune when they have ended", t.active)
	}
	if t.jobs == nil {
		t.jobs = map[api.JobID]*job{}
	}
	t.jobs[id] = j
	if exclusive {
		t.exclusive = j
	}
	t.running.Add(1)
	t.active++
	go func() {
		defer t.running.Done()
		defer cancel()
		outcome := work(ctx, jobEvents{j})
		t.mu.Lock()
		j.finish(outcome)
		t.active--
		if t.exclusive == j {
			t.exclusive = nil
		}
		t.mu.Unlock()
	}()
	return id, nil
}

// stop cancels every job that hasn't ended, refuses any more, and waits for them all to end,
// so that their cleanup has run.
func (t *jobTable) stop() {
	t.mu.Lock()
	t.closed = true
	for _, j := range t.jobs {
		j.cancel()
	}
	t.mu.Unlock()
	t.running.Wait()
}

// lookup returns the job id, or ErrNoJob.
func (t *jobTable) lookup(id api.JobID) (*job, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	j, ok := t.jobs[id]
	if !ok {
		return nil, api.ErrNoJob
	}
	return j, nil
}

// add appends e to the job's events, giving it its position, and wakes watchers,
// dropping the oldest event if the job retains as many as it keeps.
// An event after the terminal one is dropped: nothing may follow it, and a shared attempt that outlives the job
// which started it keeps reporting to it.
func (j *job) add(e api.Event) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.append(e)
}

// append appends e under the lock, as add does.
func (j *job) append(e api.Event) {
	if j.done {
		return
	}
	e.Position = j.first + int64(len(j.events))
	if len(j.events) == j.retention {
		j.events = j.events[1:]
		j.first++
	}
	j.events = append(j.events, e)
	close(j.changed)
	j.changed = make(chan struct{})
}

// finish ends the job with outcome, as its terminal event: the event is appended and the job is done in one step,
// so that a watcher that saw the event finds the outcome.
func (j *job) finish(outcome api.Outcome) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.append(api.Event{Kind: api.EventDone, Outcome: &outcome})
	j.done, j.outcome = true, outcome
}

// at returns the event at position, if the job retains it, and otherwise whether the job has ended,
// the first position retained, and a channel that closes when the job has more events.
func (j *job) at(position int64) (e api.Event, ok, done bool, first int64, changed <-chan struct{}) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if position >= j.first && position < j.first+int64(len(j.events)) {
		return j.events[position-j.first], true, j.done, j.first, j.changed
	}
	return api.Event{}, false, j.done, j.first, j.changed
}

// result returns the job's outcome, or ErrRunning if it hasn't ended.
func (j *job) result() (api.Outcome, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.done {
		return api.Outcome{}, api.ErrRunning
	}
	return j.outcome.Clone(), nil
}

// watch delivers the job's events from position from to fn, as Executor.Watch describes:
// a position already expired gets a gap event first, and a watcher that falls behind the retained events
// as it reads is dropped with ErrFellBehind.
// A watcher that has just been given a gap, and whose first retained position expired meanwhile, gets another gap
// rather than being dropped: it has read nothing yet.
// Each event is a copy, which fn may keep or change.
// The end of ctx is noticed before each event, not only while waiting for one,
// so that a watcher behind a job that has many events stops when asked.
func (j *job) watch(ctx context.Context, from int64, fn func(api.Event) error) error {
	if from < 0 {
		return fmt.Errorf("position %d is before the first event", from)
	}
	delivered := false // an event other than a gap, since which falling behind is the watcher's
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		e, ok, done, first, changed := j.at(from)
		switch {
		case ok:
			if err := fn(e.Clone()); err != nil {
				return err
			}
			if e.Kind == api.EventDone {
				return nil
			}
			delivered = true
			from++
		case from < first && delivered:
			return fmt.Errorf("%w: the events from position %d are no longer retained; the first is %d",
				api.ErrFellBehind, from, first)
		case from < first:
			if err := fn(api.Event{Position: from, Kind: api.EventGap, First: first}); err != nil {
				return err
			}
			from = first
		case done:
			return nil // from is past the terminal event
		default:
			select {
			case <-changed:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// jobEvents sends what the runner reports to a job's events.
type jobEvents struct {
	j *job
}

// Status reports status as an event about node.
func (ev jobEvents) Status(node graph.Ref, status string) {
	ev.j.add(api.Event{Kind: api.EventStatus, Nodes: []graph.Ref{node}, Status: status})
}

// Output reports data, printed by node to stream, as an event, copying it, since it is valid only during the call.
func (ev jobEvents) Output(node graph.Ref, stream byte, data []byte) {
	ev.j.add(api.Event{Kind: api.EventOutput, Nodes: []graph.Ref{node}, Stream: stream,
		Data: append([]byte(nil), data...)})
}

// Spans reports a batch of spans as an event.
func (ev jobEvents) Spans(spans []tracing.Span) {
	ev.j.add(api.Event{Kind: api.EventSpans, Spans: spans})
}
