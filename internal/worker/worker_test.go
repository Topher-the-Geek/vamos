package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/api/apitest"
	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/coord"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/remote"
	"github.com/Topher-the-Geek/vamos/internal/run"
	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/Topher-the-Geek/vamos/internal/transfer"

	"github.com/containerd/platforms"
	"github.com/google/go-containerregistry/pkg/registry"
	"go.podman.io/buildah"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

func TestMain(m *testing.M) {
	// buildah re-executes the test binary to run build steps, and a worker runs it as an attempt's subprocess.
	if buildah.InitReexec() || ChildFromEnv() {
		return
	}
	if os.Getenv("BUILDAH_ISOLATION") == "" {
		os.Setenv("BUILDAH_ISOLATION", "chroot")
	}
	code := m.Run()
	if err := testenv.RemoveSuiteStores(); err != nil {
		fmt.Fprintf(os.Stderr, "removing the suite's stores: %v\n", err)
		code = 1
	}
	os.Exit(code)
}

// fixture is a coordinator with stores in memory, served over HTTP, with a cache registry of its own.
type fixture struct {
	c        *coord.Coordinator
	m        *cache.Memory
	server   *httptest.Server
	registry string // the cache repository
}

// testLease is the lease of the fixture's coordinator: short, so that tests of its expiry are quick.
const testLease = 2 * time.Second

// newFixture returns a coordinator with short leases, whose workers push layers to an in-process registry.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWithLease(t, testLease)
}

// newFixtureWithLease returns a coordinator whose attempts' leases last lease, with heartbeats at a sixth of it.
func newFixtureWithLease(t *testing.T, lease time.Duration) *fixture {
	t.Helper()
	testenv.RequireBuildah(t)
	reg := httptest.NewServer(untagging(registry.New(registry.Logger(log.New(io.Discard, "", 0)))))
	t.Cleanup(reg.Close)
	host := strings.TrimPrefix(reg.URL, "http://")
	testenv.InsecureRegistry(t, host)
	sys, err := transfer.SystemContext()
	testenv.Must(t, err)
	m := cache.NewMemory()
	c, err := coord.New(m, m, coord.Config{
		Lease:           lease,
		Heartbeat:       lease / 6,
		CacheRepository: host + "/vamos-cache",
		Images:          &coord.Registry{Sys: sys},
	})
	testenv.Must(t, err)
	t.Cleanup(func() { c.Close() })
	server := httptest.NewServer((&remote.Server{Executor: c, Content: m, Workers: c.Workers()}).Handler())
	t.Cleanup(server.Close)
	return &fixture{c: c, m: m, server: server, registry: host + "/vamos-cache"}
}

// startWorker runs a worker of the fixture's coordinator, in a store of its own, until the test ends;
// each of configure changes its configuration first.
func startWorker(t *testing.T, f *fixture, configure ...func(*Config)) (*Worker, *testenv.Stores) {
	t.Helper()
	stores := testenv.PrivateStores(t)
	cfg := Config{
		Coordinator: f.server.URL,
		Store:       stores.Options,
		Capacity:    2,
		Child:       TestChild(stores.Options),
		CgroupRoot:  testCgroupRoot(t),
		Log:         testWriter{t},
	}
	for _, c := range configure {
		c(&cfg)
	}
	w, err := New(cfg)
	testenv.Must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	// The worker registers in its own time; the coordinator offers its platform once it has.
	for start := time.Now(); ; {
		caps, err := f.c.Capabilities(context.Background())
		testenv.Must(t, err)
		if caps.Offers(run.HostPlatform) {
			break
		}
		if time.Since(start) > 10*time.Second {
			t.Fatal("the worker never registered")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return w, stores
}

// untagging makes the in-process registry h delete a manifest's tags along with the manifest when it is deleted by
// digest, as Distribution does and as the coordinator's prune relies on; the in-process registry alone keeps the tag.
func untagging(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo, ref, isManifest := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v2/"), "/manifests/")
		if r.Method == http.MethodDelete && isManifest && strings.HasPrefix(ref, "sha256:") {
			for _, tag := range tagsOf(h, repo) {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/v2/"+repo+"/manifests/"+tag, nil))
				if rec.Header().Get("Docker-Content-Digest") == ref {
					h.ServeHTTP(httptest.NewRecorder(),
						httptest.NewRequest(http.MethodDelete, "/v2/"+repo+"/manifests/"+tag, nil))
				}
			}
		}
		h.ServeHTTP(w, r)
	})
}

// tagsOf lists the tags of repo at the registry h.
func tagsOf(h http.Handler, repo string) []string {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/"+repo+"/tags/list", nil))
	var list struct {
		Tags []string `json:"tags"`
	}
	json.Unmarshal(rec.Body.Bytes(), &list)
	return list.Tags
}

// testCgroupRoot returns a cgroup root for this test binary's workers, removed when the test ends.
func testCgroupRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(filepath.Dir(cgroupRoot), fmt.Sprintf("vamos-test-%d", os.Getpid()))
	t.Cleanup(func() {
		if err := os.Remove(root); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Logf("removing the test's cgroup root: %v", err)
		}
	})
	return root
}

// testWriter logs to the test.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// layerGraph stores a graph whose root is a layer on scratch adding a file with content, and returns its request.
func layerGraph(t *testing.T, f *fixture, content string, steps ...graph.Step) api.GraphRequest {
	t.Helper()
	ctx := t.Context()
	file, err := f.m.PutBytes(ctx, []byte(content))
	testenv.Must(t, err)
	tree, err := f.m.PutBytes(ctx, (&cache.Tree{Entries: []cache.TreeEntry{
		{Path: "a.txt", Type: cache.EntryFile, Mode: 0o644, Blob: &file},
	}}).Encode())
	testenv.Must(t, err)
	from := graph.Ref(0)
	platform := platforms.Format(run.HostPlatform)
	g := &graph.Graph{Version: graph.Version, ExecPlatform: platform, Roots: []graph.Ref{1}, Nodes: []graph.Node{
		{Label: "src", Desc: `glob "src"`, Source: &graph.Source{Tree: tree}},
		{Label: "app", Desc: `layer "app"`, Layer: &graph.Layer{Base: graph.Scratch, Platform: platform,
			Steps: append([]graph.Step{{Op: graph.OpAdd, Display: `add("src", "/data")`, From: &from, Dest: "/data"}},
				steps...)}},
	}}
	testenv.Must(t, g.Validate())
	stored, err := f.m.PutBytes(ctx, graph.Encode(g))
	testenv.Must(t, err)
	return api.GraphRequest{Graph: stored, Version: graph.Version, Platform: run.HostPlatform}
}

// runJob submits req and watches it to its end, returning its outcome and the statuses reported.
func runJob(t *testing.T, c *coord.Coordinator, req api.GraphRequest) (api.Outcome, []string) {
	t.Helper()
	ctx := t.Context()
	id, err := c.Submit(ctx, api.Request{Graph: &req})
	testenv.Must(t, err)
	var statuses []string
	testenv.Must(t, c.Watch(ctx, id, 0, func(e api.Event) error {
		if e.Kind == api.EventStatus {
			statuses = append(statuses, e.Status)
		}
		return nil
	}))
	outcome, err := c.Result(ctx, id)
	testenv.Must(t, err)
	testenv.Must(t, c.Release(ctx, id)) // as a client does once it has delivered: a held build keeps a prune out
	return outcome, statuses
}

// TestWorkerBuilds checks that a worker builds a layer the coordinator assigns, in a subprocess, pushes it to the
// cache repository before reporting, so that the result's location is there, and that the next build is a hit that
// the coordinator checks against the registry without asking the worker.
func TestWorkerBuilds(t *testing.T) {
	f := newFixture(t)
	startWorker(t, f)
	req := layerGraph(t, f, "built on a worker")
	outcome, statuses := runJob(t, f.c, req)
	if outcome.Failure != nil {
		t.Fatalf("outcome = %+v", outcome.Failure)
	}
	res := outcome.Results[0]
	var registry []cache.Location
	for _, l := range res.Locations {
		if l.Kind == cache.LocationRegistry {
			registry = append(registry, l)
		}
	}
	if len(registry) != 1 || registry[0].Reference != f.registry+":"+res.Image {
		t.Errorf("locations = %+v, want the image at %s:%s", res.Locations, f.registry, res.Image)
	}
	if !strings.Contains(strings.Join(statuses, ","), "building") {
		t.Errorf("statuses = %q, want the layer building", statuses)
	}
	checker := &coord.Registry{Sys: mustSys(t)}
	if problem, err := checker.CheckImage(t.Context(), res.Image, registry[0]); problem != nil || err != nil {
		t.Errorf("the image at its location = %v, %v; want it there", problem, err)
	}

	outcome, statuses = runJob(t, f.c, req)
	if outcome.Failure != nil || !strings.Contains(strings.Join(statuses, ","), "cached") {
		t.Errorf("the second build = %+v with statuses %q; want the layer cached", outcome.Failure, statuses)
	}
}

// mustSys returns the registry settings of this process's environment.
func mustSys(t *testing.T) *types.SystemContext {
	t.Helper()
	sys, err := transfer.SystemContext()
	testenv.Must(t, err)
	return sys
}

// TestWorkerFailureCrosses checks that a step that fails on the worker reaches the client as the same failure a
// local build reports: the step, its message and its log.
func TestWorkerFailureCrosses(t *testing.T) {
	f := newFixture(t)
	startWorker(t, f)
	req := layerGraph(t, f, "failing", graph.Step{Op: graph.OpRun, Display: `run(["/nope"])`, Argv: []string{"/nope"}})
	outcome, _ := runJob(t, f.c, req)
	if outcome.Failure == nil || outcome.Failure.Step == nil || *outcome.Failure.Step != 1 ||
		len(outcome.Failure.Nodes) != 1 || outcome.Failure.Nodes[0] != 1 {
		t.Fatalf("outcome = %+v, want the layer's second step failed", outcome.Failure)
	}
	if outcome.Failure.Log == nil {
		t.Error("the failure carries no log")
	}
}

// TestWorkerKillsOnCancel checks that cancelling a build kills the attempt's subprocess and everything it started,
// within seconds, and removes the attempt's container from the worker's store.
func TestWorkerKillsOnCancel(t *testing.T) {
	f := newFixture(t)
	w, stores := startWorker(t, f)
	ctx := t.Context()
	// The step needs a shell to sleep in, so the base is busybox from Docker Hub, which the cli suite pulls too;
	// one sleep leaves the process group with setsid, as a daemon would, and must die all the same.
	platform := platforms.Format(run.HostPlatform)
	g := &graph.Graph{Version: graph.Version, ExecPlatform: platform, Roots: []graph.Ref{1}, Nodes: []graph.Node{
		{Label: "busybox", Desc: `pull "busybox"`, Pull: &graph.Pull{Image: "docker.io/library/busybox:1.36",
			Platform: platform}},
		{Label: "app", Desc: `layer "app"`, Layer: &graph.Layer{Base: 0, Platform: platform, Steps: []graph.Step{
			{Op: graph.OpRun, Display: `run(["sh", "-c", "..."])`, Argv: []string{"sh", "-c",
				"setsid sleep 300 & sleep 300"}},
		}}},
	}}
	testenv.Must(t, g.Validate())
	stored, err := f.m.PutBytes(ctx, graph.Encode(g))
	testenv.Must(t, err)
	id, err := f.c.Submit(ctx, api.Request{Graph: &api.GraphRequest{Graph: stored, Version: graph.Version,
		Platform: run.HostPlatform}})
	testenv.Must(t, err)
	// Wait for the layer to run: its attempt's cgroup holds processes.
	var attempt string
	deadline := time.Now().Add(3 * time.Minute)
	for attempt == "" {
		if time.Now().After(deadline) {
			t.Fatal("the layer never started running")
		}
		time.Sleep(100 * time.Millisecond)
		ids, _ := w.contain.leftovers()
		for _, id := range ids {
			procs, _ := os.ReadFile(filepath.Join(w.contain.path(id), "cgroup.procs"))
			if strings.Count(string(procs), "\n") >= 3 { // the attempt, sh and the sleeps
				attempt = id
			}
		}
	}
	start := time.Now()
	testenv.Must(t, f.c.Cancel(ctx, id))
	testenv.Must(t, f.c.Watch(ctx, id, 0, func(api.Event) error { return nil }))
	if outcome, err := f.c.Result(ctx, id); err != nil || !outcome.Cancelled {
		t.Errorf("outcome = %+v, %v; want it cancelled", outcome, err)
	}
	for {
		if _, err := os.Stat(w.contain.path(attempt)); errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Since(start) > 30*time.Second {
			procs, _ := os.ReadFile(filepath.Join(w.contain.path(attempt), "cgroup.procs"))
			t.Fatalf("the attempt's cgroup still holds processes %s after %s", procs, time.Since(start))
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("the attempt's processes were gone %s after the cancel", time.Since(start))
	store, err := storage.GetStore(stores.Options)
	testenv.Must(t, err)
	defer store.Shutdown(false)
	containers, err := store.Containers()
	testenv.Must(t, err)
	for _, c := range containers {
		for _, name := range c.Names {
			if strings.HasPrefix(name, attemptPrefix) {
				t.Errorf("container %s remains after the kill", name)
			}
		}
	}
}

// attemptPrefix is how the run package names attempts' containers.
const attemptPrefix = "vamos-attempt-"

// TestContract runs the executor contract on a coordinator with one worker, through remote.Client, as a client
// sees it: the coordinator exports nothing, so the contract checks locations and the refusal instead.
func TestContract(t *testing.T) {
	testenv.RequireBuildah(t)
	contents := map[api.Executor]*cache.Memory{}
	var mu sync.Mutex
	apitest.Contract(t, apitest.Fixture{
		New: func(t *testing.T) api.Executor {
			t.Helper()
			// A coordinator refuses a prune for a lease after it starts, which the contract's prune doesn't expect,
			// so the lease is short and waited out.
			const lease = 600 * time.Millisecond
			f := newFixtureWithLease(t, lease)
			f.c.RetainEvents(8)
			startWorker(t, f)
			time.Sleep(lease)
			client, err := remote.NewClient(f.server.URL)
			testenv.Must(t, err)
			t.Cleanup(func() { client.Close() })
			mu.Lock()
			contents[client] = f.m
			mu.Unlock()
			return client
		},
		Put: func(t *testing.T, ex api.Executor, data []byte) cache.Blob {
			t.Helper()
			mu.Lock()
			m := contents[ex]
			mu.Unlock()
			b, err := m.PutBytes(t.Context(), data)
			testenv.Must(t, err)
			return b
		},
		Missing: func(t *testing.T, ex api.Executor, blobs []cache.Blob) []cache.Blob {
			t.Helper()
			mu.Lock()
			m := contents[ex]
			mu.Unlock()
			missing, err := m.Missing(t.Context(), blobs)
			testenv.Must(t, err)
			return missing
		},
		Platform:      run.HostPlatform,
		Registry:      registry.New(registry.Logger(log.New(io.Discard, "", 0))),
		Insecure:      testenv.InsecureRegistry,
		Retention:     8,
		Buffered:      true,
		RequireBuilds: testenv.RequireBuildah,
		NoExport:      true,
	})
}

// TestWorkerFencesItself checks that an attempt whose heartbeats get no answer is killed before its lease would
// expire at the coordinator, on the worker's own clock, so that the coordinator may retry its work without the two
// running at once: a proxy between the worker and the coordinator hangs every heartbeat once the attempt runs.
func TestWorkerFencesItself(t *testing.T) {
	f := newFixture(t)
	var hanging atomic.Bool
	upstream := httputil.NewSingleHostReverseProxy(mustURL(t, f.server.URL))
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hanging.Load() && strings.HasSuffix(r.URL.Path, "/heartbeat") {
			// The body is read first: the server notices the client leaving only once it has read the request.
			io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			return
		}
		upstream.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	f.server = proxy
	w, stores := startWorker(t, f)
	ctx := t.Context()
	platform := platforms.Format(run.HostPlatform)
	g := &graph.Graph{Version: graph.Version, ExecPlatform: platform, Roots: []graph.Ref{1}, Nodes: []graph.Node{
		{Label: "busybox", Desc: `pull "busybox"`, Pull: &graph.Pull{Image: "docker.io/library/busybox:1.36",
			Platform: platform}},
		{Label: "app", Desc: `layer "app"`, Layer: &graph.Layer{Base: 0, Platform: platform, Steps: []graph.Step{
			{Op: graph.OpRun, Display: `run(["sleep", "300"])`, Argv: []string{"sleep", "300"}},
		}}},
	}}
	testenv.Must(t, g.Validate())
	stored, err := f.m.PutBytes(ctx, graph.Encode(g))
	testenv.Must(t, err)
	id, err := f.c.Submit(ctx, api.Request{Graph: &api.GraphRequest{Graph: stored, Version: graph.Version,
		Platform: run.HostPlatform}})
	testenv.Must(t, err)
	// The layer's attempt is the one to fence: the pull's has ended once the layer is building.
	wctx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	building := make(chan struct{})
	go f.c.Watch(wctx, id, 0, func(e api.Event) error {
		if e.Kind == api.EventStatus && e.Status == "building" && len(e.Nodes) == 1 && e.Nodes[0] == 1 {
			close(building)
			return context.Canceled
		}
		return nil
	})
	select {
	case <-building:
	case <-time.After(2 * time.Minute):
		t.Fatal("the layer never started building")
	}
	attempt := waitForAttempt(t, w)
	hanging.Store(true)
	fenced := time.Now()
	// The fence is at 80% of the lease; the kill and cleanup follow. The coordinator's view of the lease,
	// a lease from its last renewal, is what the attempt must not outlive.
	for time.Since(fenced) < testLease {
		if _, err := os.Stat(w.contain.path(attempt)); errors.Is(err, os.ErrNotExist) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(w.contain.path(attempt)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("attempt %s's cgroup still exists %s after its heartbeats stopped answering; want it killed within "+
			"the lease", attempt, time.Since(fenced).Round(time.Millisecond))
	}
	if err := run.RemoveAttempt(ctx, stores.Options, attempt); err != nil {
		t.Errorf("after the kill, the attempt's container: %v", err)
	}
	testenv.Must(t, f.c.Cancel(ctx, id))
}

// TestWorkerRenewsBeforeStarting checks that an assignment delivered after its lease has passed at the coordinator,
// which started the lease before sending it, never starts its process: the worker renews before starting it, and the
// coordinator, which has tried the work again, refuses the renewal.
func TestWorkerRenewsBeforeStarting(t *testing.T) {
	f := newFixture(t)
	upstream := httputil.NewSingleHostReverseProxy(mustURL(t, f.server.URL))
	var delayed atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/assignments") {
			upstream.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		upstream.ServeHTTP(rec, r)
		if rec.Code == http.StatusOK && delayed.CompareAndSwap(false, true) {
			time.Sleep(testLease + testLease/4) // the first assignment arrives once its lease has passed
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		w.Write(rec.Body.Bytes())
	}))
	t.Cleanup(proxy.Close)
	f.server = proxy
	var started atomic.Int32
	startWorker(t, f, func(cfg *Config) {
		child := cfg.Child
		cfg.Child = func(ctx context.Context) *exec.Cmd {
			started.Add(1)
			return child(ctx)
		}
	})
	outcome, _ := runJob(t, f.c, layerGraph(t, f, "delivered late"))
	if outcome.Failure != nil {
		t.Fatalf("outcome = %+v, want the retry to build it", outcome.Failure)
	}
	if !delayed.Load() {
		t.Fatal("no assignment was delayed")
	}
	if got := started.Load(); got != 1 {
		t.Errorf("the worker started %d attempts' processes, want only the retry's", got)
	}
}

// waitForAttempt returns the ID of the attempt w runs, once it runs exactly one.
func waitForAttempt(t *testing.T, w *Worker) string {
	t.Helper()
	for start := time.Now(); ; {
		w.mu.Lock()
		if len(w.attempts) == 1 {
			for id := range w.attempts {
				w.mu.Unlock()
				return id
			}
		}
		w.mu.Unlock()
		if time.Since(start) > 30*time.Second {
			t.Fatal("the worker never ran an attempt")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// mustURL parses raw.
func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	testenv.Must(t, err)
	return u
}

// TestPruneClearsRepository checks that a coordinator's prune, once a lease has passed since it started, removes the
// cache repository's tags along with the records and blobs, so that the image a result located there is gone.
func TestPruneClearsRepository(t *testing.T) {
	f := newFixture(t)
	startWorker(t, f)
	ctx := t.Context()
	outcome, _ := runJob(t, f.c, layerGraph(t, f, "pruned away"))
	if outcome.Failure != nil {
		t.Fatalf("outcome = %+v", outcome.Failure)
	}
	res := outcome.Results[0]
	var location cache.Location
	for _, l := range res.Locations {
		if l.Kind == cache.LocationRegistry {
			location = l
		}
	}
	checker := &coord.Registry{Sys: mustSys(t)}
	if problem, err := checker.CheckImage(ctx, res.Image, location); problem != nil || err != nil {
		t.Fatalf("the image at its location before the prune = %v, %v; want it there", problem, err)
	}
	time.Sleep(testLease) // a prune is refused for a lease after the coordinator started
	id, err := f.c.Prune(ctx)
	testenv.Must(t, err)
	testenv.Must(t, f.c.Watch(ctx, id, 0, func(api.Event) error { return nil }))
	pruned, err := f.c.Result(ctx, id)
	testenv.Must(t, err)
	if pruned.Failure != nil || pruned.Pruned == nil || pruned.Pruned.Tags != 1 || pruned.Pruned.Results == 0 ||
		pruned.Pruned.Blobs == 0 {
		t.Fatalf("the prune's outcome = %+v, failure %+v; want 1 tag and the results removed", pruned, pruned.Failure)
	}
	if problem, err := checker.CheckImage(ctx, res.Image, location); problem == nil || err != nil {
		t.Errorf("the image at its location after the prune = %v, %v; want it gone", problem, err)
	}
	// A fresh store's generation is 1, so one prune makes it 2.
	if caps, err := f.c.Capabilities(ctx); err != nil || caps.Generation != 2 {
		t.Errorf("capabilities after the prune = %+v, %v; want generation 2", caps, err)
	}
	// The next build is cold: the layer is built again, and pushed again.
	outcome, statuses := runJob(t, f.c, layerGraph(t, f, "pruned away"))
	if outcome.Failure != nil || !strings.Contains(strings.Join(statuses, ","), "building") {
		t.Errorf("the build after the prune = %+v with statuses %q; want the layer built again", outcome.Failure,
			statuses)
	}
	if problem, err := checker.CheckImage(ctx, res.Image, location); problem != nil || err != nil {
		t.Errorf("the image after the rebuild = %v, %v; want it back at its location", problem, err)
	}
}
