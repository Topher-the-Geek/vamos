package coord

import (
	"fmt"
	"slices"
	"strings"
	"sync"
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

// indexGraph returns, for an executor on amd64, the graph that the loader makes of an index for amd64 and arm64 of
// a layer that adds a task whose base is dep(..., platform = EXEC_PLATFORM), and which adds another such task
// without a platform: each task is configured for each of the index's platforms, so that both platforms' tasks are
// alike but for the tasks they add, and share their keys. If layerRuns, each layer runs a command as well.
// Nodes: 0 and 1 the pulls for amd64 and arm64; 2 and 5 the task codegen, 3 and 6 the task build, for amd64 and
// arm64; 4 and 7 the layers; 8 the index, the root.
func indexGraph(t *testing.T, f *fixture, nonce string, layerRuns bool) api.GraphRequest {
	t.Helper()
	sel := fileset.DefaultSelection()
	sel.Include = []string{"."}
	from := func(ref graph.Ref) *graph.Ref { return &ref }
	codegen := func(desc string) graph.Node {
		return graph.Node{Label: "codegen", Desc: desc, Task: &graph.Task{Base: 0, Steps: []graph.Step{
			{Op: graph.OpRun, Display: `run(["codegen"])`, Argv: []string{"codegen", nonce}},
		}, Output: &graph.Output{Source: "/out", Selection: sel}}}
	}
	build := func(desc string, codegen graph.Ref) graph.Node {
		return graph.Node{Label: "build", Desc: desc, Task: &graph.Task{Base: 0, Steps: []graph.Step{
			{Op: graph.OpAdd, Display: `add("codegen", "/w")`, From: from(codegen), Dest: "/w"},
			{Op: graph.OpRun, Display: `run(["build"])`, Argv: []string{"build", nonce}},
		}, Output: &graph.Output{Source: "/out", Selection: sel}}}
	}
	layer := func(desc string, base graph.Ref, platform string, build graph.Ref) graph.Node {
		steps := []graph.Step{{Op: graph.OpAdd, Display: `add(dep("build"), "/app")`, From: from(build), Dest: "/app"}}
		if layerRuns {
			steps = append(steps, graph.Step{Op: graph.OpRun, Display: `run(["apk", "upgrade"])`,
				Argv: []string{"apk", "upgrade", nonce}})
		}
		return graph.Node{Label: "layer", Desc: desc, Layer: &graph.Layer{Base: base, Platform: platform,
			Steps: steps}}
	}
	g := &graph.Graph{Version: graph.Version, ExecPlatform: "linux/amd64", Roots: []graph.Ref{8}, Nodes: []graph.Node{
		{Label: "base", Desc: `pull "base"`, Pull: &graph.Pull{Image: "example.com/base:v1", Platform: "linux/amd64"}},
		{Label: "base", Desc: `pull "base" (platform=linux/arm64)`,
			Pull: &graph.Pull{Image: "example.com/base:v1", Platform: "linux/arm64"}},
		codegen(`task "codegen"`),
		build(`task "build"`, 2),
		layer(`layer "layer"`, 0, "linux/amd64", 3),
		codegen(`task "codegen" (platform=linux/arm64)`),
		build(`task "build" (platform=linux/arm64)`, 5),
		layer(`layer "layer" (platform=linux/arm64)`, 1, "linux/arm64", 6),
		{Label: "server", Desc: `index "server"`, Index: &graph.Index{Manifests: []graph.Ref{4, 7}}},
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

// ranOn records which worker platform ran each layer or task attempt, by the key of its action, as fakeRun runs it.
type ranOn struct {
	mu   sync.Mutex
	runs map[string][]string // the platforms of the workers that ran each key's attempts, in order
	kind map[string]string   // each key's action's kind, and for a layer its platform
}

// handler returns fakeRun's handler for a worker of platform, recording what it runs.
func (r *ranOn) handler(f *fixture, platform imgspecv1.Platform) func(
	a *remote.Assignment, killed func() bool) remote.AttemptReport {
	run := fakeRun(f, nil)
	return func(a *remote.Assignment, killed func() bool) remote.AttemptReport {
		if a.Request != nil && a.Request.Action.Kind != sched.KindResolve {
			r.mu.Lock()
			key := a.Request.Key.String()
			r.runs[key] = append(r.runs[key], platforms.Format(platform))
			r.kind[key] = a.Request.Action.Kind + " " + a.Request.Action.Platform
			r.mu.Unlock()
		}
		return run(a, killed)
	}
}

// overlapping returns handle, whose tasks first wait, for a few seconds at most, until both of indexGraph's nodes of
// their key are attached to c's attempt, so that one shares the other's rather than finding its result cached.
func overlapping(c *Coordinator, handle func(a *remote.Assignment, killed func() bool) remote.AttemptReport) func(
	a *remote.Assignment, killed func() bool) remote.AttemptReport {
	return func(a *remote.Assignment, killed func() bool) remote.AttemptReport {
		if a.Request != nil && a.Request.Action.Kind == sched.KindTask {
			key := a.Request.Action.Key()
			for start := time.Now(); c.sharing.AttachedTo(key) < 2 && time.Since(start) < 5*time.Second; {
				time.Sleep(time.Millisecond)
			}
		}
		return handle(a, killed)
	}
}

// assertShared checks that, of each pair of nodes configured alike for amd64 and arm64 in statuses, one built and
// the other said it shared that one's attempt.
func assertShared(t *testing.T, statuses []string, pairs map[graph.Ref]graph.Ref, descs map[graph.Ref]string) {
	t.Helper()
	for a, b := range pairs {
		ok := func(first, second graph.Ref) bool {
			return slices.Contains(statuses, fmt.Sprintf("[%d]: building", first)) && slices.Contains(statuses,
				fmt.Sprintf("[%d]: building, shared with %s", second, descs[first]))
		}
		if !ok(a, b) && !ok(b, a) {
			t.Errorf("statuses = %q, want one of nodes %d and %d building and the other sharing it", statuses, a, b)
		}
	}
}

// indexPairs are indexGraph's nodes configured alike for both platforms, and descs their descriptions.
var (
	indexPairs = map[graph.Ref]graph.Ref{2: 5, 3: 6}
	indexDescs = map[graph.Ref]string{
		2: `task "codegen"`, 5: `task "codegen" (platform=linux/arm64)`,
		3: `task "build"`, 6: `task "build" (platform=linux/arm64)`,
	}
)

// TestSharedConfigurations checks that the tasks that an index's platforms configure alike, on a base for the exec
// platform, run once though the graph has a node for each platform: one attaches to the other's attempt, and says
// so, rather than seeming to build again.
func TestSharedConfigurations(t *testing.T) {
	f := newFixture(t, Config{})
	r := &ranOn{runs: map[string][]string{}, kind: map[string]string{}}
	startWorker(t, f, amd64, 4, overlapping(f.c, r.handler(f, amd64)))
	outcome, statuses := runJob(t, f.c, indexGraph(t, f, "shared", false))
	if outcome.Failure != nil {
		t.Fatalf("outcome = %+v", outcome.Failure)
	}
	if len(r.runs) != 4 {
		t.Errorf("attempts by key = %v, want the two tasks and the two layers", r.runs)
	}
	for key, runs := range r.runs {
		if len(runs) != 1 {
			t.Errorf("the %s of key %s ran %d times, want once", r.kind[key], key, len(runs))
		}
	}
	assertShared(t, statuses, indexPairs, indexDescs)
}

// TestPerPlatformExecution checks that a coordinator with amd64 and arm64 workers runs each platform's layer, whose
// commands are for its platform, on a worker of that platform, natively, and the tasks on the exec platform's base
// once, on the exec platform; that a platform the deployment offers with no worker registered waits for one, saying
// so, rather than emulating; and that a platform no worker runs natively is refused, naming the way out.
func TestPerPlatformExecution(t *testing.T) {
	t.Run("mixed pool", func(t *testing.T) {
		f := newFixture(t, Config{})
		r := &ranOn{runs: map[string][]string{}, kind: map[string]string{}}
		startWorker(t, f, amd64, 2, overlapping(f.c, r.handler(f, amd64)))
		startWorker(t, f, arm64, 2, overlapping(f.c, r.handler(f, arm64)))
		outcome, statuses := runJob(t, f.c, indexGraph(t, f, "mixed", true))
		if outcome.Failure != nil {
			t.Fatalf("outcome = %+v", outcome.Failure)
		}
		want := map[string][]string{
			"task ":             {"linux/amd64"},
			"layer linux/amd64": {"linux/amd64"},
			"layer linux/arm64": {"linux/arm64"},
		}
		if len(r.runs) != 4 {
			t.Errorf("attempts by key = %v, want the two tasks and the two layers", r.runs)
		}
		for key, runs := range r.runs {
			if !slices.Equal(runs, want[r.kind[key]]) {
				t.Errorf("the %s of key %s ran on %q, want %q", r.kind[key], key, runs, want[r.kind[key]])
			}
		}
		assertShared(t, statuses, indexPairs, indexDescs)
		if len(outcome.Results) != 1 || len(outcome.Results[0].Index) != 2 {
			t.Errorf("results = %+v, want the index of both platforms' images", outcome.Results)
		}
	})
	t.Run("offered without a worker", func(t *testing.T) {
		f := newFixture(t, Config{Platforms: []imgspecv1.Platform{amd64, arm64}})
		r := &ranOn{runs: map[string][]string{}, kind: map[string]string{}}
		startWorker(t, f, amd64, 2, r.handler(f, amd64), arm64) // it could emulate, but isn't asked to
		ctx := t.Context()
		// The tag is recorded for both platforms, so that only the arm64 layer, not its pull, waits for a worker.
		index := digest.FromString("index of example.com/base:v1")
		record := &cache.TagRecord{Digest: index, Instances: map[string]cache.TagInstance{}}
		for _, p := range []string{"linux/amd64", "linux/arm64"} {
			id := digest.FromString(index.String() + " " + p)
			location := cache.Location{Kind: cache.LocationRegistry, Reference: "hub-proxy:5000/base@" + id.String()}
			record.Instances[p] = cache.TagInstance{Digest: id, Image: id, Platform: p, Epoch: 1,
				Locations: []cache.Location{location}}
		}
		if err := f.m.SetTag(ctx, "example.com/base:v1", record); err != nil {
			t.Fatal(err)
		}
		req := indexGraph(t, f, "waits", true)
		req.Emulate = true
		id, err := f.c.Submit(ctx, api.Request{Graph: &req})
		if err != nil {
			t.Fatal(err)
		}
		waiting := make(chan struct{})
		var once sync.Once
		watched := make(chan error, 1)
		go func() {
			watched <- f.c.Watch(ctx, id, 0, func(e api.Event) error {
				if e.Kind == api.EventOutput && slices.Equal(e.Nodes, []graph.Ref{7}) &&
					strings.Contains(string(e.Data), "waiting for a worker that runs linux/arm64; none is registered") {
					once.Do(func() { close(waiting) })
				}
				return nil
			})
		}()
		select {
		case <-waiting:
		case err := <-watched:
			t.Fatalf("the build ended without waiting for an arm64 worker: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("the build never said it waits for an arm64 worker")
		}
		startWorker(t, f, arm64, 2, r.handler(f, arm64))
		if err := <-watched; err != nil {
			t.Fatal(err)
		}
		outcome, err := f.c.Result(ctx, id)
		if err != nil || outcome.Failure != nil {
			t.Fatalf("outcome = %+v, %v", outcome.Failure, err)
		}
		for key, runs := range r.runs {
			if r.kind[key] == "layer linux/arm64" && !slices.Equal(runs, []string{"linux/arm64"}) {
				t.Errorf("the arm64 layer ran on %q, want the arm64 worker alone, natively", runs)
			}
		}
	})
	t.Run("adding files without a worker", func(t *testing.T) {
		// Layers that run no command run on any worker while none of their platform is registered.
		f := newFixture(t, Config{Platforms: []imgspecv1.Platform{amd64, arm64}})
		r := &ranOn{runs: map[string][]string{}, kind: map[string]string{}}
		startWorker(t, f, amd64, 2, r.handler(f, amd64))
		outcome, _ := runJob(t, f.c, indexGraph(t, f, "adds", false))
		if outcome.Failure != nil {
			t.Fatalf("outcome = %+v", outcome.Failure)
		}
		for key, runs := range r.runs {
			if r.kind[key] == "layer linux/arm64" && !slices.Equal(runs, []string{"linux/amd64"}) {
				t.Errorf("the arm64 layer ran on %q, want the amd64 worker, the only one", runs)
			}
		}
	})
	t.Run("no native worker", func(t *testing.T) {
		f := newFixture(t, Config{Platforms: []imgspecv1.Platform{amd64}})
		r := &ranOn{runs: map[string][]string{}, kind: map[string]string{}}
		startWorker(t, f, amd64, 2, r.handler(f, amd64))
		outcome, _ := runJob(t, f.c, indexGraph(t, f, "refused", true))
		if outcome.Failure == nil || !strings.Contains(outcome.Failure.Message, "its commands would run under "+
			"emulation: its image is for linux/arm64, and this build runs commands only on linux/amd64") ||
			!strings.Contains(outcome.Failure.Hint, "add a worker for linux/arm64") {
			t.Fatalf("outcome = %+v, want the arm64 layer refused, naming a worker for its platform", outcome.Failure)
		}
		for key := range r.runs {
			if r.kind[key] == "layer linux/arm64" {
				t.Error("the arm64 layer ran though no worker runs its platform natively")
			}
		}
	})
}

// TestExecPlatformForClients checks that a coordinator configured with an execution platform for clients reports it,
// normalized, with its capabilities, that one without reports none, as an older coordinator, and that one not among
// the platforms configured is refused, since it might not be offered.
func TestExecPlatformForClients(t *testing.T) {
	m := cache.NewMemory()
	configured := imgspecv1.Platform{OS: "linux", Architecture: "aarch64"}
	c, err := New(m, m, Config{Platforms: []imgspecv1.Platform{amd64, arm64}, ExecPlatform: &configured})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	caps, err := c.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if caps.ExecPlatform == nil || platforms.Format(*caps.ExecPlatform) != "linux/arm64" {
		t.Errorf("capabilities = %+v, want the execution platform linux/arm64 for clients", caps.ExecPlatform)
	}

	c, err = New(m, m, Config{Platforms: []imgspecv1.Platform{amd64}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if caps, err := c.Capabilities(t.Context()); err != nil || caps.ExecPlatform != nil {
		t.Errorf("capabilities = %+v, %v; want no execution platform for clients", caps.ExecPlatform, err)
	}

	_, err = New(m, m, Config{Platforms: []imgspecv1.Platform{amd64}, ExecPlatform: &arm64})
	if want := "the execution platform linux/arm64 isn't one of the platforms offered, [linux/amd64]"; err == nil ||
		err.Error() != want {
		t.Errorf("New = %v, want %q", err, want)
	}
}

// TestExpiredWorkerWakesPollers checks that once the last worker of a platform is forgotten, a worker of another
// platform that waits for an assignment is woken and takes that platform's work that runs no command, rather than
// waiting for some other change.
func TestExpiredWorkerWakesPollers(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := newPool(Config{}.withDefaults(), "i", func() (uint64, error) { return 0, nil })
	var mu sync.Mutex
	p.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	register := func(platform imgspecv1.Platform) string {
		reg, err := p.Register(t.Context(), remote.WorkerInfo{Platform: platform, ActionVersion: sched.ActionVersion,
			Capacity: 1})
		if err != nil {
			t.Fatal(err)
		}
		return reg.ID
	}
	register(arm64)
	polling := register(amd64)
	adds := &sched.Request{RunPlatform: "linux/arm64", Action: &sched.Action{Kind: sched.KindLayer,
		Steps: []sched.Step{{Op: graph.OpAdd}}}}
	at := &attempt{id: "a", req: adds, trace: t.Context(), done: make(chan struct{})}
	p.mu.Lock()
	p.attempts[at.id] = at
	p.pending = append(p.pending, at)
	p.mu.Unlock()

	assigned := make(chan *remote.Assignment, 1)
	go func() {
		a, _ := p.NextAssignment(t.Context(), polling) // nil once the test ends
		assigned <- a
	}()
	waitFor(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.workers[polling].polling == 1
	})
	select {
	case a := <-assigned:
		t.Fatalf("the amd64 worker took %+v while an arm64 worker was registered", a)
	default:
	}
	mu.Lock()
	now = now.Add(2 * p.cfg.Lease)
	mu.Unlock()
	p.expire()
	select {
	case a := <-assigned:
		if a == nil || a.Attempt != at.id {
			t.Errorf("the amd64 worker was assigned %+v, want the arm64 work that adds files", a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the amd64 worker wasn't woken once the arm64 worker was forgotten")
	}
}
