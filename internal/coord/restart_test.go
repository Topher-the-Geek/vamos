package coord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/remote"
	"github.com/Topher-the-Geek/vamos/internal/sched"
	"github.com/Topher-the-Geek/vamos/internal/testenv"

	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// restartable is a fixture whose coordinator keeps its jobs on disk and can be restarted over the same stores and
// state, served again at the same address, as a coordinator's pod is behind its Service.
type restartable struct {
	*fixture
	t      *testing.T
	cfg    Config
	http   *http.Server
	remote *remote.Server
}

// newRestartable returns a restartable fixture configured by cfg, its state in a directory of the test's.
func newRestartable(t *testing.T, cfg Config) *restartable {
	t.Helper()
	if cfg.Lease == 0 {
		cfg.Lease = 300 * time.Millisecond
		cfg.Heartbeat = 50 * time.Millisecond
	}
	if cfg.State == "" {
		cfg.State = t.TempDir()
	}
	return newRestartableOver(t, cache.NewMemory(), cfg)
}

// newRestartableOver returns a restartable fixture over the stores m, as newRestartable does.
func newRestartableOver(t *testing.T, m *cache.Memory, cfg Config) *restartable {
	t.Helper()
	if cfg.Lease == 0 {
		cfg.Lease = 300 * time.Millisecond
		cfg.Heartbeat = 50 * time.Millisecond
	}
	r := &restartable{fixture: &fixture{m: m}, t: t, cfg: cfg}
	r.start("127.0.0.1:0")
	t.Cleanup(r.stop)
	return r
}

// start starts a coordinator, served at addr.
func (r *restartable) start(addr string) {
	r.t.Helper()
	c, err := New(r.m, r.m, r.cfg)
	if err != nil {
		r.t.Fatal(err)
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		c.Close()
		r.t.Fatal(err)
	}
	r.c, r.url = c, "http://"+listener.Addr().String()
	r.remote = &remote.Server{Executor: c, Content: r.m, Workers: c.Workers()}
	r.http = &http.Server{Handler: r.remote.Handler()}
	go r.http.Serve(listener)
}

// stop shuts the coordinator down as the coordinator command does: the streams are cut, what else is in flight is
// waited for, then the coordinator is closed.
func (r *restartable) stop() {
	r.remote.EndStreams()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.http.Shutdown(ctx); err != nil {
		r.http.Close()
	}
	r.c.Close()
}

// restart stops the coordinator and starts another over the same stores and state, at the same address.
func (r *restartable) restart() {
	r.t.Helper()
	r.stop()
	r.start(strings.TrimPrefix(r.url, "http://"))
}

// untilKilled returns a function that waits until killed says that the attempt was told to die, or the test ends,
// whichever is first, since a worker's last heartbeat may come after the test's end.
// It is called after the worker is started, so that its cleanup runs before the worker's.
func untilKilled(t *testing.T) (wait func(killed func() bool), ended func()) {
	done := make(chan struct{})
	var once sync.Once
	return func(killed func() bool) {
			for !killed() {
				select {
				case <-done:
					return
				case <-time.After(5 * time.Millisecond):
				}
			}
		}, func() {
			once.Do(func() { close(done) })
		}
}

// TestRestartResumesBuild checks that a build running when the coordinator restarts is resumed by the next run,
// which keeps the instance, so that a client's watch resumes across the restart, meets a gap that says the
// coordinator restarted, and sees the build succeed, its attempts lost with the restart run again elsewhere.
func TestRestartResumesBuild(t *testing.T) {
	r := newRestartable(t, Config{Grace: 10 * time.Second})
	wait, ended := untilKilled(t)
	blocked := startWorker(t, r.fixture, amd64, 2, fakeRun(r.fixture, func(a *remote.Assignment,
		killed func() bool) error {
		wait(killed)
		return errors.New("killed")
	}))
	t.Cleanup(ended)
	ctx := t.Context()
	client, err := remote.NewClient(r.url)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var mu sync.Mutex
	var warnings []string
	client.Warn = func(msg string) {
		mu.Lock()
		defer mu.Unlock()
		warnings = append(warnings, msg)
	}
	before, err := client.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	req := testGraph(t, r.fixture, "resumed")
	id, err := client.Submit(ctx, api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	var events []api.Event
	watched := make(chan error, 1)
	go func() {
		watched <- client.Watch(ctx, id, 0, func(e api.Event) error {
			events = append(events, e)
			return nil
		})
	}()
	waitFor(t, func() bool { return blocked.takenCount() >= 2 }) // the resolve, and a task that never ends

	r.restart()
	startWorker(t, r.fixture, amd64, 2, fakeRun(r.fixture, nil))
	select {
	case err := <-watched:
		if err != nil {
			t.Fatalf("watching across the restart: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the build didn't end after the restart")
	}
	outcome, err := client.Result(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Failure != nil || outcome.Cancelled || len(outcome.Results) != 1 {
		t.Fatalf("outcome = %+v, want the build's result", outcome)
	}
	restarted := false
	for _, e := range events {
		if e.Kind == api.EventGap && e.Restarted && e.First == restartPositions {
			restarted = true
		}
	}
	if !restarted {
		t.Errorf("events = %+v, want a gap saying the coordinator restarted, to position %d", events,
			int64(restartPositions))
	}
	after, err := client.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Instance != before.Instance {
		t.Errorf("the instance went from %s to %s, want it kept with the state", before.Instance, after.Instance)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(warnings) == 0 || !strings.Contains(warnings[0], "dropped") {
		t.Errorf("warnings = %q, want the drop the restart made", warnings)
	}
}

// postSubmission submits req to the fixture over HTTP with the submission ID id, as a client's retry does,
// and returns the job's ID and the answer's status.
func postSubmission(t *testing.T, r *restartable, id string, req api.Request) (api.JobID, int) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	post, err := http.NewRequestWithContext(t.Context(), http.MethodPost, r.url+"/v1/jobs", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	post.Header.Set("Vamos-Submission", id)
	post.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(post)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var handle struct {
		ID api.JobID `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&handle); err != nil {
		t.Fatal(err)
	}
	return handle.ID, resp.StatusCode
}

// TestRestartKeepsFinished checks that a build that finished before a restart keeps its outcome: a watch from past
// its terminal event, as a client holding it makes, delivers nothing, one from its start gets the outcome, and a
// retry of its submission gets the same job rather than another.
// The terminal event keeps its position, so a watch from before it meets a gap that doesn't say the coordinator
// restarted: the build didn't run again.
func TestRestartKeepsFinished(t *testing.T) {
	r := newRestartable(t, Config{})
	startWorker(t, r.fixture, amd64, 2, fakeRun(r.fixture, nil))
	ctx := t.Context()
	req := testGraph(t, r.fixture, "kept")
	id, status := postSubmission(t, r, "submission-1", api.Request{Graph: &req})
	if status != http.StatusCreated {
		t.Fatalf("the submission's status = %d, want %d", status, http.StatusCreated)
	}
	var end int64
	if err := r.c.Watch(ctx, id, 0, func(e api.Event) error {
		end = e.Position
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want, err := r.c.Result(ctx, id)
	if err != nil || want.Failure != nil {
		t.Fatalf("outcome = %+v, %v", want, err)
	}

	r.restart()
	got, err := r.c.Result(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 1 || got.Results[0].Image != want.Results[0].Image {
		t.Errorf("the outcome after the restart = %+v, want %+v", got, want)
	}
	held := 0
	if err := r.c.Watch(ctx, id, end+1, func(api.Event) error { held++; return nil }); err != nil || held != 0 {
		t.Errorf("a watch from past the terminal event delivered %d events, %v; want none", held, err)
	}
	var events []api.Event
	if err := r.c.Watch(ctx, id, 0, func(e api.Event) error {
		events = append(events, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != api.EventGap || events[0].Restarted || events[0].First != end ||
		events[1].Kind != api.EventDone || events[1].Position != end {
		t.Errorf("a watch from the start delivered %+v, want a gap to %d, then the terminal event there", events, end)
	}
	again, status := postSubmission(t, r, "submission-1", api.Request{Graph: &req})
	if again != id || status != http.StatusOK {
		t.Errorf("the retried submission made job %s with status %d, want job %s with %d", again, status, id,
			http.StatusOK)
	}
}

// TestRestartEndsPush checks that a push running when the coordinator restarts fails after it, since whether it
// wrote its destination is unknown, rather than being pushed again.
func TestRestartEndsPush(t *testing.T) {
	r := newRestartable(t, Config{})
	pushing := make(chan struct{}, 1)
	wait, ended := untilKilled(t)
	startWorker(t, r.fixture, amd64, 1, func(a *remote.Assignment, killed func() bool) remote.AttemptReport {
		pushing <- struct{}{}
		wait(killed)
		return remote.AttemptReport{Pushed: &api.Pushed{Image: a.Push.Result.Image}}
	})
	t.Cleanup(ended)
	ctx := t.Context()
	res := api.Result{Image: strings.Repeat("a", 64), Node: 2}
	id, err := r.c.Submit(ctx, api.Request{Push: &api.PushRequest{Result: res, Destination: "example.com/app:v1"}})
	if err != nil {
		t.Fatal(err)
	}
	<-pushing

	r.restart()
	outcome, err := r.c.Result(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Failure == nil || !strings.Contains(outcome.Failure.Message, "whether the destination "+
		"example.com/app:v1 was written is unknown") || len(outcome.Failure.Nodes) != 1 ||
		outcome.Failure.Nodes[0] != 2 {
		t.Errorf("outcome = %+v, want a failure of node 2 saying the push may have written its destination",
			outcome.Failure)
	}
}

// TestRestartKeepsCancelled checks that a build cancelled before a restart stays cancelled after it.
func TestRestartKeepsCancelled(t *testing.T) {
	r := newRestartable(t, Config{})
	started := make(chan struct{}, 4)
	wait, ended := untilKilled(t)
	startWorker(t, r.fixture, amd64, 2, fakeRun(r.fixture, func(a *remote.Assignment, killed func() bool) error {
		started <- struct{}{}
		wait(killed)
		return errors.New("killed")
	}))
	t.Cleanup(ended)
	ctx := t.Context()
	req := testGraph(t, r.fixture, "cancelled")
	id, err := r.c.Submit(ctx, api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := r.c.Cancel(ctx, id); err != nil {
		t.Fatal(err)
	}

	r.restart()
	if err := r.c.Watch(ctx, id, 0, func(api.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	outcome, err := r.c.Result(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Cancelled {
		t.Errorf("outcome = %+v, want it cancelled", outcome)
	}
}

// TestRestartRemembersExpired checks that a build evicted before a restart is still known to have expired after it,
// rather than taken for one the coordinator never had.
func TestRestartRemembersExpired(t *testing.T) {
	r := newRestartable(t, Config{Grace: 100 * time.Millisecond})
	startWorker(t, r.fixture, amd64, 2, fakeRun(r.fixture, nil))
	ctx := t.Context()
	outcome, _ := runJob(t, r.c, testGraph(t, r.fixture, "expired"))
	if outcome.Failure != nil {
		t.Fatalf("outcome = %+v", outcome.Failure)
	}
	ids, err := os.ReadDir(filepath.Join(r.cfg.State, "jobs"))
	if err != nil || len(ids) != 1 {
		t.Fatalf("the jobs recorded = %v, %v; want the build's", ids, err)
	}
	id := api.JobID(strings.TrimSuffix(ids[0].Name(), ".json"))
	if err := r.c.Release(ctx, id); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		_, err := r.c.Result(ctx, id)
		return errors.Is(err, api.ErrExpired)
	})

	r.restart()
	if _, err := r.c.Result(ctx, id); !errors.Is(err, api.ErrExpired) {
		t.Errorf("the build's result after the restart: %v, want %v", err, api.ErrExpired)
	}
}

// TestRestartForgetsUnreadable checks that a job's record that can't be read is reported and removed, so that the
// coordinator starts without the job rather than failing to start.
func TestRestartForgetsUnreadable(t *testing.T) {
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "jobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(state, "jobs", "0123456789abcdef.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var warned []error
	r := newRestartable(t, Config{State: state, Warn: func(err error) { warned = append(warned, err) }})
	if len(warned) != 1 || !strings.Contains(warned[0].Error(), "forgetting the job 0123456789abcdef, whose "+
		"record can't be read") {
		t.Errorf("warnings = %v, want one saying the job is forgotten", warned)
	}
	if _, err := os.Stat(broken); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the unreadable record: %v, want it removed", err)
	}
	if _, err := r.c.Result(t.Context(), "0123456789abcdef"); !errors.Is(err, api.ErrNoJob) {
		t.Errorf("the forgotten job's result: %v, want %v", err, api.ErrNoJob)
	}
}

// blockedRun returns a handler whose layers and tasks run until their worker is told to kill them, or the test ends,
// and the function that ends them, to be given to t.Cleanup after each worker that runs them is started.
func blockedRun(t *testing.T, f *fixture) (func(a *remote.Assignment, killed func() bool) remote.AttemptReport,
	func()) {
	wait, ended := untilKilled(t)
	return fakeRun(f, func(a *remote.Assignment, killed func() bool) error {
		wait(killed)
		return errors.New("killed")
	}), ended
}

// submitBuild submits a build of testGraph's graph with nonce to the fixture's coordinator.
func submitBuild(t *testing.T, r *restartable, nonce string) api.JobID {
	t.Helper()
	req := testGraph(t, r.fixture, nonce)
	id, err := r.c.Submit(t.Context(), api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// watchAll watches the job id from position from to its end and returns the events delivered.
func watchAll(t *testing.T, r *restartable, id api.JobID, from int64) []api.Event {
	t.Helper()
	var events []api.Event
	if err := r.c.Watch(t.Context(), id, from, func(e api.Event) error {
		events = append(events, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return events
}

// jobRecords returns the names of the records in the fixture's journal.
func jobRecords(t *testing.T, r *restartable) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(r.cfg.State, "jobs"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestRestartTwice checks that a build resumed twice gives its events past both earlier runs' positions, its
// restarts recorded each time, so that a watcher from the first resumed run's positions meets a restart's gap too.
func TestRestartTwice(t *testing.T) {
	r := newRestartable(t, Config{})
	blocked, ended := blockedRun(t, r.fixture)
	first := startWorker(t, r.fixture, amd64, 2, blocked)
	t.Cleanup(ended)
	id := submitBuild(t, r, "twice")
	waitFor(t, func() bool { return first.takenCount() >= 2 })

	r.restart()
	second := startWorker(t, r.fixture, amd64, 2, blocked)
	t.Cleanup(ended)
	waitFor(t, func() bool { return second.takenCount() >= 1 })

	r.restart()
	startWorker(t, r.fixture, amd64, 2, fakeRun(r.fixture, nil))
	events := watchAll(t, r, id, restartPositions)
	if len(events) == 0 || events[0].Kind != api.EventGap || !events[0].Restarted ||
		events[0].First != 2*restartPositions {
		t.Fatalf("events = %+v, want a gap saying the coordinator restarted, to position %d", events,
			int64(2*restartPositions))
	}
	if done := events[len(events)-1]; done.Kind != api.EventDone || done.Outcome.Failure != nil {
		t.Errorf("the last event = %+v, want the build's success", done)
	}
}

// TestRestartConcludesRefusedResume checks that a build that a restart can't resume, since fewer builds are allowed
// than before, fails, and that its failure is recorded, so that the next restart keeps it rather than trying again.
func TestRestartConcludesRefusedResume(t *testing.T) {
	r := newRestartable(t, Config{MaxBuilds: 2})
	blocked, ended := blockedRun(t, r.fixture)
	w := startWorker(t, r.fixture, amd64, 4, blocked)
	t.Cleanup(ended)
	ids := []api.JobID{submitBuild(t, r, "refused 1"), submitBuild(t, r, "refused 2")}
	waitFor(t, func() bool { return w.takenCount() >= 3 }) // the resolve, and a task of each

	r.cfg.MaxBuilds = 1
	r.restart()
	refused := api.JobID("")
	for _, id := range ids {
		if _, err := r.c.Result(t.Context(), id); !errors.Is(err, api.ErrRunning) {
			refused = id
		}
	}
	if refused == "" {
		t.Fatal("both builds were resumed, though one is allowed")
	}

	r.restart()
	outcome, err := r.c.Result(t.Context(), refused)
	if err != nil || outcome.Failure == nil ||
		!strings.Contains(outcome.Failure.Message, "couldn't resume the build") {
		t.Fatalf("the refused build's outcome after another restart = %+v, %v; want its recorded failure",
			outcome.Failure, err)
	}
	events := watchAll(t, r, refused, 0)
	if len(events) != 2 || !events[0].Restarted || events[1].Position != restartPositions {
		t.Errorf("events = %+v, want a gap saying the coordinator restarted, then the outcome at %d", events,
			int64(restartPositions))
	}
}

// TestRestartAfterCrash checks that a build is resumed from the journal as a crash leaves it, with no shutdown:
// a coordinator started on a copy of the state, taken while the build ran, resumes and finishes it.
func TestRestartAfterCrash(t *testing.T) {
	r := newRestartable(t, Config{})
	blocked, ended := blockedRun(t, r.fixture)
	w := startWorker(t, r.fixture, amd64, 2, blocked)
	t.Cleanup(ended)
	id := submitBuild(t, r, "crash")
	waitFor(t, func() bool { return w.takenCount() >= 2 })
	crashed := t.TempDir()
	if err := os.CopyFS(crashed, os.DirFS(r.cfg.State)); err != nil {
		t.Fatal(err)
	}

	after := newRestartableOver(t, r.m, Config{State: crashed})
	startWorker(t, after.fixture, amd64, 2, fakeRun(after.fixture, nil))
	events := watchAll(t, after, id, 0)
	if done := events[len(events)-1]; done.Kind != api.EventDone || done.Outcome.Failure != nil {
		t.Errorf("the last event = %+v, want the build's success", done)
	}
}

// TestStateLocked checks that a second coordinator can't open a state that one has open.
func TestStateLocked(t *testing.T) {
	r := newRestartable(t, Config{})
	_, err := New(r.m, r.m, Config{State: r.cfg.State})
	if err == nil || !strings.Contains(err.Error(), "another coordinator is using it") {
		t.Errorf("a second coordinator on the state: %v, want it refused", err)
	}
}

// TestRestartEndsReleased checks that a build released while it ran, which no client wants, ends with a restart
// rather than running again.
func TestRestartEndsReleased(t *testing.T) {
	r := newRestartable(t, Config{})
	blocked, ended := blockedRun(t, r.fixture)
	w := startWorker(t, r.fixture, amd64, 2, blocked)
	t.Cleanup(ended)
	id := submitBuild(t, r, "released")
	waitFor(t, func() bool { return w.takenCount() >= 2 })
	if err := r.c.Release(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	r.restart()
	outcome, err := r.c.Result(t.Context(), id)
	if err != nil || outcome.Failure == nil || outcome.Failure.Message != sched.ErrWithdrawn.Error() {
		t.Errorf("the released build's outcome = %+v, %v; want it withdrawn", outcome.Failure, err)
	}
}

// TestRefusedSubmissionUnrecorded checks that a submission the coordinator refuses leaves no record, so that no
// restart resumes a job its client was told failed.
func TestRefusedSubmissionUnrecorded(t *testing.T) {
	r := newRestartable(t, Config{MaxBuilds: 1})
	blocked, ended := blockedRun(t, r.fixture)
	w := startWorker(t, r.fixture, amd64, 2, blocked)
	t.Cleanup(ended)
	submitBuild(t, r, "admitted")
	waitFor(t, func() bool { return w.takenCount() >= 2 })
	req := testGraph(t, r.fixture, "refused")
	if _, err := r.c.Submit(t.Context(), api.Request{Graph: &req}); !errors.Is(err, sched.ErrBusy) {
		t.Fatalf("the second submission: %v, want %v", err, sched.ErrBusy)
	}
	if records := jobRecords(t, r); len(records) != 1 {
		t.Errorf("the jobs recorded = %q, want the admitted build's alone", records)
	}
}

// TestRecordWriteFails checks that a build whose record can't be written is refused, starting nothing, and that an
// outcome that can't be recorded is reported while the build still ends with it.
func TestRecordWriteFails(t *testing.T) {
	var mu sync.Mutex
	var warned []error
	r := newRestartable(t, Config{Warn: func(err error) {
		mu.Lock()
		defer mu.Unlock()
		warned = append(warned, err)
	}})
	gate := make(chan struct{})
	startWorker(t, r.fixture, amd64, 2, fakeRun(r.fixture, func(*remote.Assignment, func() bool) error {
		<-gate
		return nil
	}))
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	})
	id := submitBuild(t, r, "unrecorded outcome")
	// Records are written through the journal's tmp directory: a file in its place fails every write.
	tmp := filepath.Join(r.cfg.State, "tmp")
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	req := testGraph(t, r.fixture, "unrecorded")
	if _, err := r.c.Submit(t.Context(), api.Request{Graph: &req}); err == nil ||
		!strings.Contains(err.Error(), "recording the job") {
		t.Errorf("a submission whose record can't be written: %v, want it refused", err)
	}
	if n := r.c.jobs.Active(); n != 1 {
		t.Errorf("%d jobs are active, want the first build's alone", n)
	}
	close(gate)
	events := watchAll(t, r, id, 0)
	if done := events[len(events)-1]; done.Outcome.Failure != nil {
		t.Errorf("the build's outcome = %+v, want its success", done.Outcome.Failure)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(warned) != 1 || !strings.Contains(warned[0].Error(), "recording how the job "+string(id)+" ended") {
		t.Errorf("warnings = %v, want one saying the build's outcome wasn't recorded", warned)
	}
	// The state is made writable again, so that the fixture's shutdown and cleanup find it as they expect.
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
}

// TestRestartKeepsNativePlatforms checks that a build resumed after a restart, before the workers of its platforms
// register again, still runs each platform's commands natively: it waits for a worker of the platform that only a
// worker offered when it was submitted, rather than refusing them as emulated.
func TestRestartKeepsNativePlatforms(t *testing.T) {
	r := newRestartable(t, Config{Grace: 10 * time.Second})
	handle, ended := blockedRun(t, r.fixture)
	blocked := startWorker(t, r.fixture, amd64, 2, handle)
	startWorker(t, r.fixture, arm64, 2, handle)
	t.Cleanup(ended)
	req := indexGraph(t, r.fixture, "restarted", true)
	id, err := r.c.Submit(t.Context(), api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return blocked.takenCount() >= 2 }) // a resolve, and a task that never ends

	r.restart()
	ran := &ranOn{runs: map[string][]string{}, kind: map[string]string{}}
	startWorker(t, r.fixture, amd64, 2, ran.handler(r.fixture, amd64))
	watched := make(chan []api.Event, 1)
	go func() { watched <- watchAll(t, r, id, 0) }()
	select {
	case <-watched:
		outcome, _ := r.c.Result(t.Context(), id)
		t.Fatalf("the build ended without an arm64 worker: %+v", outcome.Failure)
	case <-time.After(500 * time.Millisecond):
	}
	startWorker(t, r.fixture, arm64, 2, ran.handler(r.fixture, arm64))
	select {
	case <-watched:
	case <-time.After(15 * time.Second):
		t.Fatal("the build didn't end once an arm64 worker registered")
	}
	outcome, err := r.c.Result(t.Context(), id)
	if err != nil || outcome.Failure != nil {
		t.Fatalf("outcome = %+v, %v; want the build's result", outcome.Failure, err)
	}
	for key, runs := range ran.runs {
		if ran.kind[key] == "layer linux/arm64" && !slices.Equal(runs, []string{"linux/arm64"}) {
			t.Errorf("the arm64 layer ran on %q, want the arm64 worker alone", runs)
		}
	}
}

// TestRestartKeepsPlatformsOfferedLater checks that a platform offered only once the build had started, by a worker
// that registered then, which the build reached, is recorded with it, so that the build resumed after a restart,
// before that worker registers again, waits for one rather than refusing its commands as emulated.
func TestRestartKeepsPlatformsOfferedLater(t *testing.T) {
	r := newRestartable(t, Config{Grace: 10 * time.Second})
	release := make(chan struct{})
	run := fakeRun(r.fixture, nil)
	startWorker(t, r.fixture, amd64, 2, func(a *remote.Assignment, killed func() bool) remote.AttemptReport {
		if a.Request != nil && a.Request.Action.Kind == sched.KindTask {
			select { // the tasks wait until the arm64 worker registered
			case <-release:
			case <-time.After(10 * time.Second):
			}
		}
		return run(a, killed)
	})
	req := indexGraph(t, r.fixture, "later", true)
	id, err := r.c.Submit(t.Context(), api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	handle, ended := blockedRun(t, r.fixture)
	later := startWorker(t, r.fixture, arm64, 2, handle)
	t.Cleanup(ended)
	close(release)
	waitFor(t, func() bool { return later.takenCount() >= 1 }) // the arm64 layer, which never ends
	recorded := func() []string {
		r.c.journal.mu.Lock()
		defer r.c.journal.mu.Unlock()
		return r.c.journal.records[id].Native
	}
	if got := recorded(); !slices.Equal(got, []string{"linux/amd64", "linux/arm64"}) {
		t.Errorf("the build's record runs %q natively, want both platforms", got)
	}

	r.restart()
	ran := &ranOn{runs: map[string][]string{}, kind: map[string]string{}}
	startWorker(t, r.fixture, amd64, 2, ran.handler(r.fixture, amd64))
	watched := make(chan []api.Event, 1)
	go func() { watched <- watchAll(t, r, id, 0) }()
	select {
	case <-watched:
		outcome, _ := r.c.Result(t.Context(), id)
		t.Fatalf("the build ended without an arm64 worker: %+v", outcome.Failure)
	case <-time.After(500 * time.Millisecond):
	}
	startWorker(t, r.fixture, arm64, 2, ran.handler(r.fixture, arm64))
	select {
	case <-watched:
	case <-time.After(15 * time.Second):
		t.Fatal("the build didn't end once an arm64 worker registered")
	}
	outcome, err := r.c.Result(t.Context(), id)
	if err != nil || outcome.Failure != nil {
		t.Fatalf("outcome = %+v, %v; want the build's result", outcome.Failure, err)
	}
}

// TestPlatformWriteFails checks that a build that reaches a platform offered once it had started, whose record then
// can't be written, reports it and runs that platform's commands natively all the same, as the other records of a
// running build are best-effort.
func TestPlatformWriteFails(t *testing.T) {
	var mu sync.Mutex
	var warned []error
	r := newRestartable(t, Config{Warn: func(err error) {
		mu.Lock()
		defer mu.Unlock()
		warned = append(warned, err)
	}})
	release := make(chan struct{})
	ran := &ranOn{runs: map[string][]string{}, kind: map[string]string{}}
	run := ran.handler(r.fixture, amd64)
	startWorker(t, r.fixture, amd64, 2, func(a *remote.Assignment, killed func() bool) remote.AttemptReport {
		if a.Request != nil && a.Request.Action.Kind == sched.KindTask {
			select { // the tasks wait until the arm64 worker registered
			case <-release:
			case <-time.After(10 * time.Second):
			}
		}
		return run(a, killed)
	})
	id := func() api.JobID {
		req := indexGraph(t, r.fixture, "unrecorded platform", true)
		id, err := r.c.Submit(t.Context(), api.Request{Graph: &req})
		testenv.Must(t, err)
		return id
	}()
	// Records are written through the journal's tmp directory: a file in its place fails every write.
	tmp := filepath.Join(r.cfg.State, "tmp")
	testenv.Must(t, os.RemoveAll(tmp))
	testenv.Must(t, os.WriteFile(tmp, nil, 0o600))
	startWorker(t, r.fixture, arm64, 2, ran.handler(r.fixture, arm64))
	close(release)
	events := watchAll(t, r, id, 0)
	if done := events[len(events)-1]; done.Outcome.Failure != nil {
		t.Fatalf("the build's outcome = %+v, want its success", done.Outcome.Failure)
	}
	for key, runs := range ran.runs {
		if ran.kind[key] == "layer linux/arm64" && !slices.Equal(runs, []string{"linux/arm64"}) {
			t.Errorf("the arm64 layer ran on %q, want the arm64 worker alone, natively", runs)
		}
	}
	mu.Lock()
	if !slices.ContainsFunc(warned, func(err error) bool {
		return strings.Contains(err.Error(), "recording that the build "+string(id)+
			" runs commands natively on linux/arm64")
	}) {
		t.Errorf("warnings = %v, want one saying the arm64 platform wasn't recorded", warned)
	}
	mu.Unlock()
	// The state is made writable again, so that the fixture's shutdown and cleanup find it as they expect.
	testenv.Must(t, os.Remove(tmp))
	testenv.Must(t, os.Mkdir(tmp, 0o700))
}

// TestRestartResumesUnrecordedPlatforms checks that a build recorded before builds ran natively on several platforms,
// whose record names none, resumes running natively on the platforms configured, recording them, so that it waits
// for a worker of one rather than refusing its commands as emulated.
func TestRestartResumesUnrecordedPlatforms(t *testing.T) {
	r := newRestartable(t, Config{Grace: 10 * time.Second, Platforms: []imgspecv1.Platform{amd64, arm64}})
	handle, ended := blockedRun(t, r.fixture)
	blocked := startWorker(t, r.fixture, amd64, 2, handle)
	t.Cleanup(ended)
	req := indexGraph(t, r.fixture, "unrecorded", true)
	id, err := r.c.Submit(t.Context(), api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return blocked.takenCount() >= 2 })

	// The record is rewritten as the version before wrote it, while the coordinator is down.
	r.stop()
	path := filepath.Join(r.cfg.State, "jobs", string(id)+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	testenv.Must(t, json.Unmarshal(data, &fields))
	if _, ok := fields["native"]; !ok {
		t.Fatalf("the record %s names no platforms to remove", data)
	}
	delete(fields, "native")
	data, err = json.Marshal(fields)
	testenv.Must(t, err)
	testenv.Must(t, os.WriteFile(path, data, 0o600))
	r.start(strings.TrimPrefix(r.url, "http://"))

	r.c.journal.mu.Lock()
	native := r.c.journal.records[id].Native
	r.c.journal.mu.Unlock()
	if !slices.Equal(native, []string{"linux/amd64", "linux/arm64"}) {
		t.Errorf("the resumed build's record runs %q natively, want the configured platforms", native)
	}
	ran := &ranOn{runs: map[string][]string{}, kind: map[string]string{}}
	startWorker(t, r.fixture, amd64, 2, ran.handler(r.fixture, amd64))
	watched := make(chan []api.Event, 1)
	go func() { watched <- watchAll(t, r, id, 0) }()
	select {
	case <-watched:
		outcome, _ := r.c.Result(t.Context(), id)
		t.Fatalf("the build ended without an arm64 worker: %+v", outcome.Failure)
	case <-time.After(500 * time.Millisecond):
	}
	startWorker(t, r.fixture, arm64, 2, ran.handler(r.fixture, arm64))
	select {
	case <-watched:
	case <-time.After(15 * time.Second):
		t.Fatal("the build didn't end once an arm64 worker registered")
	}
	if outcome, err := r.c.Result(t.Context(), id); err != nil || outcome.Failure != nil {
		t.Fatalf("outcome = %+v, %v; want the build's result", outcome.Failure, err)
	}
}
