package sched

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/Topher-the-Geek/vamos/internal/tracing"

	"github.com/opencontainers/go-digest"
)

// blockingRunner is a Runner whose runs block until released, counting them, and whose results say which run
// they came from, so that a test sees which jobs shared an attempt.
// With gated, runs are released one at a time, through releaseRun, so that a test orders their results;
// with content, each run saves what it printed there as the result's log.
// With fail, the runs it names fail rather than returning their results.
type blockingRunner struct {
	release chan struct{} // closed to let every run finish
	gated   bool
	content cache.ContentStore
	fail    map[int32]bool // the runs, by number, that fail once released
	runs    atomic.Int32
	mu      sync.Mutex
	cancels int                     // runs whose context ended before they were released
	gates   map[int32]chan struct{} // a gate for each run, by number, which releaseRun opens; see gate
}

// gate returns the channel that the run n waits on besides release, so that a test can release runs in order.
func (b *blockingRunner) gate(n int32) chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.gates == nil {
		b.gates = map[int32]chan struct{}{}
	}
	if b.gates[n] == nil {
		b.gates[n] = make(chan struct{})
	}
	return b.gates[n]
}

// releaseRun lets the run n finish.
func (b *blockingRunner) releaseRun(n int32) {
	close(b.gate(n))
}

// Run prints, waits to be released, prints again, and returns a result stamped with the run's number;
// it records a span as it starts and another once released.
func (b *blockingRunner) Run(ctx context.Context, req Request, ev Output) (*Report, error) {
	n := b.runs.Add(1)
	log, err := cache.NewLog()
	if err != nil {
		return nil, err
	}
	defer log.Discard()
	stdout := log.Writer(cache.StreamStdout, StreamWriter{Out: ev, Stream: cache.StreamStdout})
	tracing.Instant(ctx, "started")
	fmt.Fprint(stdout, "started\n")
	released := b.release
	if b.gated || b.fail != nil {
		released = b.gate(n) // runs that are ordered, or fail, are released one at a time
	}
	select {
	case <-released:
		tracing.Instant(ctx, "released")
		fmt.Fprint(stdout, "done\n")
		if b.fail[n] {
			return nil, fmt.Errorf("run %d failed", n)
		}
		result := &cache.ActionResult{Epoch: int64(n)}
		if b.content != nil {
			if result.Log, err = log.Save(ctx, b.content); err != nil {
				return nil, err
			}
		}
		return &Report{Result: result}, nil
	case <-ctx.Done():
		b.mu.Lock()
		b.cancels++
		b.mu.Unlock()
		return nil, ctx.Err()
	}
}

// outputs collects what an Action prints.
type outputs struct {
	mu   sync.Mutex
	data []byte
}

// Output keeps data.
func (o *outputs) Output(_ byte, data []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.data = append(o.data, data...)
}

// String returns what was printed.
func (o *outputs) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return string(o.data)
}

// testRequest returns a request for a task Action, keyed, forced if force.
func testRequest(force bool) Request {
	a := &Action{Version: ActionVersion, Kind: KindTask, Base: Scratch}
	return Request{Action: a, Key: a.Key(), Force: force}
}

// testSharing returns a Sharing Runner over r, on stores in memory, which it returns too.
func testSharing(r Runner) (*Sharing, *cache.Memory) {
	m := cache.NewMemory()
	return NewSharing(r, m, m), m
}

// TestSharingDeduplicates checks that two jobs needing the same Action run it once and both get its result,
// and that cancelling one of them leaves the other to finish with that result.
func TestSharingDeduplicates(t *testing.T) {
	b := &blockingRunner{release: make(chan struct{})}
	s, _ := testSharing(b)
	first, cancelFirst := context.WithCancel(t.Context())
	results := make(chan *cache.ActionResult, 2)
	errs := make(chan error, 2)
	for _, ctx := range []context.Context{first, t.Context()} {
		go func() {
			r, err := s.attempt(ctx, testRequest(false), &outputs{})
			results <- r
			errs <- err
		}()
	}
	waitFor(t, func() bool { return b.runs.Load() == 1 && attachedCount(s) == 2 })
	cancelFirst()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Errorf("the cancelled job got %v, want %v", err, context.Canceled)
	}
	<-results
	waitFor(t, func() bool { return attachedCount(s) == 1 })
	close(b.release)
	if r, err := <-results, <-errs; err != nil || r == nil || r.Epoch != 1 {
		t.Errorf("the other job got %+v, %v; want the shared attempt's result", r, err)
	}
	b.mu.Lock()
	cancels := b.cancels
	b.mu.Unlock()
	if b.runs.Load() != 1 || cancels != 0 {
		t.Errorf("the Runner ran %d times with %d cancelled, want once, not cancelled", b.runs.Load(), cancels)
	}
}

// TestSharingNoAttachToCancelled checks that a job arriving after an attempt's last job detached starts
// an attempt of its own, rather than attaching to the cancelled one.
func TestSharingNoAttachToCancelled(t *testing.T) {
	b := &blockingRunner{release: make(chan struct{})}
	s, _ := testSharing(b)
	first, cancelFirst := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := s.attempt(first, testRequest(false), &outputs{})
		done <- err
	}()
	waitFor(t, func() bool { return b.runs.Load() == 1 })
	cancelFirst()
	<-done
	second := make(chan *cache.ActionResult, 1)
	go func() {
		r, err := s.attempt(t.Context(), testRequest(false), &outputs{})
		testenv.Must(t, err)
		second <- r
	}()
	waitFor(t, func() bool { return b.runs.Load() == 2 })
	close(b.release)
	if r := <-second; r.Epoch != 2 {
		t.Errorf("the later job got result %d, want its own attempt's, 2", r.Epoch)
	}
}

// TestSharingCancelsWhenAlone checks that an attempt is cancelled once no job is attached to it.
func TestSharingCancelsWhenAlone(t *testing.T) {
	b := &blockingRunner{release: make(chan struct{})}
	s, _ := testSharing(b)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := s.attempt(ctx, testRequest(false), &outputs{})
		done <- err
	}()
	waitFor(t, func() bool { return b.runs.Load() == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("the job got %v, want %v", err, context.Canceled)
	}
	// The last job to detach waited for the attempt, so it has ended and been cancelled by now.
	b.mu.Lock()
	cancels := b.cancels
	b.mu.Unlock()
	if cancels != 1 {
		t.Errorf("the attempt was cancelled %d times by the time the job returned, want once", cancels)
	}
	s.mu.Lock()
	inflight := len(s.inflight)
	s.mu.Unlock()
	if inflight != 0 {
		t.Errorf("%d attempts in flight after the last job detached", inflight)
	}
}

// TestSharingForced checks that a forced run never attaches, and that an ordinary job attaches to a forced attempt:
// in both orders each job gets its own attempt's result, and the record, which Sharing writes, is the later write's.
func TestSharingForced(t *testing.T) {
	records := cache.NewMemory()
	for _, tc := range []struct {
		name       string
		first      bool // forced
		second     bool
		runs       int32
		sameResult bool
	}{
		{name: "ordinary then forced", first: false, second: true, runs: 2, sameResult: false},
		{name: "forced then ordinary", first: true, second: false, runs: 1, sameResult: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &blockingRunner{release: make(chan struct{}), gated: true}
			s := NewSharing(b, records, records)
			results := make([]*cache.ActionResult, 2)
			errs := make([]error, 2)
			var wg sync.WaitGroup
			start := func(i int, force bool) {
				wg.Go(func() {
					results[i], errs[i] = s.attempt(t.Context(), testRequest(force), &outputs{})
				})
				// A forced job starts its own run; an ordinary one attaches to the attempt in flight.
				waitFor(t, func() bool {
					if force || i == 0 {
						return b.runs.Load() == int32(i+1)
					}
					return attachedCount(s) == 2
				})
			}
			start(0, tc.first)
			start(1, tc.second)
			if b.runs.Load() != tc.runs {
				t.Errorf("the Runner ran %d times, want %d", b.runs.Load(), tc.runs)
			}
			// The runs are released in order, so the later one writes the record last.
			for n := int32(1); n <= tc.runs; n++ {
				b.releaseRun(n)
				waitFor(t, func() bool {
					record, err := records.Result(t.Context(), testRequest(false).Action.Key())
					return err == nil && record != nil && record.Epoch == int64(n)
				})
			}
			wg.Wait()
			if errs[0] != nil || errs[1] != nil {
				t.Fatalf("the jobs failed: %v, %v", errs[0], errs[1])
			}
			if (results[0].Epoch == results[1].Epoch) != tc.sameResult {
				t.Errorf("results %d and %d; want the same = %v", results[0].Epoch, results[1].Epoch, tc.sameResult)
			}
			// The record holds the result published last: the later of the runs to finish.
			record, err := records.Result(t.Context(), testRequest(false).Action.Key())
			testenv.Must(t, err)
			if record == nil || record.Epoch != int64(tc.runs) {
				t.Errorf("record = %+v, want the result of run %d, the later write", record, tc.runs)
			}
		})
	}
}

// TestSharingOutputReachesAttached checks that what a shared attempt prints reaches the job that started it live,
// and a job that attached later as the result's log, whole, once the attempt has succeeded.
func TestSharingOutputReachesAttached(t *testing.T) {
	content := cache.NewMemory()
	b := &blockingRunner{release: make(chan struct{}), content: content}
	s := NewSharing(b, content, content)
	starter, attacher := &outputs{}, &outputs{}
	var wg sync.WaitGroup
	wg.Go(func() { s.attempt(t.Context(), testRequest(false), starter) })
	waitFor(t, func() bool { return starter.String() == "started\n" })
	attached := make(chan struct{})
	wg.Go(func() {
		r, err := s.attempt(t.Context(), testRequest(false), attacher)
		if err != nil || r == nil || r.Log == nil {
			t.Errorf("the later job got %+v, %v; want the shared result, with its log", r, err)
		}
		if got := attacher.String(); got != "started\ndone\n" {
			t.Errorf("the later job got %q once the attempt ended, want its whole log", got)
		}
		close(attached)
	})
	waitFor(t, func() bool { return attachedCount(s) == 2 })
	close(b.release)
	<-attached
	wg.Wait()
	if got, want := starter.String(), "started\ndone\n"; got != want {
		t.Errorf("the starter got %q, want %q", got, want)
	}
	if b.runs.Load() != 1 {
		t.Errorf("the Runner ran %d times, want once", b.runs.Load())
	}
}

// TestSharingForcedOverlap checks that a forced attempt that overlaps an ordinary one becomes the attempt that later
// jobs attach to, even once the ordinary attempt has failed: an ordinary job arriving then starts no third run.
func TestSharingForcedOverlap(t *testing.T) {
	b := &blockingRunner{release: make(chan struct{}), fail: map[int32]bool{1: true}}
	s, _ := testSharing(b)
	results := make([]*cache.ActionResult, 3)
	errs := make([]error, 3)
	var wg sync.WaitGroup
	for i, force := range []bool{false, true} {
		wg.Go(func() { results[i], errs[i] = s.attempt(t.Context(), testRequest(force), &outputs{}) })
		waitFor(t, func() bool { return b.runs.Load() == int32(i+1) })
	}
	b.releaseRun(1)
	waitFor(t, func() bool { return attachedCount(s) == 1 })
	wg.Go(func() { results[2], errs[2] = s.attempt(t.Context(), testRequest(false), &outputs{}) })
	waitFor(t, func() bool { return attachedCount(s) == 2 })
	b.releaseRun(2)
	wg.Wait()
	if errs[0] == nil {
		t.Errorf("the ordinary job got %+v, want its attempt's failure", results[0])
	}
	if errs[1] != nil || errs[2] != nil || results[1].Epoch != 2 || results[2].Epoch != 2 {
		t.Errorf("the forced and later jobs got %+v, %v and %+v, %v; want the forced attempt's result",
			results[1], errs[1], results[2], errs[2])
	}
	if b.runs.Load() != 2 {
		t.Errorf("the Runner ran %d times, want twice", b.runs.Load())
	}
}

// TestSharingStarterCancelledTrace checks that once the job that started a shared attempt is cancelled,
// while another remains attached, the attempt's spans no longer reach the starter's trace,
// and the other job still gets the result.
func TestSharingStarterCancelledTrace(t *testing.T) {
	b := &blockingRunner{release: make(chan struct{})}
	s, _ := testSharing(b)
	var mu sync.Mutex
	var names []string
	traced := tracing.WithRecorder(t.Context(), tracing.NewRecorder("executor", func(span tracing.Span) {
		mu.Lock()
		defer mu.Unlock()
		names = append(names, span.Name)
	}), "")
	starter, cancel := context.WithCancel(traced)
	starterDone := make(chan error, 1)
	go func() {
		_, err := s.attempt(starter, testRequest(false), &outputs{})
		starterDone <- err
	}()
	waitFor(t, func() bool { return b.runs.Load() == 1 })
	other := make(chan *cache.ActionResult, 1)
	go func() {
		r, err := s.attempt(t.Context(), testRequest(false), &outputs{})
		testenv.Must(t, err)
		other <- r
	}()
	waitFor(t, func() bool { return attachedCount(s) == 2 })
	cancel()
	if err := <-starterDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("the starter got %v, want %v", err, context.Canceled)
	}
	close(b.release)
	if r := <-other; r == nil || r.Epoch != 1 {
		t.Errorf("the other job got %+v, want the shared attempt's result", r)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(names, []string{"started"}) {
		t.Errorf("the starter's trace got spans %q, want only those that ended while it was attached", names)
	}
}

// failingRecords is a record store whose writes fail while fail is set.
type failingRecords struct {
	cache.RecordStore
	fail atomic.Bool
}

// errDiskFull is the failure of a failingRecords write.
var errDiskFull = errors.New("disk full")

// SetResult fails while fail is set, and records r otherwise.
func (r *failingRecords) SetResult(ctx context.Context, key digest.Digest, result *cache.ActionResult) error {
	if r.fail.Load() {
		return errDiskFull
	}
	return r.RecordStore.SetResult(ctx, key, result)
}

// SetTag fails while fail is set, and records tag otherwise.
func (r *failingRecords) SetTag(ctx context.Context, name string, tag *cache.TagRecord) error {
	if r.fail.Load() {
		return errDiskFull
	}
	return r.RecordStore.SetTag(ctx, name, tag)
}

// resolvingRunner reports the results of a blocking runner as a resolve's, with the record of the tag it resolved.
type resolvingRunner struct {
	*blockingRunner
}

// run runs through the blocking runner, and adds the tag's record to its report.
func (r resolvingRunner) Run(ctx context.Context, req Request, ev Output) (*Report, error) {
	rep, err := r.blockingRunner.Run(ctx, req, ev)
	if err != nil {
		return nil, err
	}
	rep.Tag, rep.TagRecord = req.Action.Image, &cache.TagRecord{Digest: digest.FromString("index")}
	return rep, nil
}

// TestSharingPublishFails checks that an attempt whose result, or tag record, can't be written fails every job
// attached to it, a result's failure keeping the log the attempt saved, and that a job after the store recovers
// runs the action again and records what it made.
func TestSharingPublishFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		resolve bool
	}{
		{name: "result"},
		{name: "tag", resolve: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := cache.NewMemory()
			records := &failingRecords{RecordStore: m}
			records.fail.Store(true)
			b := &blockingRunner{release: make(chan struct{}), content: m}
			var r Runner = b
			req := testRequest(false)
			if tc.resolve {
				r = resolvingRunner{b}
				req = Request{Action: &Action{Version: ActionVersion, Kind: KindResolve,
					Image: "example.com/base:v1", Platform: "linux/amd64"}}
			}
			s := NewSharing(r, m, records)
			errs := make(chan error, 2)
			for range 2 {
				go func() {
					_, err := s.attempt(t.Context(), req, &outputs{})
					errs <- err
				}()
			}
			waitFor(t, func() bool { return b.runs.Load() == 1 && attachedCount(s) == 2 })
			close(b.release)
			for range 2 {
				err := <-errs
				if !errors.Is(err, errDiskFull) || !strings.Contains(err.Error(), "saving to the build cache") {
					t.Errorf("an attached job got %v, want the failure to save its record", err)
					continue
				}
				if tc.resolve {
					continue
				}
				var logged *loggedError
				if !errors.As(err, &logged) {
					t.Errorf("the failure %v carries no log, want the attempt's", err)
				} else if log, err := m.Read(t.Context(), *logged.log, cache.MaxLogSize); err != nil ||
					!strings.Contains(string(log), "done") {
					t.Errorf("the failure's log = %q, %v; want what the attempt printed", log, err)
				}
			}

			records.fail.Store(false)
			if _, err := s.attempt(t.Context(), req, &outputs{}); err != nil {
				t.Fatalf("the job after the store recovered got %v", err)
			}
			if n := b.runs.Load(); n != 2 {
				t.Errorf("the runner ran %d times, want twice: the failed attempt made nothing to reuse", n)
			}
			if tc.resolve {
				if tag, err := m.Tag(t.Context(), req.Action.Image); tag == nil || err != nil {
					t.Errorf("the tag's record after the retry = %v, %v; want it recorded", tag, err)
				}
			} else if result, err := m.Result(t.Context(), req.Key); result == nil || result.Epoch != 2 || err != nil {
				t.Errorf("the result after the retry = %+v, %v; want the second run's", result, err)
			}
		})
	}
}

// attachedCount returns how many jobs are attached to attempts in flight.
func attachedCount(s *Sharing) int {
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

// waitFor waits until cond holds, failing the test if it doesn't within a few seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for start := time.Now(); !cond(); {
		if time.Since(start) > 5*time.Second {
			t.Fatal("the condition didn't hold in time")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestSharingWithdrawnListeners checks that a job whose demand was withdrawn neither keeps an attempt demanded nor
// lets it run emulated for the jobs still attached, which don't permit emulation.
func TestSharingWithdrawnListeners(t *testing.T) {
	withdrawn := make(chan struct{})
	close(withdrawn)
	permitting := &listener{permitted: true, withdrawn: withdrawn}
	other := &listener{withdrawn: make(chan struct{})}
	at := &attempt{attached: []*listener{permitting, other}}
	if !at.demanded() {
		t.Error("the attempt isn't demanded, though a job still wants it")
	}
	if at.permitted() {
		t.Error("the attempt is permitted to emulate by a job whose demand was withdrawn")
	}
	at.remove(other)
	if at.demanded() {
		t.Error("the attempt is demanded by a job whose demand was withdrawn")
	}
	if !(&attempt{attached: []*listener{{permitted: true}}}).permitted() {
		t.Error("a job whose demand never lapses doesn't permit the attempt to emulate")
	}
}

// joins records whose attempt a job's action ran in, as Sharing tells a Joiner, besides what it prints.
type joins struct {
	outputs
	starters []Output
}

// Join keeps starter.
func (j *joins) Join(starter Output) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.starters = append(j.starters, starter)
}

// TestSharingNativeOverEmulated checks that a job that runs an action natively never attaches to an emulated attempt
// of its key, however often emulated attempts replace the one in flight, but starts its own, which later jobs attach
// to, emulating ones included; and that each job is told whose attempt it runs in.
func TestSharingNativeOverEmulated(t *testing.T) {
	b := &blockingRunner{gated: true}
	s, _ := testSharing(b)
	emulated := func(force bool) Request {
		req := testRequest(force)
		req.Emulated, req.Permitted = true, true
		return req
	}
	type job struct {
		out    *joins
		result chan *cache.ActionResult
	}
	start := func(req Request, runs int32, attached int) job {
		j := job{out: &joins{}, result: make(chan *cache.ActionResult, 1)}
		go func() {
			r, err := s.attempt(t.Context(), req, j.out)
			if err != nil {
				t.Error(err)
			}
			j.result <- r
		}()
		waitFor(t, func() bool { return b.runs.Load() == runs && attachedCount(s) == attached })
		return j
	}
	// Only the attempt in flight of the key counts its jobs.
	e1 := start(emulated(false), 1, 1)
	n1 := start(testRequest(false), 2, 1) // its own, though e1's is in flight, which it replaces
	e2 := start(emulated(false), 2, 2)    // attaches to n1's
	e3 := start(emulated(true), 3, 1)     // forced: its own, in place of n1's
	n2 := start(testRequest(false), 4, 1) // its own again, though e3's is in flight
	for n := int32(1); n <= 4; n++ {
		b.releaseRun(n)
	}
	for _, tc := range []struct {
		name    string
		j       job
		result  int64
		starter Output
	}{
		{name: "the first emulating job", j: e1, result: 1},
		{name: "the first native job", j: n1, result: 2},
		{name: "the emulating job after it", j: e2, result: 2, starter: n1.out},
		{name: "the forced emulating job", j: e3, result: 3},
		{name: "the second native job", j: n2, result: 4},
	} {
		select {
		case r := <-tc.j.result:
			if r == nil || r.Epoch != tc.result {
				t.Errorf("%s got %+v, want the result of run %d", tc.name, r, tc.result)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s never ended", tc.name)
		}
		tc.j.out.mu.Lock()
		if len(tc.j.out.starters) != 1 || tc.j.out.starters[0] != tc.starter {
			t.Errorf("%s was told it joined %v, want %v alone", tc.name, tc.j.out.starters, tc.starter)
		}
		tc.j.out.mu.Unlock()
	}
}
