package coord

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/remote"
	"github.com/Topher-the-Geek/vamos/internal/sched"
	"github.com/Topher-the-Geek/vamos/internal/tracing"

	"github.com/containerd/platforms"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// pool is the coordinator's workers and the attempts they run: it hands pending attempts to workers that ask for
// them, fairly among builds, renews their leases on heartbeats, relays what they print, and takes their reports.
// It is the coordinator's side of remote.Workers, and the sched.Runner that the coordinator's Sharing runs through.
type pool struct {
	cfg        Config
	instance   string
	now        func() time.Time       // the clock, which tests replace
	generation func() (uint64, error) // the cache generation, which a prune advances

	mu       sync.Mutex
	workers  map[string]*worker
	attempts map[string]*attempt // live attempts, by ID
	pending  []*attempt          // attempts waiting for a worker, oldest first
	changed  chan struct{}       // closed and replaced whenever pending or workers change, waking pollers
	closed   bool
	// settling holds the leases of attempts ended when their workers were told to kill them, by ID: a worker that
	// missed the answer runs on until its fence, which comes before the lease would have passed.
	settling map[string]time.Time
}

// worker is a registered worker.
type worker struct {
	id       string
	info     remote.WorkerInfo
	lastSeen time.Time
	running  int // attempts assigned to it that are live
	polling  int // calls waiting for an assignment: a worker that waits is present
}

// attempt is one assignment of work to a worker, or one waiting for a worker.
type attempt struct {
	id    string
	build buildInfo
	req   *sched.Request
	push  *remote.PushWork
	out   sched.Output // where what it prints goes
	trace context.Context

	worker *worker   // nil while pending
	lease  time.Time // when it is lost unless renewed
	killed bool      // no build needs it: the worker is told to kill it
	lost   bool      // its lease expired
	done   chan struct{}
	report *remote.AttemptReport
	err    error
	clock  *tracing.Offset // how far the worker's clock is ahead of the coordinator's, as its heartbeats say
	// generation is the cache generation the attempt started in; a report from an earlier one than the current is
	// refused, since what it published may name what a prune removed.
	generation uint64
}

// buildInfo is what the pool knows of the build an attempt serves: for fairness and platform matching.
type buildInfo struct {
	job      api.JobID
	platform imgspecv1.Platform // the execution platform, which runs what names no platform of its own
}

type buildKey struct{}

// withBuild returns ctx carrying b, for the attempts the build makes.
func withBuild(ctx context.Context, b buildInfo) context.Context {
	return context.WithValue(ctx, buildKey{}, b)
}

// buildOf returns the build ctx carries, if any.
func buildOf(ctx context.Context) buildInfo {
	b, _ := ctx.Value(buildKey{}).(buildInfo)
	return b
}

// The errors of the pool.
var (
	errWorkerLost  = errors.New("the worker running it was lost")
	errPoolClosed  = errors.New("the coordinator is shutting down")
	errUnknownWork = errors.New("the worker isn't registered; register again")
)

// newPool returns an empty pool, whose attempts belong to the cache generation that generation reports.
func newPool(cfg Config, instance string, generation func() (uint64, error)) *pool {
	return &pool{
		cfg:        cfg,
		instance:   instance,
		now:        time.Now,
		generation: generation,
		workers:    map[string]*worker{},
		attempts:   map[string]*attempt{},
		settling:   map[string]time.Time{},
		changed:    make(chan struct{}),
	}
}

// newID returns a fresh identifier.
func newID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// wake wakes every poller waiting for a change; p.mu must be held.
func (p *pool) wake() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// Register adds a worker, refusing one whose keys would mean something else than the coordinator's.
func (p *pool) Register(_ context.Context, info remote.WorkerInfo) (remote.Registration, error) {
	if info.ActionVersion != sched.ActionVersion {
		return remote.Registration{}, fmt.Errorf("the worker runs action version %d, but this coordinator runs %d; "+
			"both must run the same version of vamos", info.ActionVersion, sched.ActionVersion)
	}
	if info.Capacity <= 0 {
		return remote.Registration{}, errors.New("the worker offers no capacity")
	}
	if info.Platform.OS == "" || info.Platform.Architecture == "" {
		return remote.Registration{}, errors.New("the worker names no platform")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return remote.Registration{}, errPoolClosed
	}
	w := &worker{id: newID(), info: info, lastSeen: p.now()}
	w.info.Platform = platforms.Normalize(info.Platform)
	w.info.Emulates = make([]imgspecv1.Platform, len(info.Emulates))
	for i, e := range info.Emulates {
		w.info.Emulates[i] = platforms.Normalize(e)
	}
	p.workers[w.id] = w
	p.wake()
	return remote.Registration{
		ID:              w.id,
		Instance:        p.instance,
		Lease:           p.cfg.Lease,
		Heartbeat:       p.cfg.Heartbeat,
		CacheRepository: p.cfg.CacheRepository,
		PullThrough:     p.cfg.PullThrough,
	}, nil
}

// offers reports whether a registered worker runs platform.
func (p *pool) offers(platform imgspecv1.Platform) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	want := platforms.Format(platforms.Normalize(platform))
	for _, w := range p.workers {
		if platforms.Format(w.info.Platform) == want {
			return true
		}
	}
	return false
}

// platformsOffered returns the platforms of the registered workers.
func (p *pool) platformsOffered() []imgspecv1.Platform {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	var offered []imgspecv1.Platform
	for _, w := range p.workers {
		if key := platforms.Format(w.info.Platform); !seen[key] {
			seen[key] = true
			offered = append(offered, w.info.Platform)
		}
	}
	return offered
}

// Run runs req on a worker, as sched.Runner asks: it waits for a worker of req's RunPlatform to take the attempt,
// relays what the attempt prints to out, and returns what the worker reported; an attempt whose worker is lost is
// tried again on another, up to the retries configured. Once ctx ends, the attempt is killed and ctx's error
// returned, without waiting for the worker's cleanup.
func (p *pool) Run(ctx context.Context, req sched.Request, out sched.Output) (*sched.Report, error) {
	rep, err := p.attempt(ctx, &req, nil, out)
	if err != nil {
		return nil, err
	}
	if rep.Failure != nil {
		return nil, rep.Failure.Err()
	}
	if rep.Report == nil {
		return nil, errors.New("the worker reported no result")
	}
	return rep.Report, nil
}

// attempt runs req, or push, on a worker, trying again on worker loss, and returns the worker's report.
// An attempt that waits a heartbeat interval with no worker of its platform registered says so to out, once,
// so that a build for a platform the deployment offers but no worker serves yet doesn't wait in silence.
func (p *pool) attempt(ctx context.Context, req *sched.Request, push *remote.PushWork, out sched.Output) (
	*remote.AttemptReport, error) {
	build := buildOf(ctx)
	retries := p.cfg.Retries
	if push != nil {
		retries = 0 // a push is never retried on worker loss: its write may have happened
	}
	if req != nil && req.Emulated {
		if platform, ok := p.emulationOffered(req.ImagePlatform()); !ok {
			return nil, fmt.Errorf("no worker of this deployment emulates %s, which its commands would run under; "+
				"add a worker with an emulator for it, or give --platform %s", platform,
				platforms.Format(platforms.Normalize(build.platform)))
		}
	}
	for try := 0; ; try++ {
		if req != nil && req.Emulated && !sched.Permitted(ctx) {
			return nil, sched.ErrEmulationNeeded // the builds that permitted emulation have left
		}
		generation, err := p.generation()
		if err != nil {
			return nil, fmt.Errorf("reading the build cache's generation: %w", err)
		}
		at := &attempt{id: newID(), build: build, req: req, push: push, out: out, trace: ctx, done: make(chan struct{}),
			generation: generation}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errPoolClosed
		}
		p.attempts[at.id] = at
		p.pending = append(p.pending, at)
		p.wake()
		p.mu.Unlock()
		if err := p.await(ctx, at); err != nil {
			return nil, err
		}
		if at.err == nil {
			return at.report, nil
		}
		if !at.lost || try >= retries {
			if at.lost && push != nil {
				return nil, fmt.Errorf("%w: it is unknown whether the push completed; check the destination, or push "+
					"again", at.err)
			}
			return nil, at.err
		}
		tracing.Current(ctx).Set(fmt.Sprintf("retry %d", try+1), at.err.Error())
	}
}

// await waits for at to end, or for ctx to end, when at is withdrawn and ctx's error returned.
// Every heartbeat interval while at waits for a worker, it tells out, once, if no worker runs its platform, and
// takes at back if no job wants it any more, as when every build that needed it has expired, or if no build that
// permits emulation still needs an emulated action, so that work nobody wants never starts.
func (p *pool) await(ctx context.Context, at *attempt) error {
	ticker := time.NewTicker(p.cfg.Heartbeat)
	defer ticker.Stop()
	told := false
	for {
		select {
		case <-at.done:
			return nil
		case <-ctx.Done():
			p.withdraw(at)
			return ctx.Err()
		case <-ticker.C:
			if !p.waiting(at) {
				continue
			}
			switch {
			case !sched.Demanded(ctx):
				p.withdraw(at)
				return sched.ErrWithdrawn
			case at.req != nil && at.req.Emulated && !sched.Permitted(ctx):
				p.withdraw(at)
				return sched.ErrEmulationNeeded
			}
			if platform, waiting := p.unserved(at); waiting && !told {
				told = true
				if platform == "" {
					at.out.Output(2, []byte("waiting for a worker; none is registered\n"))
				} else {
					at.out.Output(2, fmt.Appendf(nil, "waiting for a worker that runs %s; none is registered\n",
						platform))
				}
			}
		}
	}
}

// waiting reports whether at still waits for a worker.
func (p *pool) waiting(at *attempt) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return at.worker == nil
}

// unserved reports whether at is pending with no registered worker that suits it, and the platform it waits for, or
// "" if any would do.
func (p *pool) unserved(at *attempt) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if at.worker != nil {
		return "", false
	}
	for _, w := range p.workers {
		if p.suits(w, at) {
			return "", false
		}
	}
	if at.req != nil && !at.req.RunsCommands() {
		return "", true // it waits for any worker, since none is registered
	}
	platform := at.runsOn()
	if at.req != nil && at.req.Emulated {
		platform += " and emulates " + at.req.ImagePlatform()
	}
	return platform, true
}

// emulationOffered reports whether the deployment offers emulation of platform, by configuration or by a worker
// registered now, and returns the platform as the pool names it.
func (p *pool) emulationOffered(platform string) (string, bool) {
	want, err := platforms.Parse(platform)
	if err != nil {
		return platform, false
	}
	name := platforms.Format(platforms.Normalize(want))
	for _, e := range p.cfg.Emulated {
		if platforms.Format(platforms.Normalize(e)) == name {
			return name, true
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, w := range p.workers {
		if emulates(w.info, name) {
			return name, true
		}
	}
	return name, false
}

// emulates reports whether a worker described by info emulates the platform named name.
func emulates(info remote.WorkerInfo, name string) bool {
	for _, e := range info.Emulates {
		if platforms.Format(platforms.Normalize(e)) == name {
			return true
		}
	}
	return false
}

// withdraw takes at back, because no build needs it: a pending attempt is forgotten, and a running one is marked
// killed, which its worker learns at its next heartbeat.
func (p *pool) withdraw(at *attempt) {
	p.mu.Lock()
	defer p.mu.Unlock()
	at.killed = true
	for i, pending := range p.pending {
		if pending == at {
			p.pending = append(p.pending[:i], p.pending[i+1:]...)
			p.end(at, nil, context.Canceled)
			return
		}
	}
	// A running attempt stays in the table until its worker hears of the kill, reports, or its lease expires,
	// so that its report is discarded and its heartbeat told to kill it.
}

// end finishes at with report or err; p.mu must be held. An attempt ended twice keeps its first ending.
func (p *pool) end(at *attempt, report *remote.AttemptReport, err error) {
	select {
	case <-at.done:
		return
	default:
	}
	at.report, at.err = report, err
	if at.worker != nil {
		at.worker.running--
	}
	delete(p.attempts, at.id)
	close(at.done)
	p.wake()
}

// NextAssignment waits for an attempt that the worker id can run, until ctx ends, and assigns it.
// Among the pending attempts it can run, the one whose build has the fewest attempts running comes first,
// so that concurrent builds share the workers, and the oldest among those.
func (p *pool) NextAssignment(ctx context.Context, id string) (*remote.Assignment, error) {
	for {
		p.mu.Lock()
		w, ok := p.workers[id]
		if !ok {
			p.mu.Unlock()
			return nil, errUnknownWork
		}
		w.lastSeen = p.now()
		if w.running < w.info.Capacity {
			if at := p.pick(w); at != nil {
				at.worker = w
				at.lease = p.now().Add(p.cfg.Lease)
				w.running++
				a := &remote.Assignment{Attempt: at.id, Request: at.req, Push: at.push, Lease: p.cfg.Lease,
					Trace: tracing.Call(at.trace)}
				p.mu.Unlock()
				return a, nil
			}
		}
		changed := p.changed
		w.polling++
		p.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
		}
		p.mu.Lock()
		w.polling--
		w.lastSeen = p.now()
		p.mu.Unlock()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
}

// pick returns the pending attempt for w to run, taking it off the queue, or nil; p.mu must be held.
func (p *pool) pick(w *worker) *attempt {
	best := -1
	var load int
	for i, at := range p.pending {
		// Work no build wants any more, or an emulated action no permitting build needs, waits for await to take
		// it back, rather than starting in the interval before it looks.
		if !p.suits(w, at) || !sched.Demanded(at.trace) ||
			(at.req != nil && at.req.Emulated && !sched.Permitted(at.trace)) {
			continue
		}
		if best < 0 || p.load(at.build.job) < load {
			best, load = i, p.load(at.build.job)
		}
	}
	if best < 0 {
		return nil
	}
	at := p.pending[best]
	p.pending = append(p.pending[:best], p.pending[best+1:]...)
	return at
}

// load returns how many attempts of the build job are assigned to workers; p.mu must be held.
func (p *pool) load(job api.JobID) int {
	n := 0
	for _, at := range p.attempts {
		if at.build.job == job && at.worker != nil {
			n++
		}
	}
	return n
}

// runsOn returns the platform of the worker that at needs: the one its request runs on, as the scheduler chose it,
// or else its build's execution platform.
func (at *attempt) runsOn() string {
	if at.req != nil && at.req.RunPlatform != "" {
		return at.req.RunPlatform
	}
	return platforms.Format(platforms.Normalize(at.build.platform))
}

// suits reports whether w runs the platform at runs on, and, for an emulated action, emulates the platform of its
// image; p.mu must be held. An action that runs no command, a resolve or a target that only adds files, runs on any
// worker while none of its platform is registered.
func (p *pool) suits(w *worker, at *attempt) bool {
	if at.push != nil {
		return true // a push copies between registries, which any worker can
	}
	if platforms.Format(w.info.Platform) != at.runsOn() {
		return at.req != nil && !at.req.RunsCommands() && !p.serves(at.runsOn())
	}
	if at.req != nil && at.req.Emulated {
		want, err := platforms.Parse(at.req.ImagePlatform())
		return err == nil && emulates(w.info, platforms.Format(platforms.Normalize(want)))
	}
	return true
}

// serves reports whether a worker of platform is registered; p.mu must be held.
func (p *pool) serves(platform string) bool {
	for _, w := range p.workers {
		if platforms.Format(w.info.Platform) == platform {
			return true
		}
	}
	return false
}

// live returns the attempt id if it is live, and otherwise ErrStaleAttempt; p.mu must be held.
// An attempt whose lease has passed is lost here, without waiting for the sweep, so that a late heartbeat, events
// or report can't renew or end an attempt the worker has been told, by its fence, to have stopped.
func (p *pool) live(id string) (*attempt, error) {
	at, ok := p.attempts[id]
	if !ok || at.worker == nil || at.lost {
		return nil, remote.ErrStaleAttempt
	}
	if p.now().After(at.lease) {
		p.lose(at)
		return nil, remote.ErrStaleAttempt
	}
	return at, nil
}

// lose ends at, whose lease has passed, as lost, so that its work is tried again elsewhere; p.mu must be held.
func (p *pool) lose(at *attempt) {
	at.lost = true
	p.end(at, nil, fmt.Errorf("%w: worker %s didn't renew its lease", errWorkerLost, at.worker.id))
}

// Heartbeat renews the attempt's lease, and tells the worker whether to kill it; an attempt told to die is ended
// then, so that its slot is free before its lease would have expired, but stays settling for prunes until then.
// The worker's estimate of its clock's offset, if the heartbeat carries one, is kept when it is at least as sure
// as the last; the answer carries the coordinator's clock for the worker's next estimate.
func (p *pool) Heartbeat(_ context.Context, id string, beat remote.Heartbeat) (remote.HeartbeatAnswer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	at, err := p.live(id)
	if err != nil {
		return remote.HeartbeatAnswer{Kill: true, Clock: now}, err
	}
	at.worker.lastSeen = now
	if at.killed {
		p.settling[at.id] = at.lease // the kill doesn't renew it, and the answer may never arrive
		p.end(at, nil, context.Canceled)
		return remote.HeartbeatAnswer{Kill: true, Clock: now}, nil
	}
	at.lease = now.Add(p.cfg.Lease)
	if beat.Clock != nil && (at.clock == nil || beat.Clock.Uncertainty <= at.clock.Uncertainty) {
		at.clock = beat.Clock
	}
	return remote.HeartbeatAnswer{Clock: now}, nil
}

// Events relays what the attempt printed to the builds attached to it, and its spans, shifted onto the
// coordinator's clock, to the trace of the build that started it.
func (p *pool) Events(_ context.Context, id string, events remote.AttemptEvents) error {
	p.mu.Lock()
	at, err := p.live(id)
	if err != nil {
		p.mu.Unlock()
		return err
	}
	at.lease = p.now().Add(p.cfg.Lease)
	out, trace := at.out, at.trace
	var offset tracing.Offset
	if at.clock != nil {
		offset = *at.clock
	}
	p.mu.Unlock()
	for _, o := range events.Output {
		out.Output(o.Stream, o.Data)
	}
	if len(events.Spans) > 0 {
		tracing.Relay(trace, tracing.Shift(events.Spans, offset))
	}
	return nil
}

// Report ends the attempt with what the worker reported, unless the attempt is no longer live: a report from an
// attempt whose lease expired, or that was killed or replaced, is discarded, so that only live attempts write;
// one from a killed attempt whose worker missed the kill ends its settling.
func (p *pool) Report(_ context.Context, id string, report remote.AttemptReport) error {
	current, generationErr := p.generation() // read before the lock: it may be a file
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.settling[id]; ok {
		delete(p.settling, id) // the worker has stopped it, missing the kill
		return remote.ErrStaleAttempt
	}
	at, err := p.live(id)
	if err != nil {
		return err
	}
	switch {
	case at.killed:
		p.end(at, nil, context.Canceled) // the worker is done with it, whatever it reports
		return remote.ErrStaleAttempt
	case generationErr != nil:
		p.end(at, nil, fmt.Errorf("reading the build cache's generation: %w", generationErr))
		return remote.ErrStaleAttempt
	case at.generation != current:
		p.end(at, nil, fmt.Errorf("the build cache was pruned while the attempt ran: it started in generation %d, "+
			"and the cache is at %d; run the command again", at.generation, current))
		return remote.ErrStaleAttempt
	}
	p.end(at, &report, nil)
	return nil
}

// expire loses every attempt whose lease has passed, so that its work is tried again elsewhere,
// and forgets killed attempts whose leases have passed and workers not seen for a lease, waking the pollers, since
// work that runs no command goes to any worker once none of its platform is left.
func (p *pool) expire() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for _, at := range p.attempts {
		if at.worker != nil && now.After(at.lease) {
			p.lose(at)
		}
	}
	for id, lease := range p.settling {
		if now.After(lease) {
			delete(p.settling, id)
		}
	}
	forgot := false
	for id, w := range p.workers {
		if w.running == 0 && w.polling == 0 && now.Sub(w.lastSeen) > p.cfg.Lease {
			delete(p.workers, id)
			forgot = true
		}
	}
	if forgot {
		p.wake()
	}
}

// sweep expires leases every heartbeat interval until ctx ends, and runs also, the jobs' sweep.
func (p *pool) sweep(ctx context.Context, also func()) {
	ticker := time.NewTicker(max(p.cfg.Heartbeat, 50*time.Millisecond))
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.expire()
			also()
		case <-ctx.Done():
			return
		}
	}
}

// liveAttempts returns how many attempts are pending or running, counting a killed one until its worker reports
// or its lease would have passed, since the worker may not have heard of the kill.
func (p *pool) liveAttempts() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	n := len(p.attempts)
	for _, lease := range p.settling {
		if !now.After(lease) {
			n++
		}
	}
	return n
}

// close ends every attempt as the coordinator shuts down, and accepts no more.
func (p *pool) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for _, at := range p.attempts {
		p.end(at, nil, errPoolClosed)
	}
	p.pending = nil
	p.wake()
}
