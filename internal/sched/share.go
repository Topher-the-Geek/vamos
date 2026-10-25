package sched

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/tracing"

	"github.com/opencontainers/go-digest"
)

// Sharing is a Runner that deduplicates the actions in flight by key, for the jobs of one executor:
// an ordinary job that must run an Action attaches to the attempt of that key already running, if any,
// including one that a forced rebuild started, and otherwise starts one; a forced run never attaches,
// and its attempt replaces any in flight as the one that later jobs attach to, since it is the later write.
// Nor does a job that runs the action natively attach to an emulated attempt, as a job offered fewer platforms
// starts: it starts its own, which replaces that one likewise, so that native work never waits for an emulator or
// on another job's permission. A job is told whose attempt it runs in, if its Output is a Joiner.
// Each attempt returns its own result to every job attached, whether or not a later write replaces the record.
// An attempt runs while any job is attached to it, and is cancelled when none is: cancelling a job detaches it,
// and the last job to detach waits for the attempt to end, since a command in flight can't be interrupted,
// so that its container is removed before the job reports the cancellation.
// What the attempt prints reaches the job that started it as it is printed; a job that attaches later gets
// the result's log replayed once the attempt succeeds, rather than live output from part way through,
// and a failure's log with the failure.
// The attempt's trace spans belong to the job that started it, while it is attached: one that detaches
// while others remain has its spans revoked, so that the attempt sends nothing to it after it ends.
// Sharing alone writes the record store: once an attempt has published its artifact and reported its result,
// the result is recorded under its key, or a resolve's tag record is written, before any job gets the result,
// and the latest write wins; the Runner writes no record.
type Sharing struct {
	runner  Runner
	content cache.ContentStore // holds the logs that late jobs are replayed
	records cache.RecordStore  // takes what attempts Report

	mu       sync.Mutex
	inflight map[digest.Digest]*attempt
}

// attempt is one execution of an Action, which jobs attach to.
type attempt struct {
	cancel   context.CancelFunc // ends the attempt, once no job is attached
	done     chan struct{}      // closed when the attempt has its result
	emulated bool               // its request is Emulated
	starter  Output             // the Output of the job that started it
	result   *cache.ActionResult
	err      error

	mu       sync.Mutex
	attached []*listener // the jobs waiting for the result, with their events; the attempt is cancelled when none is
}

// listener is one job attached to an attempt, told apart by identity, since events may be functions.
type listener struct {
	ev        Output
	late      bool            // it attached to an attempt in flight: the result's log is replayed, not live output
	revoke    func()          // stops the attempt's spans reaching the job that started it; nil for a late job
	permitted bool            // the job permits emulated execution
	withdrawn <-chan struct{} // closed once the job's demand is withdrawn; nil for a job whose demand never lapses
}

// NewSharing returns a Sharing Runner over r, whose content store holds the logs of results,
// and which records what attempts Report in records.
func NewSharing(r Runner, content cache.ContentStore, records cache.RecordStore) *Sharing {
	return &Sharing{runner: r, content: content, records: records, inflight: map[digest.Digest]*attempt{}}
}

// Run runs req through the attempt of its key: attaching to one in flight, unless req.Force, or the one in flight
// is emulated and req isn't, or else starting one, which those runs register too, in place of the one in flight,
// so that ordinary jobs attach to it.
// The Report returned carries the attempt's result alone: its record has been written.
func (s *Sharing) Run(ctx context.Context, req Request, ev Output) (*Report, error) {
	result, err := s.attempt(ctx, req, ev)
	if err != nil {
		return nil, err
	}
	return &Report{Result: result}, nil
}

// attempt runs req through the attempt of its key, as run describes, and returns the attempt's result.
func (s *Sharing) attempt(ctx context.Context, req Request, ev Output) (*cache.ActionResult, error) {
	key := req.Action.Key()
	s.mu.Lock()
	if at, found := s.inflight[key]; found && !req.Force && (req.Emulated || !at.emulated) {
		// A job that doesn't permit emulation may still attach to an emulating attempt: its result would be a
		// cache hit moments later anyway.
		l := &listener{ev: ev, late: true, permitted: req.Permitted, withdrawn: Withdrawn(ctx)}
		at.attach(l)
		s.mu.Unlock()
		join(ev, at.starter)
		return s.await(ctx, at, l)
	}
	if req.Emulated && !req.Permitted {
		s.mu.Unlock()
		return nil, ErrEmulationNeeded
	}
	// The attempt keeps the starting job's trace, while it is attached, but not its cancellation:
	// only detaching cancels it.
	actx, revoke := tracing.Revocable(context.WithoutCancel(ctx))
	actx, cancel := context.WithCancel(actx)
	l := &listener{ev: ev, revoke: revoke, permitted: req.Permitted, withdrawn: Withdrawn(ctx)}
	at := &attempt{cancel: cancel, done: make(chan struct{}), emulated: req.Emulated, starter: ev,
		attached: []*listener{l}}
	actx = withDemand(actx, at.demanded)
	if req.Emulated {
		actx = withPermission(actx, at.permitted)
	}
	s.inflight[key] = at
	s.mu.Unlock()
	join(ev, nil)
	go func() {
		defer close(at.done)
		rep, err := s.runner.Run(actx, req, at)
		if err == nil {
			// What an attempt published is recorded even if every job has detached meanwhile: it is made.
			err = s.publish(context.WithoutCancel(actx), req, rep)
		}
		if err == nil {
			at.result = rep.Result
		}
		at.err = err
		s.mu.Lock()
		if s.inflight[key] == at {
			delete(s.inflight, key)
		}
		s.mu.Unlock()
	}()
	return s.await(ctx, at, l)
}

// join tells ev, if it is a Joiner, whose attempt runs its job's action: the one starter started, or its own.
func join(ev Output, starter Output) {
	if j, ok := ev.(Joiner); ok {
		j.Join(starter)
	}
}

// publish writes what the attempt of req reported: a layer's or task's result under its key,
// or the record of the tag a resolve resolved, so that the artifact is published before the result is.
// A result that can't be recorded fails the attempt, keeping its log, as the artifact can't be reused.
func (s *Sharing) publish(ctx context.Context, req Request, rep *Report) (err error) {
	switch {
	case rep == nil || rep.Result == nil:
		return errors.New("the attempt reported no result")
	case rep.Tag != "":
		if err := s.records.SetTag(ctx, rep.Tag, rep.TagRecord); err != nil {
			return fmt.Errorf("saving to the build cache: %w", err)
		}
	case req.Key != "":
		_, span := tracing.Start(ctx, "cache write", "key", req.Key.String())
		defer func() { span.EndWith(err) }()
		if log := rep.Result.Log; log != nil {
			span.Set("log", log.Digest.String(), "log bytes", log.Size)
		}
		if err := s.records.SetResult(ctx, req.Key, rep.Result); err != nil {
			return WithLog(fmt.Errorf("saving to the build cache: %w", err), rep.Result.Log)
		}
	}
	return nil
}

// await waits for the attempt at to end, or for ctx, the job's, to end first, which detaches the job, l;
// the last job to detach waits for the cancelled attempt to end, so that its cleanup has run,
// and the starter that detaches before others has the attempt's spans revoked.
// A late job that gets a result is replayed its log.
func (s *Sharing) await(ctx context.Context, at *attempt, l *listener) (*cache.ActionResult, error) {
	select {
	case <-at.done:
		s.detach(at, l)
		if l.late && at.err == nil {
			if err := s.replay(ctx, at.result, l.ev); err != nil {
				return nil, fmt.Errorf("replaying its output from the build cache: %w", err)
			}
		}
		return at.result, at.err
	case <-ctx.Done():
		if s.detach(at, l) {
			<-at.done
		} else if l.revoke != nil {
			l.revoke()
		}
		return nil, ctx.Err()
	}
}

// replay passes on the log of result, if it has one, to ev.
func (s *Sharing) replay(ctx context.Context, result *cache.ActionResult, ev Output) error {
	if result == nil || result.Log == nil {
		return nil
	}
	log, err := s.content.Read(ctx, *result.Log, cache.MaxLogSize)
	if err != nil {
		return err
	}
	return cache.ReplayLog(ctx, bytes.NewReader(log), StreamWriter{ev, cache.StreamStdout},
		StreamWriter{ev, cache.StreamStderr})
}

// detach takes the job l off at; once no job is attached, the attempt is cancelled and taken out of flight,
// so that no job attaches to a cancelled attempt, and detach reports that it did so.
func (s *Sharing) detach(at *attempt, l *listener) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !at.remove(l) {
		return false
	}
	at.cancel()
	for key, in := range s.inflight {
		if in == at {
			delete(s.inflight, key)
		}
	}
	return true
}

// attach adds the job l to those the attempt reports to.
func (at *attempt) attach(l *listener) {
	at.mu.Lock()
	defer at.mu.Unlock()
	at.attached = append(at.attached, l)
}

// Attached returns how many jobs are attached to attempts in flight, for tests that wait for a job to attach.
func (s *Sharing) Attached() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, at := range s.inflight {
		at.mu.Lock()
		n += len(at.attached)
		at.mu.Unlock()
	}
	return n
}

// AttachedTo returns how many jobs are attached to the attempt in flight of key, for tests that wait for nodes alike
// to share one attempt, whatever else is in flight.
func (s *Sharing) AttachedTo(key digest.Digest) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.inflight[key]
	if !ok {
		return 0
	}
	at.mu.Lock()
	defer at.mu.Unlock()
	return len(at.attached)
}

// wants reports whether the job l still wants the attempt: its demand isn't withdrawn.
func (l *listener) wants() bool {
	select {
	case <-l.withdrawn:
		return false
	default:
		return true
	}
}

// demanded reports whether a job attached to the attempt still wants it.
func (at *attempt) demanded() bool {
	return at.wanted(func(*listener) bool { return true })
}

// permitted reports whether a job attached to the attempt that still wants it permits emulation, so that a job
// whose demand was withdrawn no longer lets the attempt run emulated for the others.
func (at *attempt) permitted() bool {
	return at.wanted(func(l *listener) bool { return l.permitted })
}

// wanted reports whether a job attached to the attempt still wants it and satisfies also.
func (at *attempt) wanted(also func(*listener) bool) bool {
	at.mu.Lock()
	defer at.mu.Unlock()
	for _, l := range at.attached {
		if l.wants() && also(l) {
			return true
		}
	}
	return false
}

// remove takes the job l off the attempt, and reports whether it was the last.
func (at *attempt) remove(l *listener) bool {
	at.mu.Lock()
	defer at.mu.Unlock()
	for i, e := range at.attached {
		if e == l {
			at.attached = append(at.attached[:i], at.attached[i+1:]...)
			break
		}
	}
	return len(at.attached) == 0
}

// Output passes data, printed to stream, to the job that started the attempt, while it is attached;
// late jobs get the log instead.
func (at *attempt) Output(stream byte, data []byte) {
	at.mu.Lock()
	var live []*listener
	for _, l := range at.attached {
		if !l.late {
			live = append(live, l)
		}
	}
	at.mu.Unlock()
	for _, l := range live {
		l.ev.Output(stream, data)
	}
}
