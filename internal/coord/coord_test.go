package coord

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/fileset"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/remote"
	"github.com/Topher-the-Geek/vamos/internal/sched"

	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// amd64 and arm64 are the platforms the tests' workers run.
var (
	amd64 = imgspecv1.Platform{OS: "linux", Architecture: "amd64"}
	arm64 = imgspecv1.Platform{OS: "linux", Architecture: "arm64"}
)

// fixture is a coordinator served over HTTP, at url, with its stores in memory.
type fixture struct {
	c   *Coordinator
	m   *cache.Memory
	url string
}

// newFixture returns a coordinator configured by cfg, with short leases unless cfg sets them.
func newFixture(t *testing.T, cfg Config) *fixture {
	t.Helper()
	if cfg.Lease == 0 {
		cfg.Lease = 300 * time.Millisecond
		cfg.Heartbeat = 50 * time.Millisecond
	}
	m := cache.NewMemory()
	c, err := New(m, m, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	server := httptest.NewServer((&remote.Server{Executor: c, Content: m, Workers: c.Workers()}).Handler())
	t.Cleanup(server.Close)
	return &fixture{c: c, m: m, url: server.URL}
}

// fakeWorker is a worker that runs attempts with a scripted handler, over the worker-facing API.
type fakeWorker struct {
	t        *testing.T
	f        *fixture
	client   *remote.WorkerClient
	reg      remote.Registration
	platform imgspecv1.Platform
	// handle runs an assignment and returns its report; it is given a function that reports whether the attempt
	// was told to die, and may block on gates.
	handle func(a *remote.Assignment, killed func() bool) remote.AttemptReport
	// silent makes the worker stop heartbeating once it has taken an attempt, as a dead worker would.
	silent bool
	// maxTake, if set, is how many attempts the worker takes before it stops asking for more.
	maxTake int

	mu       sync.Mutex
	taken    []string // the attempts taken, in order
	reported []error  // the answers to each report
	stop     context.CancelFunc
	done     chan struct{}
}

// startWorker registers a worker of platform with the fixture, and runs it until the test ends.
func startWorker(t *testing.T, f *fixture, platform imgspecv1.Platform, capacity int,
	handle func(a *remote.Assignment, killed func() bool) remote.AttemptReport,
	emulates ...imgspecv1.Platform) *fakeWorker {
	t.Helper()
	w := newWorker(t, f, platform, capacity, handle, emulates...)
	w.start()
	return w
}

// newWorker registers a worker of platform, which emulates the platforms emulates, with the fixture,
// to be started once its options are set.
func newWorker(t *testing.T, f *fixture, platform imgspecv1.Platform, capacity int,
	handle func(a *remote.Assignment, killed func() bool) remote.AttemptReport,
	emulates ...imgspecv1.Platform) *fakeWorker {
	t.Helper()
	client, err := remote.NewWorkerClient(f.url)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := client.Register(t.Context(), remote.WorkerInfo{
		Platform: platform, Emulates: emulates, Isolation: "chroot", Version: "test",
		ActionVersion: sched.ActionVersion, Capacity: capacity,
	})
	if err != nil {
		t.Fatal(err)
	}
	w := &fakeWorker{t: t, f: f, client: client, reg: reg, platform: platform, handle: handle,
		done: make(chan struct{})}
	t.Cleanup(func() { client.Close() })
	return w
}

// start runs the worker until the test ends.
func (w *fakeWorker) start() {
	ctx, cancel := context.WithCancel(context.Background())
	w.stop = cancel
	go w.run(ctx)
	w.t.Cleanup(func() {
		cancel()
		<-w.done
	})
}

// run polls for assignments and runs each in a goroutine of its own, with heartbeats, until ctx ends.
func (w *fakeWorker) run(ctx context.Context) {
	defer close(w.done)
	var wg sync.WaitGroup
	defer wg.Wait()
	for ctx.Err() == nil {
		if w.maxTake > 0 && w.takenCount() >= w.maxTake {
			<-ctx.Done()
			return
		}
		a, err := w.client.NextAssignment(ctx, w.reg.ID)
		if err != nil {
			if ctx.Err() == nil {
				w.t.Logf("worker %s: %v", w.reg.ID, err)
			}
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if a == nil {
			continue
		}
		w.mu.Lock()
		w.taken = append(w.taken, a.Attempt)
		w.mu.Unlock()
		wg.Go(func() { w.attempt(ctx, a) })
	}
}

// attempt runs a, heartbeating meanwhile unless silent, and reports how it ended.
func (w *fakeWorker) attempt(ctx context.Context, a *remote.Assignment) {
	var killed sync.Mutex
	kill := false
	isKilled := func() bool {
		killed.Lock()
		defer killed.Unlock()
		return kill
	}
	beat, stopBeat := context.WithCancel(ctx)
	defer stopBeat()
	if !w.silent {
		go func() {
			ticker := time.NewTicker(w.reg.Heartbeat)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					answer, err := w.client.Heartbeat(beat, a.Attempt, remote.Heartbeat{})
					if err != nil || answer.Kill {
						killed.Lock()
						kill = true
						killed.Unlock()
					}
				case <-beat.Done():
					return
				}
			}
		}()
	}
	report := w.handle(a, isKilled)
	err := w.client.Report(context.Background(), a.Attempt, report)
	w.mu.Lock()
	w.reported = append(w.reported, err)
	w.mu.Unlock()
}

// takenCount returns how many attempts the worker has taken.
func (w *fakeWorker) takenCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.taken)
}

// fakeRun returns a handler that runs attempts as sched's fake runner would: resolves name images by their index,
// layers images by their keys, with a registry location, and tasks make a tree of one file in the fixture's store.
// Each layer or task waits for gate, if not nil, before reporting.
func fakeRun(f *fixture, gate func(a *remote.Assignment, killed func() bool) error) func(
	a *remote.Assignment, killed func() bool) remote.AttemptReport {
	return func(a *remote.Assignment, killed func() bool) remote.AttemptReport {
		ctx := context.Background()
		req := a.Request
		if req == nil {
			return remote.AttemptReport{Pushed: &api.Pushed{Image: a.Push.Result.Image}}
		}
		act := req.Action
		if act.Kind == sched.KindResolve {
			index := act.Index
			if index == "" && !req.Again && req.Tag != nil {
				index = req.Tag.Digest
			}
			if index == "" {
				index = digest.FromString("index of " + act.Image)
			}
			id := digest.FromString(index.String() + " " + act.Platform)
			location := cache.Location{Kind: cache.LocationRegistry, Reference: "hub-proxy:5000/base@" + id.String()}
			result := &cache.ActionResult{Image: id, Platform: act.Platform, Index: index, Epoch: 1,
				Locations: []cache.Location{location}}
			record := &cache.TagRecord{Digest: index, Instances: map[string]cache.TagInstance{
				act.Platform: {Digest: id, Image: id, Platform: act.Platform, Epoch: 1, Locations: result.Locations},
			}}
			return remote.AttemptReport{Report: &sched.Report{Result: result, Tag: act.Image, TagRecord: record}}
		}
		if gate != nil {
			if err := gate(a, killed); err != nil {
				return remote.AttemptReport{Failure: sched.FailureOf(err)}
			}
		}
		log, err := cache.NewLog()
		if err != nil {
			return remote.AttemptReport{Failure: sched.FailureOf(err)}
		}
		defer log.Discard()
		log.Writer(cache.StreamStdout, io.Discard).Write([]byte("ran " + req.Key.Encoded()[:8] + "\n"))
		result := &cache.ActionResult{Epoch: req.Epoch.Unix()}
		if result.Log, err = log.Save(ctx, f.m); err != nil {
			return remote.AttemptReport{Failure: sched.FailureOf(err)}
		}
		switch act.Kind {
		case sched.KindLayer:
			result.Image, result.Platform = digest.FromString("image for "+req.Key.String()), act.Platform
			result.Locations = []cache.Location{{Kind: cache.LocationRegistry,
				Reference: "registry:5000/vamos-cache:" + result.Image.Encoded()}}
		case sched.KindTask:
			if act.Output != nil {
				file, err := f.m.PutBytes(ctx, []byte("made by "+req.Key.String()))
				if err != nil {
					return remote.AttemptReport{Failure: sched.FailureOf(err)}
				}
				tree, err := f.m.PutBytes(ctx, (&cache.Tree{Entries: []cache.TreeEntry{
					{Path: "f", Type: cache.EntryFile, Mode: 0o644, Blob: &file},
				}}).Encode())
				if err != nil {
					return remote.AttemptReport{Failure: sched.FailureOf(err)}
				}
				result.Output = &tree
			}
		}
		return remote.AttemptReport{Report: &sched.Report{Result: result}}
	}
}

// testGraph returns a graph for amd64 of a pull, two independent tasks on it, and a layer that adds both:
// nodes 0 the pull, 1 and 2 the tasks, 3 the layer, the root.
func testGraph(t *testing.T, f *fixture, nonce string) api.GraphRequest {
	t.Helper()
	return testGraphFor(t, f, nonce, amd64)
}

// testGraphFor returns testGraph's graph for platform.
func testGraphFor(t *testing.T, f *fixture, nonce string, platform imgspecv1.Platform) api.GraphRequest {
	t.Helper()
	name := platforms.Format(platform)
	sel := fileset.DefaultSelection()
	sel.Include = []string{"."}
	from := func(ref graph.Ref) *graph.Ref { return &ref }
	g := &graph.Graph{Version: graph.Version, ExecPlatform: name, Roots: []graph.Ref{3}, Nodes: []graph.Node{
		{Label: "base", Desc: `pull "base"`, Pull: &graph.Pull{Image: "example.com/base:v1", Platform: name}},
		{Label: "one", Desc: `task "one"`, Task: &graph.Task{Base: 0, Steps: []graph.Step{
			{Op: graph.OpRun, Display: `run(["one"])`, Argv: []string{"one", nonce}},
		}, Output: &graph.Output{Source: "/out", Selection: sel}}},
		{Label: "two", Desc: `task "two"`, Task: &graph.Task{Base: 0, Steps: []graph.Step{
			{Op: graph.OpRun, Display: `run(["two"])`, Argv: []string{"two", nonce}},
		}, Output: &graph.Output{Source: "/out", Selection: sel}}},
		{Label: "app", Desc: `layer "app"`, Layer: &graph.Layer{Base: 0, Platform: name, Steps: []graph.Step{
			{Op: graph.OpAdd, Display: `add("one", "/one")`, From: from(1), Dest: "/one"},
			{Op: graph.OpAdd, Display: `add("two", "/two")`, From: from(2), Dest: "/two"},
		}}},
	}}
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	stored, err := f.m.PutBytes(t.Context(), graph.Encode(g))
	if err != nil {
		t.Fatal(err)
	}
	return api.GraphRequest{Graph: stored, Version: graph.Version, Platform: platform}
}

// runJob submits req and watches it to its end, returning its outcome and the statuses reported.
func runJob(t *testing.T, c *Coordinator, req api.GraphRequest) (api.Outcome, []string) {
	t.Helper()
	ctx := t.Context()
	id, err := c.Submit(ctx, api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	var statuses []string
	if err := c.Watch(ctx, id, 0, func(e api.Event) error {
		if e.Kind == api.EventStatus {
			statuses = append(statuses, fmt.Sprintf("%v: %s", e.Nodes, e.Status))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	outcome, err := c.Result(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return outcome, statuses
}

// waitFor waits until cond holds, failing the test if it doesn't within a few seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for start := time.Now(); !cond(); {
		if time.Since(start) > 10*time.Second {
			t.Fatal("the condition didn't hold in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestBuildsAcrossWorkers checks that a build's independent targets run at once on different workers, that the
// result is assembled with the workers' locations, and that a second build is a hit without asking any worker.
func TestBuildsAcrossWorkers(t *testing.T) {
	f := newFixture(t, Config{})
	var mu sync.Mutex
	running := map[string]bool{} // attempts running now, by worker
	both := make(chan struct{})
	gate := func(a *remote.Assignment, killed func() bool) error {
		mu.Lock()
		running[a.Attempt] = true
		n := len(running)
		mu.Unlock()
		if n == 2 {
			close(both)
		}
		select {
		case <-both:
		case <-time.After(5 * time.Second):
			return errors.New("the other task never ran at once")
		}
		return nil
	}
	w1 := startWorker(t, f, amd64, 1, fakeRun(f, gate))
	w2 := startWorker(t, f, amd64, 1, fakeRun(f, gate))
	outcome, statuses := runJob(t, f.c, testGraph(t, f, "cold"))
	if outcome.Failure != nil {
		t.Fatalf("outcome = %+v", outcome.Failure)
	}
	if len(outcome.Results) != 1 || outcome.Results[0].Image == "" || len(outcome.Results[0].Locations) != 1 ||
		outcome.Results[0].Locations[0].Kind != cache.LocationRegistry {
		t.Errorf("results = %+v, want the layer's image with its registry location", outcome.Results)
	}
	if w1.takenCount() == 0 || w2.takenCount() == 0 {
		t.Errorf("the workers took %d and %d attempts, want both to have run some", w1.takenCount(), w2.takenCount())
	}
	for _, want := range []string{"[1]: building", "[2]: building", "[3]: building"} {
		if !slices.Contains(statuses, want) {
			t.Errorf("statuses = %q, want %q", statuses, want)
		}
	}

	// Every target, the pull included, is a hit: no worker is asked anything.
	taken := w1.takenCount() + w2.takenCount()
	outcome, statuses = runJob(t, f.c, testGraph(t, f, "cold"))
	if outcome.Failure != nil {
		t.Fatalf("the warm outcome = %+v", outcome.Failure)
	}
	for _, want := range []string{"[1]: cached", "[2]: cached", "[3]: cached"} {
		if !slices.Contains(statuses, want) {
			t.Errorf("warm statuses = %q, want %q", statuses, want)
		}
	}
	if w1.takenCount()+w2.takenCount() != taken {
		t.Errorf("the warm build asked workers for %d attempts, want none", w1.takenCount()+w2.takenCount()-taken)
	}
}

// TestRetriesLostWorker checks that an attempt whose worker stops heartbeating is lost once its lease expires and
// tried again on another worker, that the build succeeds, and that the lost worker's late report is discarded.
func TestRetriesLostWorker(t *testing.T) {
	f := newFixture(t, Config{Retries: 1})
	release := make(chan struct{})
	dead := newWorker(t, f, amd64, 1, fakeRun(f, func(a *remote.Assignment, killed func() bool) error {
		<-release // the attempt hangs until the test releases it, long after its lease
		return nil
	}))
	dead.silent = true
	dead.maxTake = 2 // the resolve, which it reports in time, and one task, which it never does
	dead.start()
	done := make(chan api.Outcome, 1)
	go func() {
		outcome, _ := runJob(t, f.c, testGraph(t, f, "lost"))
		done <- outcome
	}()
	// The dead worker takes the first task it is offered; once lost, a live worker takes the rest.
	waitFor(t, func() bool { return dead.takenCount() >= 2 }) // the resolve, then a task
	startWorker(t, f, amd64, 2, fakeRun(f, nil))
	var outcome api.Outcome
	select {
	case outcome = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the build didn't end")
	}
	if outcome.Failure != nil {
		t.Fatalf("outcome = %+v, want success after the retry", outcome.Failure)
	}
	close(release)
	waitFor(t, func() bool {
		dead.mu.Lock()
		defer dead.mu.Unlock()
		return len(dead.reported) >= 2
	})
	dead.mu.Lock()
	defer dead.mu.Unlock()
	if dead.reported[0] != nil || !errors.Is(dead.reported[1], remote.ErrStaleAttempt) {
		t.Errorf("the lost worker's reports were answered %v, want the resolve's accepted and the task's late one "+
			"refused as %v", dead.reported, remote.ErrStaleAttempt)
	}
}

// TestRetriesExhausted checks that work whose workers keep being lost fails the build once the retries are spent,
// naming the loss.
func TestRetriesExhausted(t *testing.T) {
	f := newFixture(t, Config{Retries: 1})
	for range 2 {
		w := newWorker(t, f, amd64, 1, fakeRun(f, func(*remote.Assignment, func() bool) error {
			time.Sleep(2 * time.Second)
			return nil
		}))
		w.silent = true
		w.start()
	}
	outcome, _ := runJob(t, f.c, testGraph(t, f, "exhausted"))
	if outcome.Failure == nil || !strings.Contains(outcome.Failure.Message, "the worker running it was lost") {
		t.Errorf("outcome = %+v, want the loss reported once the retries are spent", outcome)
	}
}

// TestCancelKillsAttempt checks that cancelling a build tells the worker, at its next heartbeat, to kill the attempt
// no build needs, that the build's terminal event reports the cancellation without waiting for the worker,
// and that an attempt another build still needs is left running.
func TestCancelKillsAttempt(t *testing.T) {
	f := newFixture(t, Config{})
	killedAt := make(chan string, 4)
	startWorker(t, f, amd64, 4, fakeRun(f, func(a *remote.Assignment, killed func() bool) error {
		for start := time.Now(); !killed(); {
			if time.Since(start) > 5*time.Second {
				return nil // never killed: finish normally
			}
			time.Sleep(5 * time.Millisecond)
		}
		killedAt <- a.Attempt
		return context.Canceled
	}))
	ctx := t.Context()
	req := testGraph(t, f, "cancel")
	first, err := f.c.Submit(ctx, api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	// A second build of the same graph attaches to the first's attempts.
	second, err := f.c.Submit(ctx, api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		f.c.pool.mu.Lock()
		defer f.c.pool.mu.Unlock()
		running := 0
		for _, at := range f.c.pool.attempts {
			if at.worker != nil && at.req != nil && at.req.Action.Kind != sched.KindResolve {
				running++
			}
		}
		return running == 2
	})
	start := time.Now()
	if err := f.c.Cancel(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Watch(ctx, second, 0, func(api.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if outcome, err := f.c.Result(ctx, second); err != nil || !outcome.Cancelled {
		t.Errorf("the cancelled build's outcome = %+v, %v; want it cancelled", outcome, err)
	}
	select {
	case at := <-killedAt:
		t.Fatalf("attempt %s was killed though the first build still needs it", at)
	case <-time.After(300 * time.Millisecond):
	}

	if err := f.c.Cancel(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Watch(ctx, first, 0, func(api.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	ended := time.Since(start)
	if outcome, err := f.c.Result(ctx, first); err != nil || !outcome.Cancelled {
		t.Errorf("the first build's outcome = %+v, %v; want it cancelled", outcome, err)
	}
	for range 2 {
		select {
		case <-killedAt:
		case <-time.After(5 * time.Second):
			t.Fatal("the attempts weren't told to die once no build needed them")
		}
	}
	if ended > 3*time.Second {
		t.Errorf("the cancelled builds took %s to end, want them not to wait for the workers' cleanup", ended)
	}
	// Once told to die, an attempt is ended and its worker's slot free, without waiting for its lease to expire.
	freed := time.Now()
	waitFor(t, func() bool {
		f.c.pool.mu.Lock()
		defer f.c.pool.mu.Unlock()
		for _, w := range f.c.pool.workers {
			if w.running != 0 {
				return false
			}
		}
		return len(f.c.pool.attempts) == 0
	})
	if took := time.Since(freed); took > f.c.cfg.Lease {
		t.Errorf("the killed attempts held their slots for %s after the kill, want them freed at the heartbeat", took)
	}
}

// TestPlatformRouting checks that work goes only to workers of the build's platform, that a build for a platform
// no worker runs yet waits for one, and that one for a platform the deployment doesn't offer is refused at submit.
func TestPlatformRouting(t *testing.T) {
	f := newFixture(t, Config{Platforms: []imgspecv1.Platform{amd64}})
	other := startWorker(t, f, arm64, 1, fakeRun(f, nil))
	req := testGraph(t, f, "routing")
	done := make(chan api.Outcome, 1)
	go func() {
		outcome, _ := runJob(t, f.c, req)
		done <- outcome
	}()
	select {
	case outcome := <-done:
		t.Fatalf("the build ended without a worker of its platform: %+v", outcome)
	case <-time.After(300 * time.Millisecond):
	}
	if other.takenCount() != 0 {
		t.Errorf("the arm64 worker took %d attempts of an amd64 build", other.takenCount())
	}
	startWorker(t, f, amd64, 1, fakeRun(f, nil))
	select {
	case outcome := <-done:
		if outcome.Failure != nil {
			t.Errorf("outcome = %+v, want success once a worker of the platform registered", outcome.Failure)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the build didn't end once a worker of its platform registered")
	}
	if other.takenCount() != 0 {
		t.Errorf("the arm64 worker took %d attempts of an amd64 build", other.takenCount())
	}

	refused := req
	refused.Platform = imgspecv1.Platform{OS: "linux", Architecture: "s390x"}
	_, err := f.c.Submit(t.Context(), api.Request{Graph: &refused})
	if err == nil || !strings.Contains(err.Error(), "linux/s390x, which this coordinator doesn't offer") {
		t.Errorf("Submit for an unoffered platform = %v, want it refused", err)
	}
}

// TestRequestPlatformMatchesGraph checks that a build whose graph was configured for another platform than its
// request names, which Submit checked, fails rather than waiting for a worker of the graph's.
func TestRequestPlatformMatchesGraph(t *testing.T) {
	f := newFixture(t, Config{})
	startWorker(t, f, amd64, 1, fakeRun(f, nil))
	g := &graph.Graph{Version: graph.Version, ExecPlatform: "linux/s390x", Roots: []graph.Ref{0}, Nodes: []graph.Node{
		{Label: "base", Desc: `pull "base"`, Pull: &graph.Pull{Image: "example.com/base:v1", Platform: "linux/s390x"}},
	}}
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	stored, err := f.m.PutBytes(t.Context(), graph.Encode(g))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan api.Outcome, 1)
	go func() {
		outcome, _ := runJob(t, f.c, api.GraphRequest{Graph: stored, Version: graph.Version, Platform: amd64})
		done <- outcome
	}()
	select {
	case outcome := <-done:
		if outcome.Failure == nil || !strings.Contains(outcome.Failure.Message,
			"the build graph was configured for an executor on linux/s390x, but its request names linux/amd64") {
			t.Errorf("outcome = %+v, want the disagreement named", outcome.Failure)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the build waited for a worker of the graph's platform, which its request doesn't name")
	}
}

// TestWorkerRegistration checks that a worker of another action version, or without capacity, is refused,
// and that an unregistered worker's poll says so.
func TestWorkerRegistration(t *testing.T) {
	f := newFixture(t, Config{})
	client, err := remote.NewWorkerClient(f.url)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx := t.Context()
	if _, err := client.Register(ctx, remote.WorkerInfo{Platform: amd64, ActionVersion: sched.ActionVersion + 1,
		Capacity: 1}); err == nil || !strings.Contains(err.Error(), "action version") {
		t.Errorf("registering another action version = %v, want it refused", err)
	}
	_, err = client.Register(ctx, remote.WorkerInfo{Platform: amd64, ActionVersion: sched.ActionVersion})
	if err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Errorf("registering without capacity = %v, want it refused", err)
	}
	if _, err := client.NextAssignment(ctx, "nobody"); err == nil || !strings.Contains(err.Error(), "register again") {
		t.Errorf("polling as an unregistered worker = %v, want it told to register", err)
	}
	if _, err := client.Heartbeat(ctx, "nothing", remote.Heartbeat{}); !errors.Is(err, remote.ErrStaleAttempt) {
		t.Errorf("a heartbeat for an unknown attempt = %v, want %v", err, remote.ErrStaleAttempt)
	}
	caps, err := f.c.Capabilities(ctx)
	if err != nil || len(caps.Platforms) != 0 || caps.Instance == "" {
		t.Errorf("capabilities with no worker = %+v, %v; want no platform and an instance", caps, err)
	}
	startWorker(t, f, arm64, 1, fakeRun(f, nil))
	caps, _ = f.c.Capabilities(ctx)
	if len(caps.Platforms) != 1 || platforms.Format(caps.Platforms[0]) != "linux/arm64" {
		t.Errorf("capabilities with an arm64 worker = %v, want linux/arm64 offered", caps.Platforms)
	}
}

// TestFairness checks that pending work goes first to the build with the fewest attempts running.
func TestFairness(t *testing.T) {
	p := newPool(Config{}.withDefaults(), "i", func() (uint64, error) { return 0, nil })
	w := &worker{id: "w", info: remote.WorkerInfo{Platform: amd64, Capacity: 4}}
	p.workers[w.id] = w
	busy := buildInfo{job: "busy", platform: amd64}
	idle := buildInfo{job: "idle", platform: amd64}
	req := &sched.Request{Action: &sched.Action{Kind: sched.KindTask}}
	for range 2 {
		at := &attempt{id: newID(), build: busy, req: req, worker: w, done: make(chan struct{})}
		p.attempts[at.id] = at
	}
	ctx := t.Context()
	first := &attempt{id: "busy-pending", build: busy, req: req, trace: ctx, done: make(chan struct{})}
	second := &attempt{id: "idle-pending", build: idle, req: req, trace: ctx, done: make(chan struct{})}
	p.attempts[first.id], p.attempts[second.id] = first, second
	p.pending = []*attempt{first, second}
	if got := p.pick(w); got != second {
		t.Errorf("picked %s, want the idle build's attempt though it is younger", got.id)
	}
	if got := p.pick(w); got != first {
		t.Errorf("picked %v, want the busy build's attempt once nothing else is pending", got)
	}
}

// TestExpiredAttemptIsRefusedBeforeTheSweep checks that an attempt whose lease has passed is lost by the first
// heartbeat or events that meets it, without waiting for the sweep, so that none of them renews or ends it.
func TestExpiredAttemptIsRefusedBeforeTheSweep(t *testing.T) {
	for name, call := range map[string]func(p *pool, id string) error{
		"heartbeat": func(p *pool, id string) error {
			answer, err := p.Heartbeat(t.Context(), id, remote.Heartbeat{})
			if err != nil && !answer.Kill {
				t.Error("a refused heartbeat didn't tell the worker to kill")
			}
			return err
		},
		"events": func(p *pool, id string) error {
			return p.Events(t.Context(), id, remote.AttemptEvents{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			p := newPool(Config{}.withDefaults(), "i", func() (uint64, error) { return 0, nil })
			p.now = func() time.Time { return now }
			w := &worker{id: "w", info: remote.WorkerInfo{Platform: amd64, Capacity: 1}, running: 1}
			p.workers[w.id] = w
			at := &attempt{id: "a", worker: w, lease: now.Add(p.cfg.Lease), done: make(chan struct{})}
			p.attempts[at.id] = at
			now = now.Add(p.cfg.Lease / 2)
			if err := call(p, at.id); err != nil {
				t.Fatalf("before its lease passed, the call = %v, want it accepted", err)
			}
			now = at.lease.Add(time.Millisecond) // past the lease, though no sweep has run
			if err := call(p, at.id); !errors.Is(err, remote.ErrStaleAttempt) {
				t.Fatalf("after its lease passed, the call = %v, want %v", err, remote.ErrStaleAttempt)
			}
			select {
			case <-at.done:
			default:
				t.Fatal("the expired attempt wasn't ended, so its work isn't tried again")
			}
			if !errors.Is(at.err, errWorkerLost) || at.report != nil {
				t.Errorf("the attempt ended with %v and report %v, want the loss", at.err, at.report)
			}
			if w.running != 0 {
				t.Errorf("the worker still runs %d attempts, want its slot freed", w.running)
			}
		})
	}
}

// TestKilledAttemptSettles checks that an attempt ended by the heartbeat telling its worker to kill it still counts
// against a prune, since that answer may never arrive, until the worker reports or the lease the kill didn't renew
// has passed, while its worker's slot is free at once.
func TestKilledAttemptSettles(t *testing.T) {
	for name, settle := range map[string]func(p *pool, id string, lease time.Time, now *time.Time){
		"report": func(p *pool, id string, _ time.Time, _ *time.Time) {
			if err := p.Report(t.Context(), id, remote.AttemptReport{}); !errors.Is(err, remote.ErrStaleAttempt) {
				t.Errorf("the report of a killed attempt = %v, want %v", err, remote.ErrStaleAttempt)
			}
		},
		"lease": func(p *pool, _ string, lease time.Time, now *time.Time) {
			*now = lease.Add(time.Millisecond)
			p.expire()
		},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			p := newPool(Config{}.withDefaults(), "i", func() (uint64, error) { return 0, nil })
			p.now = func() time.Time { return now }
			w := &worker{id: "w", info: remote.WorkerInfo{Platform: amd64, Capacity: 1}, running: 1}
			p.workers[w.id] = w
			at := &attempt{id: "a", worker: w, lease: now.Add(p.cfg.Lease), done: make(chan struct{})}
			p.attempts[at.id] = at
			lease := at.lease
			p.withdraw(at)
			now = now.Add(p.cfg.Lease / 2)
			if answer, err := p.Heartbeat(t.Context(), at.id, remote.Heartbeat{}); err != nil || !answer.Kill {
				t.Fatalf("the heartbeat of a withdrawn attempt = %+v, %v; want it told to kill", answer, err)
			}
			if w.running != 0 {
				t.Errorf("the worker still runs %d attempts after the kill, want its slot freed", w.running)
			}
			// The answer is lost: the worker runs on until its fence, before the lease.
			if n := p.liveAttempts(); n != 1 {
				t.Errorf("after the kill was answered, %d attempts are live, want the killed one until it settles", n)
			}
			p.expire()
			if n := p.liveAttempts(); n != 1 {
				t.Errorf("a sweep before the lease left %d attempts live, want the killed one still", n)
			}
			settle(p, at.id, lease, &now)
			if n := p.liveAttempts(); n != 0 {
				t.Errorf("once the killed attempt settled, %d attempts are live, want none", n)
			}
		})
	}
}

// TestExpiredReportIsDiscarded checks that a report after the lease is refused and not recorded.
func TestExpiredReportIsDiscarded(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := newPool(Config{}.withDefaults(), "i", func() (uint64, error) { return 0, nil })
	p.now = func() time.Time { return now }
	w := &worker{id: "w", info: remote.WorkerInfo{Platform: amd64, Capacity: 1}, running: 1}
	at := &attempt{id: "a", worker: w, lease: now.Add(p.cfg.Lease), done: make(chan struct{})}
	p.attempts[at.id] = at
	now = at.lease.Add(time.Millisecond)
	if err := p.Report(t.Context(), at.id, remote.AttemptReport{}); !errors.Is(err, remote.ErrStaleAttempt) {
		t.Fatalf("a report after the lease = %v, want %v", err, remote.ErrStaleAttempt)
	}
	if at.report != nil || !errors.Is(at.err, errWorkerLost) {
		t.Errorf("the attempt ended with %v and report %v, want the loss and no report", at.err, at.report)
	}
}

// TestRetriesZero checks that no retries can be asked for, and that leaving them unset is still the default.
func TestRetriesZero(t *testing.T) {
	for _, c := range []struct {
		name    string
		retries int
		want    int
	}{
		{name: "unset", retries: 0, want: 2},
		{name: "none", retries: NoRetries, want: 0},
		{name: "three", retries: 3, want: 3},
	} {
		if got := (Config{Retries: c.retries}).withDefaults().Retries; got != c.want {
			t.Errorf("%s: retries = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestBuildExpires checks that a build no client watches for the grace period has its demand withdrawn: what runs
// finishes and is recorded, nothing more starts, the build fails saying it expired, and once the window has passed
// it is evicted and reported expired, not unknown. A released build is evicted without waiting for the window.
func TestBuildExpires(t *testing.T) {
	f := newFixture(t, Config{Grace: 400 * time.Millisecond, Window: 800 * time.Millisecond})
	gate := make(chan struct{})
	var started sync.Mutex
	ran := map[string]bool{} // the commands of the attempts that ran, by their first argument
	startWorker(t, f, amd64, 4, fakeRun(f, func(a *remote.Assignment, killed func() bool) error {
		if a.Request.Action.Kind != sched.KindTask {
			return nil // the resolve and the layer run at once
		}
		started.Lock()
		ran[a.Request.Action.Steps[0].Argv[0]] = true
		started.Unlock()
		<-gate
		return nil
	}))
	ctx := t.Context()
	req := testGraph(t, f, "expiring")
	id, err := f.c.Submit(ctx, api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	// Both tasks run; the build is never watched, so its demand lapses while they run.
	waitFor(t, func() bool {
		started.Lock()
		defer started.Unlock()
		return ran["one"] && ran["two"]
	})
	time.Sleep(600 * time.Millisecond)
	close(gate)
	if err := f.c.Watch(ctx, id, 0, func(api.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	outcome, err := f.c.Result(ctx, id)
	if err != nil || outcome.Failure == nil || !strings.Contains(outcome.Failure.Message, "expired") {
		t.Fatalf("outcome = %+v, %v; want a failure saying the build expired", outcome, err)
	}
	// The tasks that ran are recorded: a second build finds them cached, and runs the layer alone.
	outcome, statuses := runJob(t, f.c, testGraph(t, f, "expiring"))
	if outcome.Failure != nil {
		t.Fatalf("the second build = %+v", outcome.Failure)
	}
	for _, want := range []string{"[1]: cached", "[2]: cached", "[3]: building"} {
		if !slices.Contains(statuses, want) {
			t.Errorf("statuses = %q, want %q", statuses, want)
		}
	}
	// The expired build is evicted after the window, and remembered.
	waitFor(t, func() bool {
		_, err := f.c.Result(ctx, id)
		return errors.Is(err, api.ErrExpired)
	})
	if _, err := f.c.Result(ctx, "never"); !errors.Is(err, api.ErrNoJob) {
		t.Errorf("Result of an unknown job = %v, want %v", err, api.ErrNoJob)
	}
	// A watched build doesn't expire, and a released one is gone at the next sweep.
	third, err := f.c.Submit(ctx, api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.c.Watch(ctx, third, 0, func(api.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if outcome, err := f.c.Result(ctx, third); err != nil || outcome.Failure != nil {
		t.Fatalf("the watched build = %+v, %v; want success", outcome, err)
	}
	if err := f.c.Release(ctx, third); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, err := f.c.Result(ctx, third)
		return errors.Is(err, api.ErrExpired)
	})
}

// emulatedGraph returns a graph run on amd64 whose root is an arm64 layer that runs a command, so that its
// commands run under emulation.
func emulatedGraph(t *testing.T, f *fixture, nonce string) api.GraphRequest {
	t.Helper()
	g := &graph.Graph{Version: graph.Version, ExecPlatform: "linux/amd64", Roots: []graph.Ref{1}, Nodes: []graph.Node{
		{Label: "base", Desc: `pull "base"`, Pull: &graph.Pull{Image: "example.com/base:v1", Platform: "linux/arm64"}},
		{Label: "app", Desc: `layer "app"`, Layer: &graph.Layer{Base: 0, Platform: "linux/arm64", Steps: []graph.Step{
			{Op: graph.OpRun, Display: `run(["make"])`, Argv: []string{"make", nonce}},
		}}},
	}}
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	stored, err := f.m.PutBytes(t.Context(), graph.Encode(g))
	if err != nil {
		t.Fatal(err)
	}
	return api.GraphRequest{Graph: stored, Version: graph.Version, Platform: amd64}
}

// TestEmulationRules checks that a build without --emulate fails a target whose commands would run emulated before
// anything runs, that one with it fails at once when nobody emulates the platform, waits when the deployment offers
// emulation of it until a worker that emulates it registers, runs there and nowhere else, and that a build without
// the permission attaches to the emulating attempt in flight and is a hit afterwards.
func TestEmulationRules(t *testing.T) {
	t.Run("not permitted", func(t *testing.T) {
		f := newFixture(t, Config{})
		w := startWorker(t, f, amd64, 1, fakeRun(f, nil), arm64)
		outcome, _ := runJob(t, f.c, emulatedGraph(t, f, "refused"))
		if outcome.Failure == nil || !strings.Contains(outcome.Failure.Message, "its commands would run under "+
			"emulation: its image is for linux/arm64, and this build runs commands on linux/amd64") ||
			!strings.Contains(outcome.Failure.Hint, "--emulate") {
			t.Fatalf("outcome = %+v, want the mismatch message with both ways out", outcome.Failure)
		}
		if w.takenCount() != 1 {
			t.Errorf("the worker took %d attempts, want the resolve alone", w.takenCount())
		}
	})
	t.Run("nobody emulates", func(t *testing.T) {
		f := newFixture(t, Config{})
		startWorker(t, f, amd64, 1, fakeRun(f, nil))
		req := emulatedGraph(t, f, "nobody")
		req.Emulate = true
		outcome, _ := runJob(t, f.c, req)
		if outcome.Failure == nil || !strings.Contains(outcome.Failure.Message,
			"no worker of this deployment emulates linux/arm64") {
			t.Fatalf("outcome = %+v, want a failure saying nobody emulates the platform", outcome.Failure)
		}
	})
	t.Run("on scratch", func(t *testing.T) {
		// A layer on Scratch has no base platform: its own is what its commands run under.
		f := newFixture(t, Config{})
		native := startWorker(t, f, amd64, 1, fakeRun(f, nil))
		emulating := startWorker(t, f, amd64, 1, fakeRun(f, nil), arm64)
		g := &graph.Graph{Version: graph.Version, ExecPlatform: "linux/amd64", Roots: []graph.Ref{0},
			Nodes: []graph.Node{{Label: "app", Desc: `layer "app"`, Layer: &graph.Layer{Base: graph.Scratch,
				Platform: "linux/arm64", Steps: []graph.Step{
					{Op: graph.OpRun, Display: `run(["make"])`, Argv: []string{"make", "scratch"}},
				}}}}}
		if err := g.Validate(); err != nil {
			t.Fatal(err)
		}
		stored, err := f.m.PutBytes(t.Context(), graph.Encode(g))
		if err != nil {
			t.Fatal(err)
		}
		req := api.GraphRequest{Graph: stored, Version: graph.Version, Platform: amd64, Emulate: true}
		outcome, _ := runJob(t, f.c, req)
		if outcome.Failure != nil {
			t.Fatalf("outcome = %+v, want the layer built on the emulating worker", outcome.Failure)
		}
		if emulating.takenCount() != 1 || native.takenCount() != 0 {
			t.Errorf("the emulating worker took %d attempts and the native one %d; want the layer on the emulating "+
				"worker alone", emulating.takenCount(), native.takenCount())
		}
	})
	t.Run("offered", func(t *testing.T) {
		f := newFixture(t, Config{Emulated: []imgspecv1.Platform{arm64}})
		native := startWorker(t, f, amd64, 1, fakeRun(f, nil))
		gate := make(chan struct{})
		running := make(chan struct{}, 1)
		req := emulatedGraph(t, f, "offered")
		req.Emulate = true
		done := make(chan api.Outcome, 1)
		go func() {
			outcome, _ := runJob(t, f.c, req)
			done <- outcome
		}()
		select {
		case outcome := <-done:
			t.Fatalf("the build ended without a worker that emulates: %+v", outcome)
		case <-time.After(300 * time.Millisecond):
		}
		emulating := startWorker(t, f, amd64, 1, fakeRun(f, func(a *remote.Assignment, killed func() bool) error {
			running <- struct{}{}
			<-gate
			return nil
		}), arm64)
		<-running
		// A build without the permission attaches to the attempt in flight, building rather than hitting the cache.
		unpermitted := emulatedGraph(t, f, "offered")
		attached := make(chan api.Outcome, 1)
		attachedStatuses := make(chan []string, 1)
		go func() {
			outcome, statuses := runJob(t, f.c, unpermitted)
			attached <- outcome
			attachedStatuses <- statuses
		}()
		waitFor(t, func() bool { return f.c.sharing.Attached() == 2 })
		close(gate)
		for _, ch := range []chan api.Outcome{done, attached} {
			select {
			case outcome := <-ch:
				if outcome.Failure != nil {
					t.Errorf("outcome = %+v, want success", outcome.Failure)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("a build didn't end once the emulating worker finished")
			}
		}
		if statuses := <-attachedStatuses; !slices.Contains(statuses, "[1]: building") {
			t.Errorf("the attached build's statuses = %q, want the layer building, attached to the attempt", statuses)
		}
		if emulating.takenCount() == 0 || native.takenCount() > 1 {
			t.Errorf("the emulating worker took %d attempts and the native one %d; want the layer on the emulating "+
				"worker alone", emulating.takenCount(), native.takenCount())
		}
		// Afterwards the result is a hit for a build without the permission.
		outcome, statuses := runJob(t, f.c, unpermitted)
		if outcome.Failure != nil || !slices.Contains(statuses, "[1]: cached") {
			t.Errorf("a later unpermitted build = %+v with %q; want the layer cached", outcome.Failure, statuses)
		}
	})
}

// TestGenerationFencing checks that the capabilities report the cache generation, never zero, that a build captured
// against an earlier generation fails, the first included, that a report from an attempt that started before a
// prune is refused and fails the build, and that a prune is refused until a lease has passed since the coordinator
// started.
func TestGenerationFencing(t *testing.T) {
	f := newFixture(t, Config{Platforms: []imgspecv1.Platform{amd64}})
	ctx := t.Context()
	caps, err := f.c.Capabilities(ctx)
	if err != nil || caps.Generation != 1 {
		t.Fatalf("capabilities = %+v, %v; want generation 1", caps, err)
	}
	first := testGraph(t, f, "first")
	first.Generation = caps.Generation // captured before the first prune
	if _, err := f.c.Prune(ctx); !errors.Is(err, api.ErrSettling) {
		t.Errorf("a prune right after the start = %v, want %v", err, api.ErrSettling)
	}
	time.Sleep(f.c.cfg.Lease)
	id, err := f.c.Prune(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.c.Watch(ctx, id, 0, func(api.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if caps, err = f.c.Capabilities(ctx); err != nil || caps.Generation != 2 {
		t.Fatalf("capabilities after a prune = %+v, %v; want generation 2", caps, err)
	}
	stale := testGraph(t, f, "stale")
	stale.Generation = 7
	for name, req := range map[string]api.GraphRequest{"another": stale, "the first": first} {
		outcome, _ := runJob(t, f.c, req)
		if outcome.Failure == nil ||
			!strings.Contains(outcome.Failure.Message, "pruned while the build's sources were being captured") {
			t.Errorf("a build of %s generation = %+v, want it failed with a rerun message", name, outcome.Failure)
		}
	}
	current := testGraph(t, f, "current")
	current.Generation = 2

	// An attempt that started before the records were cleared reports in vain.
	gate := make(chan struct{})
	started := make(chan struct{}, 4)
	startWorker(t, f, amd64, 4, fakeRun(f, func(a *remote.Assignment, killed func() bool) error {
		started <- struct{}{}
		<-gate
		return nil
	}))
	jobID, err := f.c.Submit(ctx, api.Request{Graph: &current})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := f.m.ClearResults(ctx); err != nil {
		t.Fatal(err)
	}
	close(gate)
	if err := f.c.Watch(ctx, jobID, 0, func(api.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	outcome, err := f.c.Result(ctx, jobID)
	if err != nil || outcome.Failure == nil ||
		!strings.Contains(outcome.Failure.Message, "pruned while the attempt ran") {
		t.Errorf("outcome = %+v, %v; want a failure saying the cache was pruned while the attempt ran", outcome, err)
	}
}

// gatedGeneration wraps the fixture's stores so that a read of the generation, once armed, waits for a gate.
type gatedGeneration struct {
	*cache.Memory
	armed   atomic.Bool
	reading chan struct{} // receives once an armed read has begun
	gate    chan struct{} // closed to let an armed read through
}

func (g *gatedGeneration) Generation(ctx context.Context) (uint64, error) {
	if g.armed.Load() {
		g.reading <- struct{}{}
		<-g.gate
	}
	return g.Memory.Generation(ctx)
}

// TestGenerationCheckedOnceOpen checks that a build's generation is checked once the build is open: a prune
// while the check runs is refused, so that none can pass between the check and the build.
func TestGenerationCheckedOnceOpen(t *testing.T) {
	stores := &gatedGeneration{Memory: cache.NewMemory(), reading: make(chan struct{}, 1), gate: make(chan struct{})}
	c, err := New(stores, stores, Config{Lease: 50 * time.Millisecond, Heartbeat: 10 * time.Millisecond,
		Platforms: []imgspecv1.Platform{amd64}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	f := &fixture{c: c, m: stores.Memory}
	ctx := t.Context()
	time.Sleep(c.cfg.Lease) // the hold after starting
	req := testGraph(t, f, "checked")
	req.Generation = 1
	stores.armed.Store(true)
	submitted := make(chan error, 1)
	go func() {
		id, err := c.Submit(ctx, api.Request{Graph: &req})
		if err == nil {
			err = c.Cancel(ctx, id)
		}
		submitted <- err
	}()
	<-stores.reading
	stores.armed.Store(false)
	if _, err := c.Prune(ctx); err == nil || !strings.Contains(err.Error(), "held open") {
		t.Errorf("a prune while the build checks its generation = %v, want it refused", err)
	}
	close(stores.gate)
	if err := <-submitted; err != nil {
		t.Fatal(err)
	}
}

// TestWithdrawnWorkIsNotAssigned checks that pending work whose build was released is given to no worker that asks
// before the pool next looks at it, so that work nobody wants doesn't start in that interval.
func TestWithdrawnWorkIsNotAssigned(t *testing.T) {
	// The heartbeat is long, so that the pool doesn't take the work back while the worker asks.
	f := newFixture(t, Config{Lease: 30 * time.Second, Heartbeat: 10 * time.Second, Grace: 30 * time.Second,
		Platforms: []imgspecv1.Platform{amd64}})
	ctx := t.Context()
	req := testGraph(t, f, "withdrawn")
	id, err := f.c.Submit(ctx, api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return f.c.pool.liveAttempts() == 1 })
	if err := f.c.Release(ctx, id); err != nil {
		t.Fatal(err)
	}
	w := startWorker(t, f, amd64, 1, fakeRun(f, nil))
	time.Sleep(300 * time.Millisecond)
	if n := w.takenCount(); n != 0 {
		t.Errorf("the worker took %d attempts of a released build, want none", n)
	}
}

// TestExpiredPendingAttemptEnds checks that a build whose work waits for a worker that never comes ends once it
// expires: the pending attempt is taken back, so that nothing of it runs later and a prune isn't held up by it.
func TestExpiredPendingAttemptEnds(t *testing.T) {
	f := newFixture(t, Config{Platforms: []imgspecv1.Platform{arm64}, Grace: 300 * time.Millisecond})
	ctx := t.Context()
	req := testGraphFor(t, f, "unserved", arm64)
	id, err := f.c.Submit(ctx, api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return f.c.pool.liveAttempts() == 1 })
	// Nobody watches: a watch would hold the build. The result is polled instead.
	var outcome api.Outcome
	waitFor(t, func() bool {
		outcome, err = f.c.Result(ctx, id)
		return !errors.Is(err, api.ErrRunning)
	})
	if err != nil || outcome.Failure == nil || !strings.Contains(outcome.Failure.Message, "expired") {
		t.Fatalf("outcome = %+v, %v; want a failure saying the build expired", outcome, err)
	}
	if n := f.c.pool.liveAttempts(); n != 0 {
		t.Errorf("%d attempts are still live after the build expired, want none", n)
	}
}

// TestCancelledPushOutcome checks that a push cancelled while its worker runs it ends as cancelled, with an outcome
// saying the destination may or may not have been written.
func TestCancelledPushOutcome(t *testing.T) {
	f := newFixture(t, Config{})
	gate := make(chan struct{})
	pushing := make(chan struct{}, 1)
	startWorker(t, f, amd64, 1, func(a *remote.Assignment, killed func() bool) remote.AttemptReport {
		pushing <- struct{}{}
		<-gate
		return remote.AttemptReport{Pushed: &api.Pushed{Image: a.Push.Result.Image}}
	})
	ctx := t.Context()
	res := api.Result{Image: strings.Repeat("a", 64)}
	id, err := f.c.Submit(ctx, api.Request{Push: &api.PushRequest{Result: res, Destination: "example.com/app:v1"}})
	if err != nil {
		t.Fatal(err)
	}
	<-pushing
	if err := f.c.Cancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Watch(ctx, id, 0, func(api.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	close(gate)
	outcome, err := f.c.Result(ctx, id)
	if err != nil || !outcome.Cancelled || outcome.Failure == nil ||
		!strings.Contains(outcome.Failure.Message,
			"the destination example.com/app:v1 may or may not have been written") {
		t.Errorf("outcome = %+v, %v; want it cancelled, saying the destination may have been written", outcome, err)
	}
}

// TestPruneRefusedWhileHeld checks that a finished build held by its client, which watches it again through its
// delivery, keeps a prune out until it is released, and that an unwatched one lets a prune through once its grace
// period has passed.
func TestPruneRefusedWhileHeld(t *testing.T) {
	// Each sleep below is short against the grace period, so that a slow machine can't let the build lapse
	// meanwhile, and the two are longer together, so that the second refusal is the renewed hold's.
	f := newFixture(t, Config{Grace: 1500 * time.Millisecond})
	startWorker(t, f, amd64, 4, fakeRun(f, nil))
	ctx := t.Context()
	time.Sleep(f.c.cfg.Lease) // the hold after starting
	id, err := f.c.Submit(ctx, api.Request{Graph: ptr(testGraph(t, f, "held"))})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.c.Watch(ctx, id, 0, func(api.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.Prune(ctx); err == nil || !strings.Contains(err.Error(), "other jobs are running or held open") {
		t.Errorf("a prune while the build is held = %v, want it refused", err)
	}
	// Watching again renews the hold; released, the build keeps nothing out.
	time.Sleep(time.Second)
	if err := f.c.Watch(ctx, id, 1000, func(api.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if _, err := f.c.Prune(ctx); err == nil || !strings.Contains(err.Error(), "held open") {
		t.Errorf("a prune while the hold is renewed = %v, want it refused", err)
	}
	if err := f.c.Release(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, err := f.c.Prune(ctx)
		return err == nil
	})
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }

// orderedStores wraps the fixture's stores to record the order of a prune's clears.
type orderedStores struct {
	*cache.Memory
	mu    sync.Mutex
	order []string
}

func (o *orderedStores) ClearContent(ctx context.Context) (int, error) {
	o.mu.Lock()
	o.order = append(o.order, "content")
	o.mu.Unlock()
	return o.Memory.ClearContent(ctx)
}

func (o *orderedStores) ClearResults(ctx context.Context) (int, error) {
	o.mu.Lock()
	o.order = append(o.order, "results")
	o.mu.Unlock()
	return o.Memory.ClearResults(ctx)
}

// TestPruneOrder checks that a prune clears the content before the records, whose clearing advances the generation,
// so that a client that read the new generation uploads to a store that nothing clears any more.
func TestPruneOrder(t *testing.T) {
	stores := &orderedStores{Memory: cache.NewMemory()}
	c, err := New(stores, stores, Config{Lease: 50 * time.Millisecond, Heartbeat: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	ctx := t.Context()
	time.Sleep(c.cfg.Lease)
	id, err := c.Prune(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Watch(ctx, id, 0, func(api.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	stores.mu.Lock()
	defer stores.mu.Unlock()
	if want := []string{"content", "results"}; !slices.Equal(stores.order, want) {
		t.Errorf("the prune cleared %v, want %v", stores.order, want)
	}
}
