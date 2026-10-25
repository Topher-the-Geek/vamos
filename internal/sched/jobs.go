package sched

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/tracing"
)

// The bounds on what a Job retains for watchers, by default.
const (
	// defaultRetention is how many of a Job's events the executor keeps for watchers:
	// progress events are small, and a watcher that falls this far behind has stopped reading.
	// Command output is in the log blobs too, so a gap loses no information that mattered.
	defaultRetention = 4096
	// defaultRetentionBytes bounds the output a Job's retained events hold, since command output can be large.
	defaultRetentionBytes = 8 << 20
	// evictedRemembered is how many evicted jobs the table remembers, so that a watch of one says it expired
	// rather than that the job never existed, as it would after a restart.
	evictedRemembered = 4096
)

// Jobs holds the jobs of an executor, in memory.
// A build is attached to while a watch of it is open; a build with no watcher for the grace period, counted from
// its last watcher leaving, or from its start if it was never watched, expires: its demand for work that hasn't
// started is withdrawn, what runs finishes and publishes, and the job ends with a failure saying it expired.
// A finished job stays readable for the window after its end or its last watch, then is evicted, or at the next
// sweep once released; a watch of an evicted job gets api.ErrExpired.
// Without a grace period nothing expires or is evicted, as a local executor's jobs, which live as long as the
// command, need neither.
type Jobs struct {
	retention      int           // events kept per Job; defaultRetention if 0
	retentionBytes int           // bytes of output kept per Job; defaultRetentionBytes if 0
	grace          time.Duration // how long a build goes unwatched before it expires; 0 never
	window         time.Duration // how long a finished job stays readable; at least grace
	limit          int           // how many jobs may be open at once; no bound if 0
	now            func() time.Time

	mu        sync.Mutex
	jobs      map[api.JobID]*Job
	evicted   map[api.JobID]bool // the jobs evicted, the last evictedRemembered of them
	evictions []api.JobID        // in order, so that the oldest is forgotten first
	running   sync.WaitGroup     // the jobs that haven't ended
	active    int                // how many jobs haven't ended, reservations included
	reserved  int                // how many jobs are reserved and not yet started
	exclusive *Job               // the Job that must run alone, a prune, while it runs
	closed    bool               // stop has been called: no Job starts
	onEvict   func(api.JobID)    // told of each eviction, outside the lock
}

// RetainEvents makes the table retain n events of each job for watchers, rather than the default;
// it is for tests, before any job is started.
func (t *Jobs) RetainEvents(n int) {
	t.retention = n
}

// RetainBytes bounds the output that each job's retained events hold to n bytes, rather than the default.
func (t *Jobs) RetainBytes(n int) {
	t.retentionBytes = n
}

// Limit makes the table refuse a job, with ErrBusy, while n are open; 0 removes the bound.
// It is for before any job is started.
func (t *Jobs) Limit(n int) {
	t.limit = n
}

// Expire makes builds expire after grace without a watcher, and finished jobs readable for window, which is
// raised to grace if shorter; Sweep applies both. A grace of 0 turns both off.
func (t *Jobs) Expire(grace, window time.Duration) {
	t.grace, t.window = grace, max(window, grace)
}

// OnEvict makes the table tell fn of each job it evicts, once the eviction is done, outside its lock;
// it is for before any job is started.
func (t *Jobs) OnEvict(fn func(api.JobID)) {
	t.onEvict = fn
}

// Active returns how many jobs haven't ended.
func (t *Jobs) Active() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active
}

// clock returns the time, as the table tells it.
func (t *Jobs) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

type jobKey struct{}

// JobOf returns the job that ctx, as Start gives a job's work, belongs to, or "" for another context.
func JobOf(ctx context.Context) api.JobID {
	id, _ := ctx.Value(jobKey{}).(api.JobID)
	return id
}

type withdrawKey struct{}

// Withdrawn returns a channel that closes when the demand of the job ctx belongs to is withdrawn, as a build's
// is when it expires or is released while running: what runs finishes, and nothing more starts.
// For a context that is no job's, it is nil, which never closes.
func Withdrawn(ctx context.Context) <-chan struct{} {
	ch, _ := ctx.Value(withdrawKey{}).(chan struct{})
	return ch
}

// The errors that starting a Job can meet.
var (
	ErrClosed  = errors.New("the executor has shut down")
	ErrPruning = errors.New("a prune is running on this executor; submit again when it has ended")
	ErrBusy    = errors.New("the executor is busy: as many jobs are open as it accepts; try again")
)

// Job is one Job of the executor: the events it retains, for watchers to read from any position they hold,
// and the context that cancels it.
// Events are appended under mu; changed is closed and replaced whenever one is, so that watchers can wait for it.
// The Job retains the last retention events: older positions are expired, and a watch from one meets a gap.
type Job struct {
	cancel         context.CancelFunc
	retention      int
	retentionBytes int
	resumed        int64         // the position an earlier run's events end before, for a Job resumed; 0 otherwise
	expires        bool          // a build, whose demand lapses without a watcher
	withdrawn      chan struct{} // closed once the demand is withdrawn
	now            func() time.Time

	mu       sync.Mutex
	first    int64       // the position of events[0]
	events   []api.Event // the retained events, the last of them the terminal event once done
	bytes    int         // the output the retained events hold
	changed  chan struct{}
	done     bool
	sealed   bool        // its work has returned: events are dropped until the terminal event
	outcome  api.Outcome // once done
	watchers int         // the watches open now
	idle     time.Time   // when the last watcher left, or the Job started
	ended    time.Time   // when the Job ended
	released bool        // the client is done with it
	expired  bool        // its demand was withdrawn for want of a watcher
}

// Cancel asks the job to stop; its effect is reported in its terminal event.
func (j *Job) Cancel() {
	j.cancel()
}

// Release says that the client is done with the Job, delivery included: a finished Job is evicted at the next
// sweep, and a running build's demand is withdrawn, as if it had expired.
func (j *Job) Release() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.released = true
	if !j.done && j.expires {
		j.withdraw()
	}
}

// withdraw withdraws the Job's demand, once; j.mu must be held.
func (j *Job) withdraw() {
	select {
	case <-j.withdrawn:
	default:
		close(j.withdrawn)
	}
}

// Start adds a Job that runs work in a goroutine of its own, with a context that only Cancel ends,
// and returns its handle.
// work reports its progress to the events it is given, and its outcome ends the Job.
// An exclusive Job, a prune, is refused while any Job runs, and no Job starts while one runs, after stop,
// or while as many Jobs are open as the limit allows, all decided under one lock, so that a prune and another Job
// never overlap on this executor and the bound holds under concurrent starts.
// A Job started so never expires: it is a push or a prune, which runs to completion unwatched.
func (t *Jobs) Start(exclusive bool, work func(ctx context.Context, ev Events) api.Outcome) (api.JobID, error) {
	return t.StartAs(Spec{Exclusive: exclusive}, work)
}

// StartBuild adds a Job as Start does, which expires without a watcher: a build, whose demand its client's
// attachment carries.
func (t *Jobs) StartBuild(work func(ctx context.Context, ev Events) api.Outcome) (api.JobID, error) {
	return t.StartAs(Spec{Build: true}, work)
}

// Spec says how a Job starts, for StartAs, Reserve and Restore.
type Spec struct {
	// ID is the Job's ID: one that NewJobID made, which an executor keeping its jobs on disk records before the
	// start; a fresh one if "".
	ID api.JobID
	// Exclusive is a Job that runs alone, a prune; Build one that expires without a watcher.
	Exclusive, Build bool
	// First is the position of the Job's first event, and Resumed the position before which every position is an
	// earlier run's: for a Job resumed after a restart, both are past every position its earlier runs gave, so that a
	// watcher resuming from one of those meets a gap that says the executor restarted.
	First, Resumed int64
	// Ending, if set, is given the Job's terminal event, with its position and outcome, once its work has returned and
	// before any watcher can see the event, outside the table's lock, so that an executor keeping its jobs on disk
	// records it first. Events that the work's attempts send after it returned are dropped from then on.
	Ending func(done api.Event)
}

// NewJobID returns a fresh job ID.
func NewJobID() api.JobID {
	var b [8]byte
	rand.Read(b[:])
	return api.JobID(hex.EncodeToString(b[:]))
}

// StartAs adds a Job as spec says, as Start and StartBuild describe.
func (t *Jobs) StartAs(spec Spec, work func(ctx context.Context, ev Events) api.Outcome) (api.JobID, error) {
	r, err := t.Reserve(spec)
	if err != nil {
		return "", err
	}
	return r.Start(work)
}

// Reservation is a Job admitted to the table that hasn't started: it holds its place under the limit, and an
// exclusive one its exclusion, until Start or Cancel, so that an executor can record the Job between its admission
// and its start, and record none that the table refuses.
type Reservation struct {
	t     *Jobs
	id    api.JobID
	spec  Spec
	j     *Job
	ended bool // started or cancelled
}

// Reserve admits a Job as spec says, under the rules that Start describes, without starting it.
func (t *Jobs) Reserve(spec Spec) (*Reservation, error) {
	id := spec.ID
	if id == "" {
		id = NewJobID()
	}
	j := t.newJob(spec)
	t.mu.Lock()
	defer t.mu.Unlock()
	held := 0
	if spec.Exclusive {
		held = t.held()
	}
	switch {
	case t.closed:
		return nil, ErrClosed
	case t.exclusive != nil:
		return nil, ErrPruning
	case spec.Exclusive && held > 0:
		return nil, fmt.Errorf("%d other jobs are running or held open on this executor; prune when they have ended "+
			"and their clients have released them", held)
	case t.limit > 0 && t.active >= t.limit:
		return nil, ErrBusy
	}
	if spec.Exclusive {
		t.exclusive = j
	}
	t.active++
	t.reserved++
	return &Reservation{t: t, id: id, spec: spec, j: j}, nil
}

// ID returns the ID the Job will have.
func (r *Reservation) ID() api.JobID {
	return r.id
}

// Start starts the Job reserved, which runs work, as Start describes, unless the table has stopped since the
// reservation, when it fails with ErrClosed, as Cancel would.
func (r *Reservation) Start(work func(ctx context.Context, ev Events) api.Outcome) (api.JobID, error) {
	t, j := r.t, r.j
	t.mu.Lock()
	defer t.mu.Unlock()
	if r.ended {
		return "", errors.New("the reservation was used already")
	}
	r.ended = true
	t.reserved--
	if t.closed {
		t.unreserve(j)
		return "", ErrClosed
	}
	ctx, cancel := context.WithCancel(context.Background())
	ctx = context.WithValue(ctx, jobKey{}, r.id)
	ctx = context.WithValue(ctx, withdrawKey{}, j.withdrawn)
	j.cancel = cancel
	if t.jobs == nil {
		t.jobs = map[api.JobID]*Job{}
	}
	t.jobs[r.id] = j
	t.running.Add(1)
	ending := r.spec.Ending
	go func() {
		defer t.running.Done()
		defer cancel()
		outcome := work(ctx, jobEvents{j})
		done := j.seal(outcome)
		if ending != nil {
			ending(done)
		}
		t.mu.Lock()
		j.finish(outcome, t.clock())
		t.active--
		if t.exclusive == j {
			t.exclusive = nil
		}
		t.mu.Unlock()
	}()
	return r.id, nil
}

// Cancel gives the reservation up, if it hasn't been started.
func (r *Reservation) Cancel() {
	t := r.t
	t.mu.Lock()
	defer t.mu.Unlock()
	if r.ended {
		return
	}
	r.ended = true
	t.reserved--
	t.unreserve(r.j)
}

// unreserve gives up the place that j, reserved and never started, held; t.mu must be held.
func (t *Jobs) unreserve(j *Job) {
	t.active--
	if t.exclusive == j {
		t.exclusive = nil
	}
}

// newJob returns a Job as spec says, not yet in the table.
func (t *Jobs) newJob(spec Spec) *Job {
	retention := t.retention
	if retention <= 0 {
		retention = defaultRetention
	}
	retentionBytes := t.retentionBytes
	if retentionBytes <= 0 {
		retentionBytes = defaultRetentionBytes
	}
	return &Job{cancel: func() {}, retention: retention, retentionBytes: retentionBytes, resumed: spec.Resumed,
		first: spec.First, expires: spec.Build, withdrawn: make(chan struct{}), changed: make(chan struct{}),
		idle: t.clock(), now: t.clock}
}

// Restore adds a Job that an earlier run of the executor finished with outcome at ended, as spec says, its terminal
// event at spec.First, the position it had, so that a watcher past it gets nothing and one before it a gap, then the
// event: it is readable for the window from now, as if its client had just stopped watching, since the client may
// have been holding it through delivery when the executor stopped.
func (t *Jobs) Restore(spec Spec, outcome api.Outcome, ended time.Time) (*Job, error) {
	j := t.newJob(spec)
	j.finish(outcome, ended)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, ErrClosed
	}
	if t.jobs == nil {
		t.jobs = map[api.JobID]*Job{}
	}
	t.jobs[spec.ID] = j
	return j, nil
}

// RememberEvicted makes the table remember id as a Job evicted, as an earlier run of the executor did, so that a
// watch of it says it expired; the oldest is given first.
func (t *Jobs) RememberEvicted(id api.JobID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.remember(id)
}

// held returns how many jobs are open: running, watched, or finished builds their clients may still hold, within
// the grace period of their last watcher leaving and not released; t.mu must be held, and each j.mu is taken under
// it, the order Sweep and finish keep too.
func (t *Jobs) held() int {
	now := t.clock()
	n := t.reserved
	for _, j := range t.jobs {
		if j.open(now, t.grace) {
			n++
		}
	}
	return n
}

// open reports whether the Job is open at now, as held describes.
func (j *Job) open(now time.Time, grace time.Duration) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.done || j.watchers > 0 {
		return true
	}
	return grace > 0 && j.expires && !j.released && now.Sub(j.idle) < grace
}

// Stop cancels every Job that hasn't ended, refuses any more, and waits for them all to end,
// so that their cleanup has run.
func (t *Jobs) Stop() {
	t.mu.Lock()
	t.closed = true
	for _, j := range t.jobs {
		j.Cancel()
	}
	t.mu.Unlock()
	t.running.Wait()
}

// Lookup returns the Job id, or ErrNoJob; api.ErrExpired for a Job that was evicted.
func (t *Jobs) Lookup(id api.JobID) (*Job, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	j, ok := t.jobs[id]
	if !ok {
		if t.evicted[id] {
			return nil, api.ErrExpired
		}
		return nil, api.ErrNoJob
	}
	return j, nil
}

// Sweep expires the builds that have gone unwatched for the grace period, and evicts the finished jobs that have
// been readable for the window, or were released; it does nothing without a grace period.
func (t *Jobs) Sweep() {
	if t.grace <= 0 {
		return
	}
	now := t.clock()
	var evicted []api.JobID
	t.mu.Lock()
	for id, j := range t.jobs {
		if evict := j.sweep(now, t.grace, t.window); evict {
			delete(t.jobs, id)
			t.remember(id)
			evicted = append(evicted, id)
		}
	}
	t.mu.Unlock()
	if t.onEvict != nil {
		for _, id := range evicted {
			t.onEvict(id)
		}
	}
}

// remember keeps id as evicted, forgetting the oldest beyond evictedRemembered; t.mu must be held.
func (t *Jobs) remember(id api.JobID) {
	if t.evicted == nil {
		t.evicted = map[api.JobID]bool{}
	}
	t.evicted[id] = true
	t.evictions = append(t.evictions, id)
	if len(t.evictions) > evictedRemembered {
		delete(t.evicted, t.evictions[0])
		t.evictions = t.evictions[1:]
	}
}

// sweep applies the grace period and the window to the Job at now, and reports whether to evict it.
func (j *Job) sweep(now time.Time, grace, window time.Duration) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	switch {
	case j.done:
		// A client that keeps watching a finished build, as one holding it through delivery does, keeps it.
		return j.watchers == 0 && (j.released || now.Sub(j.ended) >= window && now.Sub(j.idle) >= window)
	case j.expires && !j.expired && j.watchers == 0 && now.Sub(j.idle) >= grace:
		j.expired = true
		j.withdraw()
	}
	return false
}

// add appends e to the Job's events, giving it its position, and wakes watchers,
// dropping the oldest event if the Job retains as many as it keeps.
// An event after the terminal one is dropped: nothing may follow it, and a shared attempt that outlives the Job
// which started it keeps reporting to it.
func (j *Job) add(e api.Event) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.done || j.sealed {
		return
	}
	j.append(e)
}

// append appends e under the lock, as add does, dropping the oldest events while the Job retains more than it
// keeps, by count or by bytes of output.
func (j *Job) append(e api.Event) {
	e.Position = j.first + int64(len(j.events))
	j.events = append(j.events, e)
	j.bytes += len(e.Data)
	for len(j.events) > 1 && (len(j.events) > j.retention || j.bytes > j.retentionBytes) {
		j.bytes -= len(j.events[0].Data)
		j.events[0] = api.Event{} // the backing array outlives the slice: what it held would stay reachable
		j.events = j.events[1:]
		j.first++
	}
	close(j.changed)
	j.changed = make(chan struct{})
}

// seal drops the events that arrive from now on, and returns the terminal event that finish will append, with outcome
// and its position.
func (j *Job) seal(outcome api.Outcome) api.Event {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.sealed = true
	return api.Event{Position: j.first + int64(len(j.events)), Kind: api.EventDone, Outcome: &outcome}
}

// finish ends the Job with outcome, as its terminal event: the event is appended and the Job is done in one step,
// so that a watcher that saw the event finds the outcome.
func (j *Job) finish(outcome api.Outcome, now time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.append(api.Event{Kind: api.EventDone, Outcome: &outcome})
	j.done, j.outcome, j.ended = true, outcome, now
}

// at returns the event at position, if the Job retains it, and otherwise whether the Job has ended,
// the first position retained, and a channel that closes when the Job has more events.
func (j *Job) at(position int64) (e api.Event, ok, done bool, first int64, changed <-chan struct{}) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if position >= j.first && position < j.first+int64(len(j.events)) {
		return j.events[position-j.first], true, j.done, j.first, j.changed
	}
	return api.Event{}, false, j.done, j.first, j.changed
}

// Result returns the Job's outcome, or ErrRunning if it hasn't ended.
func (j *Job) Result() (api.Outcome, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.done {
		return api.Outcome{}, api.ErrRunning
	}
	return j.outcome.Clone(), nil
}

// attach counts a watcher, and the function it returns the watcher's leaving, stamping when the Job was last
// watched, so that the grace period counts from there.
func (j *Job) attach() func() {
	j.mu.Lock()
	j.watchers++
	j.mu.Unlock()
	return func() {
		j.mu.Lock()
		defer j.mu.Unlock()
		j.watchers--
		j.idle = j.now()
	}
}

// Watch delivers the Job's events from position from to fn, as Executor.Watch describes:
// a position already expired gets a gap event first, and a watcher that falls behind the retained events
// as it reads is dropped with ErrFellBehind.
// A watcher that has just been given a gap, and whose first retained position expired meanwhile, gets another gap
// rather than being dropped: it has read nothing yet. A gap from a position an earlier run of a resumed Job gave
// says that the executor restarted.
// Each event is a copy, which fn may keep or change.
// The end of ctx is noticed before each event, not only while waiting for one,
// so that a watcher behind a Job that has many events stops when asked.
// While it runs, the Job is attached to; a watch past the terminal event, which delivers nothing, renews the
// attachment all the same, which is how a client holds a build through its delivery.
func (j *Job) Watch(ctx context.Context, from int64, fn func(api.Event) error) error {
	if from < 0 {
		return fmt.Errorf("position %d is before the first event", from)
	}
	defer j.attach()()
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
			gap := api.Event{Position: from, Kind: api.EventGap, First: first, Restarted: from < j.resumed}
			if err := fn(gap); err != nil {
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

// jobEvents sends what the Runner reports to a Job's events.
type jobEvents struct {
	j *Job
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
