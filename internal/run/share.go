package run

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/tracing"

	"github.com/opencontainers/go-digest"
)

// sharing is a runner that deduplicates the actions in flight by key, for the jobs of one executor:
// an ordinary job that must run an action attaches to the attempt of that key already running, if any,
// including one that a forced rebuild started, and otherwise starts one; a forced run never attaches,
// and its attempt replaces any in flight as the one that later jobs attach to, since it is the later write.
// Each attempt returns its own result to every job attached, whether or not a later write replaces the record.
// An attempt runs while any job is attached to it, and is cancelled when none is: cancelling a job detaches it,
// and the last job to detach waits for the attempt to end, since a command in flight can't be interrupted,
// so that its container is removed before the job reports the cancellation.
// What the attempt prints reaches the job that started it as it is printed; a job that attaches later gets
// the result's log replayed once the attempt succeeds, rather than live output from part way through,
// and a failure's log with the failure.
// The attempt's trace spans belong to the job that started it, while it is attached: one that detaches
// while others remain has its spans revoked, so that the attempt sends nothing to it after it ends.
type sharing struct {
	runner  runner
	content cache.ContentStore // holds the logs that late jobs are replayed

	mu       sync.Mutex
	inflight map[digest.Digest]*attempt
}

// attempt is one execution of an action, which jobs attach to.
type attempt struct {
	cancel context.CancelFunc // ends the attempt, once no job is attached
	done   chan struct{}      // closed when the attempt has its result
	result *cache.ActionResult
	err    error

	mu       sync.Mutex
	attached []*listener // the jobs waiting for the result, with their events; the attempt is cancelled when none is
}

// listener is one job attached to an attempt, told apart by identity, since events may be functions.
type listener struct {
	ev     actionEvents
	late   bool   // it attached to an attempt in flight, so it gets the result's log replayed, not live output
	revoke func() // stops the attempt's spans reaching the job that started it; nil for a late job
}

// newSharing returns a sharing runner over r, whose content store holds the logs of results.
func newSharing(r runner, content cache.ContentStore) *sharing {
	return &sharing{runner: r, content: content, inflight: map[digest.Digest]*attempt{}}
}

// check checks through the runner.
func (s *sharing) check(ctx context.Context, a *action, result *cache.ActionResult) (error, error) {
	return s.runner.check(ctx, a, result)
}

// run runs req through the attempt of its key: attaching to one in flight, unless req.force,
// or else starting one, which a forced run registers too, in place of any in flight, so that ordinary jobs
// attach to it.
func (s *sharing) run(ctx context.Context, req actionRequest, ev actionEvents) (*cache.ActionResult, error) {
	key := req.action.key()
	s.mu.Lock()
	if at, found := s.inflight[key]; found && !req.force {
		l := &listener{ev: ev, late: true}
		at.attach(l)
		s.mu.Unlock()
		return s.await(ctx, at, l)
	}
	// The attempt keeps the starting job's trace, while it is attached, but not its cancellation:
	// only detaching cancels it.
	actx, revoke := tracing.Revocable(context.WithoutCancel(ctx))
	actx, cancel := context.WithCancel(actx)
	l := &listener{ev: ev, revoke: revoke}
	at := &attempt{cancel: cancel, done: make(chan struct{}), attached: []*listener{l}}
	s.inflight[key] = at
	s.mu.Unlock()
	go func() {
		defer close(at.done)
		at.result, at.err = s.runner.run(actx, req, at)
		s.mu.Lock()
		if s.inflight[key] == at {
			delete(s.inflight, key)
		}
		s.mu.Unlock()
	}()
	return s.await(ctx, at, l)
}

// await waits for the attempt at to end, or for ctx, the job's, to end first, which detaches the job, l;
// the last job to detach waits for the cancelled attempt to end, so that its cleanup has run,
// and the starter that detaches before others has the attempt's spans revoked.
// A late job that gets a result is replayed its log.
func (s *sharing) await(ctx context.Context, at *attempt, l *listener) (*cache.ActionResult, error) {
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
func (s *sharing) replay(ctx context.Context, result *cache.ActionResult, ev actionEvents) error {
	if result == nil || result.Log == nil {
		return nil
	}
	log, err := s.content.Read(ctx, *result.Log, cache.MaxLogSize)
	if err != nil {
		return err
	}
	return cache.ReplayLog(ctx, bytes.NewReader(log), streamWriter{ev, cache.StreamStdout},
		streamWriter{ev, cache.StreamStderr})
}

// detach takes the job l off at; once no job is attached, the attempt is cancelled and taken out of flight,
// so that no job attaches to a cancelled attempt, and detach reports that it did so.
func (s *sharing) detach(at *attempt, l *listener) bool {
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
