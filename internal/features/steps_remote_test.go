package features

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/remote"
	"github.com/Topher-the-Geek/vamos/internal/run"
	"github.com/Topher-the-Geek/vamos/internal/testenv"
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
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/capabilities", func(rw http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(rw).Encode(api.Capabilities{GraphVersions: versions})
	})
	w.startCoordinator(mux)
}

// startCoordinator serves handler, at <coordinator>, until the scenario ends.
func (w *world) startCoordinator(handler http.Handler) {
	w.t.Helper()
	if w.coordinator != "" {
		w.t.Fatal("the scenario has a coordinator already")
	}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.served.Add(1)
		if r.Method == http.MethodPost && r.URL.Path == "/v1/jobs" { // remote's pathJobs
			w.jobs.Add(1)
		}
		handler.ServeHTTP(rw, r)
	}))
	w.t.Cleanup(server.Close)
	w.coordinator = server.URL
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

// atCoordinator returns s with each <coordinator> replaced by the URL of the scenario's coordinator.
func (w *world) atCoordinator(s string) string {
	w.t.Helper()
	if strings.Contains(s, "<coordinator>") && w.coordinator == "" {
		w.t.Fatalf("%q names <coordinator>, but no Given step started one", s)
	}
	return strings.ReplaceAll(s, "<coordinator>", w.coordinator)
}

// onTheCoordinator builds on the coordinator at url, with --coordinator, in which <coordinator> stands for the
// scenario's. Every command takes it.
func onTheCoordinator(url string) option {
	return option{"onTheCoordinator", everyCommand, func(w *world, c *commandLine) {
		w.t.Helper()
		c.flags = append(c.flags, "--coordinator", w.atCoordinator(url))
	}}
}
