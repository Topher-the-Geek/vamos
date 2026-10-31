package features

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cli"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/remote"
	"github.com/Topher-the-Geek/vamos/internal/run"
	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/Topher-the-Geek/vamos/internal/worker"
	"github.com/google/go-containerregistry/pkg/name"
	gcrremote "github.com/google/go-containerregistry/pkg/v1/remote"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/afero"
	"go.podman.io/storage"
)

// Steps about building on a coordinator, which --coordinator names: one in this process, at <coordinator>.

// GivenACoordinator starts a coordinator, at <coordinator>, that builds with a local executor on stores of its
// own, as `vamos coordinator --local` does, until the scenario ends: so that what a command sends, it reads from
// the coordinator.
func (w *world) GivenACoordinator() {
	w.t.Helper()
	local, err := run.NewLocal(testenv.PrivateStores(w.t).Options)
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { local.Close() })
	content, err := local.ContentStore()
	if err != nil {
		w.t.Fatal(err)
	}
	w.startCoordinator((&remote.Server{Executor: local, Content: content}).Handler())
}

// GivenACoordinatorThatRunsOnlyGraphVersions starts a coordinator, at <coordinator>, that says it runs only graphs
// of versions, and answers nothing else, not finding it.
func (w *world) GivenACoordinatorThatRunsOnlyGraphVersions(versions ...int) {
	w.t.Helper()
	w.startCoordinatorThatSays(map[string]any{"graph_versions": versions})
}

// startCoordinatorThatSays starts a coordinator, at <coordinator>, whose capabilities are capabilities, as JSON
// encodes them, and that answers nothing else, not finding it.
func (w *world) startCoordinatorThatSays(capabilities any) {
	w.t.Helper()
	answer, err := json.Marshal(capabilities)
	if err != nil {
		w.t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/capabilities", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		if _, err := rw.Write(answer); err != nil {
			w.t.Errorf("the coordinator answering its capabilities: %v", err)
		}
	})
	w.startCoordinator(mux)
}

// GivenACoordinatorThatOffers starts a coordinator, at <coordinator>, that says it runs commands on offered, and
// that clients run them on preferred, unless it is "", and answers nothing else, not finding it.
func (w *world) GivenACoordinatorThatOffers(offered []string, preferred string) {
	w.t.Helper()
	caps := api.Capabilities{GraphVersions: []int{graph.Version}}
	for _, p := range offered {
		caps.Platforms = append(caps.Platforms, w.platform(p))
	}
	if preferred != "" {
		p := w.platform(preferred)
		caps.ExecPlatform = &p
	}
	w.startCoordinatorThatSays(caps)
}

// platform returns the platform that text names, stopping the scenario if it names none.
func (w *world) platform(text string) imgspecv1.Platform {
	w.t.Helper()
	p, err := graph.ParsePlatform(text)
	if err != nil {
		w.t.Fatal(err)
	}
	return p
}

// ThenTheGraphIsConfiguredFor checks that the graph that WhenIShowTheGraph showed is configured for run() steps to
// run on platform.
func (w *world) ThenTheGraphIsConfiguredFor(platform string) {
	w.t.Helper()
	if w.graph == nil {
		w.t.Fatal(w.noGraph())
	}
	if w.graph.ExecPlatform != platform {
		w.t.Errorf("the graph is configured for %s, want %s", w.graph.ExecPlatform, platform)
	}
}

// startCoordinator serves handler, at <coordinator>, until the scenario ends, counting the requests it is sent and
// the jobs it is given.
func (w *world) startCoordinator(handler http.Handler) {
	w.t.Helper()
	w.hasNoCoordinator()
	w.coordinatorServer = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.served.Add(1)
		if r.Method == http.MethodPost && r.URL.Path == "/v1/jobs" { // remote's pathJobs
			w.jobs.Add(1)
		}
		handler.ServeHTTP(rw, r)
	}))
	w.t.Cleanup(w.coordinatorServer.Close)
	w.coordinator = w.coordinatorServer.URL
}

// ThenTheCoordinatorWasGivenTheBuild checks that the last command gave the scenario's coordinator a job: that it
// built there, and not in this process.
func (w *world) ThenTheCoordinatorWasGivenTheBuild() {
	w.t.Helper()
	if w.jobsDuring == 0 {
		w.t.Error("the coordinator was given no job, so the command built elsewhere")
	}
}

// WhenIStartACoordinatorUntilItListens runs `vamos coordinator` with args, listening on a port of its choosing, with
// a cache folder of the scenario's own, and interrupts it once it says where it listens, or after ten seconds if it
// never does: so that one that starts ends cleanly, and one given a mistake fails before it listens.
func (w *world) WhenIStartACoordinatorUntilItListens(args ...string) {
	w.t.Helper()
	w.sendsNothing("WhenIStartACoordinatorUntilItListens")
	interruptedOnceItPrints("Coordinating builds at ").apply(w, &commandLine{})
	w.interruptBy = 10 * time.Second
	w.run(append([]string{"coordinator", "--listen", "127.0.0.1:0", "--cache-dir", w.t.TempDir()}, args...)...)
}

// GivenTheCoordinatorHasBuilt builds target on the scenario's coordinator, with --coordinator, failing the scenario
// unless the build succeeds.
func (w *world) GivenTheCoordinatorHasBuilt(target string) {
	w.t.Helper()
	w.WhenIBuild(target, onTheCoordinator("<coordinator>"))
	if w.err != nil {
		w.t.Fatalf("building %s on the coordinator: %v\n%s", target, w.err, w.outputs())
	}
	if w.jobsDuring == 0 {
		w.t.Fatalf("building %s with --coordinator gave the coordinator no job", target)
	}
}

// ThenTheCoordinatorWasSentNothing checks that the last command sent the scenario's coordinator no request.
func (w *world) ThenTheCoordinatorWasSentNothing() {
	w.t.Helper()
	if w.servedDuring != 0 {
		w.t.Errorf("the coordinator was sent %d requests, want none", w.servedDuring)
	}
}

// hasNoCoordinator stops the scenario if a Given step has started a coordinator already.
func (w *world) hasNoCoordinator() {
	w.t.Helper()
	if w.coordinator != "" {
		w.t.Fatal("the scenario has a coordinator already")
	}
}

// front makes <coordinator> a front for the coordinator that served started, once it says where it listens, so that
// steps count what is sent to it, as for every coordinator, and returns the URL it listens at itself.
// It tells watching, if it has room, once it has sent the coordinator a command's watch of a job: not as the watch
// is answered, since a job may send no event, and so no answer, until a keep-alive many seconds later.
func (w *world) front(served *server) string {
	w.t.Helper()
	address := "http://" + served.waitFor(w.t, "Coordinating builds at ")
	backend, err := url.Parse(address)
	if err != nil {
		w.t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(backend)
	proxy.FlushInterval = -1 // so that events stream
	w.watching = make(chan struct{}, 1)
	direct := proxy.Director
	proxy.Director = func(r *http.Request) {
		direct(r)
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events") {
			*r = *r.WithContext(httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{
				WroteRequest: func(info httptrace.WroteRequestInfo) {
					if info.Err != nil { // the coordinator has no watch, and the command fails to reach it
						return
					}
					select {
					case w.watching <- struct{}{}:
					default:
					}
				},
			}))
		}
	}
	w.startCoordinator(proxy)
	return address
}

// atCoordinator returns s with each <coordinator> replaced by the URL of the scenario's coordinator.
func (w *world) atCoordinator(s string) string {
	w.t.Helper()
	if strings.Contains(s, "<coordinator>") && w.coordinator == "" {
		w.t.Fatalf("%q names <coordinator>, but no Given step started one", s)
	}
	return strings.ReplaceAll(s, "<coordinator>", w.coordinator)
}

// onTheCoordinator builds on the coordinator at url, with --coordinator, in which <coordinator> stands for the
// scenario's. Every command takes it, and graph, which asks the coordinator which platform to configure for.
func onTheCoordinator(url string) option {
	return option{"onTheCoordinator", everyCommandAndGraph, func(w *world, c *commandLine) {
		w.t.Helper()
		c.flags = append(c.flags, "--coordinator", w.atCoordinator(url))
	}}
}

// GivenACoordinatorWithAWorker starts `vamos coordinator`, with args, and `vamos worker`, in this process, until the
// scenario ends: the coordinator, at <coordinator>, keeps the layers that its workers build in <registry>/vamos-cache,
// args being placeholders as atPlaceholders says, and the
// worker, which builds in stores of its own, registers with it. It waits until the worker has registered.
func (w *world) GivenACoordinatorWithAWorker(args ...string) {
	w.t.Helper()
	w.hasNoCoordinator()
	w.t.Setenv(worker.ChildEnv, "1") // the worker runs this test binary as its attempts' process
	workerStores := testenv.PrivateStores(w.t)
	ctx, cancel := context.WithCancel(context.Background())
	var coordinator, registered *server
	w.t.Cleanup(func() { // registered first, so that it runs however the step ends
		cancel()
		for _, s := range []*server{registered, coordinator} {
			if s != nil {
				s.wait(w.t, time.Minute)
			}
		}
	})
	coordinator = w.serve(ctx, &w.stores.Options, append([]string{"coordinator", "--listen", "127.0.0.1:0",
		"--cache-dir", w.t.TempDir(), "--cache-repository", w.atRegistry("<registry>/vamos-cache"), "--lease", "2s"},
		w.allAtPlaceholders(args)...)...)
	address := w.front(coordinator)
	// The worker reaches the coordinator itself, so that what it sends isn't counted as a command's.
	registered = w.serve(ctx, &workerStores.Options, "worker", "--coordinator", address, "--capacity", "2")
	// As the worker says, rather than as the coordinator's platforms do, which --offer gives before any registers.
	registered.waitFor(w.t, "worker: registered as ", coordinator)
}

// allAtPlaceholders returns args, each with placeholders replaced as atPlaceholders says.
func (w *world) allAtPlaceholders(args []string) []string {
	w.t.Helper()
	expanded := make([]string, len(args))
	for i, arg := range args {
		expanded[i] = w.atPlaceholders(arg)
	}
	return expanded
}

// ThenTheCacheRepositoryHoldsALayer checks that <registry>/vamos-cache, the coordinator's cache repository, has a
// tag: an image that a worker pushed there.
func (w *world) ThenTheCacheRepositoryHoldsALayer() {
	w.t.Helper()
	repo, err := name.NewRepository(w.atRegistry("<registry>/vamos-cache"), name.Insecure)
	if err != nil {
		w.t.Fatal(err)
	}
	tags, err := gcrremote.List(repo, w.readingTheRegistry()...)
	if err != nil || len(tags) == 0 {
		w.t.Errorf("the cache repository has tags %q, %v; want a layer that the worker pushed", tags, err)
	}
}

// server is a vamos command that serves, run by serve, until its context ends.
type server struct {
	name  string // the command, such as "vamos worker", for a failure's message
	mu    sync.Mutex
	out   bytes.Buffer
	ended chan struct{} // closed once it has ended, with err
	err   error
}

func (s *server) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out.Write(p)
}

// wait waits, for at most d, for the server to end, and returns its error, failing the test if it doesn't end.
func (s *server) wait(t *testing.T, d time.Duration) error {
	t.Helper()
	select {
	case <-s.ended:
		return s.err
	case <-time.After(d):
		t.Fatalf("%s didn't end within %s\n%s", s.name, d, s.output())
		return nil
	}
}

// output returns what the server has printed so far.
func (s *server) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out.String()
}

// waitFor waits until the server prints a line that starts with prefix, and returns the rest of the line,
// failing the test if the server, or any of others that it needs, ends first, or it prints no such line within
// twenty seconds.
func (s *server) waitFor(t *testing.T, prefix string, others ...*server) string {
	t.Helper()
	needed := append([]*server{s}, others...)
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		late := time.Now().After(deadline)
		ended := slices.IndexFunc(needed, func(n *server) bool {
			select {
			case <-n.ended:
				return true
			default:
				return false
			}
		})
		// Read after seeing an end, or the deadline, so that a line printed just before it counts.
		if rest, ok := s.printed(prefix); ok {
			return rest
		}
		if ended >= 0 {
			t.Fatalf("%s ended with %v before %s printed %q\n%s", needed[ended].name, needed[ended].err, s.name,
				prefix, outputsOf(needed))
		}
		if late {
			t.Fatalf("%s printed no %q\n%s", s.name, prefix, outputsOf(needed))
		}
	}
}

// printed returns the rest of the first whole line that the server has printed that starts with prefix, and whether
// it has printed one.
func (s *server) printed(prefix string) (string, bool) {
	lines := strings.Split(s.output(), "\n")
	for _, line := range lines[:len(lines)-1] { // whole lines only, as one may be written in parts
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
}

// outputsOf shows what each of servers has printed, for a failure's message.
func outputsOf(servers []*server) string {
	var b strings.Builder
	for _, s := range servers {
		fmt.Fprintf(&b, "%s printed:\n%s\n", s.name, s.output())
	}
	return b.String()
}

// serve runs the vamos command line args, which reads no project, on the stores that store gives, until ctx ends,
// stopping the scenario if args is empty.
func (w *world) serve(ctx context.Context, store *storage.StoreOptions, args ...string) *server {
	w.t.Helper()
	if len(args) == 0 {
		w.t.Fatal("serve runs a command that serves, but was given no command line")
	}
	s := &server{name: "vamos " + args[0], ended: make(chan struct{})}
	cmd := cli.NewRootCommand(cli.Config{Fs: afero.NewMemMapFs(), Store: store}) // which reads no project
	cmd.SetOut(s)
	cmd.SetErr(s)
	cmd.SetArgs(args)
	go func() {
		s.err = cmd.ExecuteContext(ctx)
		close(s.ended)
	}()
	return s
}

// GivenARegistryThatNeverAnswers starts a registry, at <registry>, that holds every request it is sent until the
// one who sent it gives up, or the scenario ends.
func (w *world) GivenARegistryThatNeverAnswers() {
	w.t.Helper()
	ended := make(chan struct{})
	w.startRegistry(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-ended:
		case <-r.Context().Done():
		}
	}), true)
	w.t.Cleanup(func() { close(ended) }) // after startRegistry's, so first, letting the server close
}

// GivenALocalCoordinator starts `vamos coordinator --local`, in this process, behind <coordinator>, which builds on
// the scenario's stores until WhenIInterruptTheCoordinatorWhileIBuild interrupts it, or the scenario ends.
func (w *world) GivenALocalCoordinator() {
	w.t.Helper()
	w.hasNoCoordinator()
	ctx, cancel := context.WithCancel(context.Background())
	w.t.Cleanup(func() {
		cancel()
		if w.local != nil {
			w.local.wait(w.t, time.Minute) // which ThenTheCoordinatorEndsCleanlyWithin checks, if a scenario asks
		}
	})
	w.local = w.serve(ctx, &w.stores.Options, "coordinator", "--local", "--listen", "127.0.0.1:0")
	w.interruptLocal = cancel
	w.front(w.local)
}

// WhenIInterruptTheCoordinatorWhileIBuild builds target on the coordinator of GivenALocalCoordinator and, once the
// build watches its job there, interrupts the coordinator, as Ctrl-C does, and once it has ended, closes
// <coordinator>, so that whatever the build sends from then finds no coordinator, as when one has ended.
// The build is the last command, for Then steps to check. It stops the scenario if the build ended before the
// interrupt, or if the build or the coordinator takes a minute where the step waits for it.
func (w *world) WhenIInterruptTheCoordinatorWhileIBuild(target string) {
	w.t.Helper()
	if w.local == nil {
		w.t.Fatal("no Given step started a local coordinator")
	}
	// Alongside the build, which runs as any command does, and which this touches nothing of until it has ended.
	select {
	case <-w.watching: // told of a watch by an earlier command, not this build's
	default:
	}
	var failures []string
	built, interrupted := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(interrupted)
		select {
		case <-w.watching:
		case <-built: // which the interrupt then follows, as the check below sees
		case <-time.After(time.Minute):
			failures = append(failures, "the build didn't watch its job within a minute")
		}
		w.interruptedAt = time.Now()
		w.interruptLocal()
		select {
		case <-w.local.ended:
			w.localErr, w.localTook = w.local.err, time.Since(w.interruptedAt)
		case <-time.After(time.Minute):
			failures = append(failures, "the coordinator didn't end within a minute of its interrupt")
		}
		w.coordinatorServer.Close()
	}()
	func() {
		returned := false
		defer func() { // even if the build stops the scenario, so that nothing is left changing it
			close(built)
			<-interrupted
			if !returned && len(failures) > 0 { // what the build's failure would otherwise hide
				w.t.Errorf("%s\n%s\n%s", strings.Join(failures, "; "), w.outputs(), w.local.output())
			}
		}()
		w.WhenIBuild(target, onTheCoordinator("<coordinator>"))
		returned = true
	}()
	w.buildTook = w.endedAt.Sub(w.interruptedAt) // not counting the wait for the coordinator to end
	if w.buildTook < 0 {
		failures = append(failures, "the build ended before its coordinator was interrupted")
	}
	if len(failures) > 0 {
		w.t.Fatalf("%s\n%s\n%s", strings.Join(failures, "; "), w.outputs(), w.local.output())
	}
}

// ThenTheBuildEndsWithin checks that the build of WhenIInterruptTheCoordinatorWhileIBuild ended within d of the
// coordinator's interrupt.
func (w *world) ThenTheBuildEndsWithin(d time.Duration) {
	w.t.Helper()
	if w.buildTook > d {
		w.t.Errorf("the build ended %s after its coordinator was interrupted, want within %s", w.buildTook, d)
	}
}

// ThenTheCoordinatorEndsCleanlyWithin checks that the coordinator that WhenIInterruptTheCoordinatorWhileIBuild
// interrupted ended without an error, within d of the interrupt.
func (w *world) ThenTheCoordinatorEndsCleanlyWithin(d time.Duration) {
	w.t.Helper()
	if w.localErr != nil {
		w.t.Errorf("the coordinator, interrupted, ended with %v, want a clean end\n%s", w.localErr, w.local.output())
	}
	if w.localTook > d {
		w.t.Errorf("the coordinator took %s to end once interrupted, want within %s", w.localTook, d)
	}
}
