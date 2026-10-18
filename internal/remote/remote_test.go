package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/cache/cachetest"
	"github.com/Topher-the-Geek/vamos/internal/graph"

	"github.com/opencontainers/go-digest"
)

// fakeExecutor is an executor whose jobs are scripted: each has its events, the last the terminal one,
// and Watch delivers them from the position asked, as api.Executor describes.
type fakeExecutor struct {
	mu       sync.Mutex
	caps     api.Capabilities
	submits  int
	jobs     map[api.JobID][]api.Event
	watches  []int64 // the position each Watch started from
	fellAt   int     // if > 0, a watch falls behind after delivering this many events
	nextID   int
	cancels  []api.JobID
	outcomes map[api.JobID]api.Outcome
}

// newFakeExecutor returns an executor with one job, "job-1", of count events, which claims to be instance "i-1".
func newFakeExecutor(count int) *fakeExecutor {
	f := &fakeExecutor{
		caps:     api.Capabilities{Instance: "i-1", GracePeriod: time.Minute, GraphVersions: []int{graph.Version}},
		jobs:     map[api.JobID][]api.Event{},
		outcomes: map[api.JobID]api.Outcome{},
	}
	f.jobs["job-1"] = scriptedEvents(count)
	return f
}

// scriptedEvents returns count events, status events then a terminal one, positioned.
func scriptedEvents(count int) []api.Event {
	var events []api.Event
	for i := range count - 1 {
		events = append(events, api.Event{Position: int64(i), Kind: api.EventStatus, Nodes: []graph.Ref{0},
			Status: fmt.Sprintf("step %d", i)})
	}
	return append(events, api.Event{Position: int64(count - 1), Kind: api.EventDone, Outcome: &api.Outcome{}})
}

func (f *fakeExecutor) Capabilities(context.Context) (api.Capabilities, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.caps.Clone(), nil
}

func (f *fakeExecutor) Submit(_ context.Context, req api.Request) (api.JobID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Graph == nil && req.Push == nil {
		return "", errors.New("refused: no graph")
	}
	f.submits++
	f.nextID++
	id := api.JobID(fmt.Sprintf("submitted-%d", f.nextID))
	f.jobs[id] = scriptedEvents(2)
	return id, nil
}

func (f *fakeExecutor) Watch(ctx context.Context, id api.JobID, from int64, fn func(api.Event) error) error {
	f.mu.Lock()
	events, ok := f.jobs[id]
	f.watches = append(f.watches, from)
	fellAt := f.fellAt
	f.mu.Unlock()
	if !ok {
		return api.ErrNoJob
	}
	delivered := 0
	for i := from; i < int64(len(events)); i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if fellAt > 0 && delivered == fellAt {
			return fmt.Errorf("%w: the events from %d are no longer retained", api.ErrFellBehind, i)
		}
		if err := fn(events[i]); err != nil {
			return err
		}
		delivered++
	}
	return nil
}

func (f *fakeExecutor) Cancel(_ context.Context, id api.JobID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.jobs[id]; !ok {
		return api.ErrNoJob
	}
	f.cancels = append(f.cancels, id)
	return nil
}

func (f *fakeExecutor) Result(_ context.Context, id api.JobID) (api.Outcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.jobs[id]; !ok {
		return api.Outcome{}, api.ErrNoJob
	}
	if o, ok := f.outcomes[id]; ok {
		return o, nil
	}
	return api.Outcome{}, api.ErrRunning
}

func (f *fakeExecutor) Release(_ context.Context, id api.JobID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.jobs[id]; !ok {
		return api.ErrNoJob
	}
	return nil
}

func (f *fakeExecutor) Prune(context.Context) (api.JobID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submits++
	f.nextID++
	id := api.JobID(fmt.Sprintf("prune-%d", f.nextID))
	f.jobs[id] = scriptedEvents(1)
	return id, nil
}

// faults injects connection drops into a server: a response that is never sent, as a lost acknowledgement,
// and an event stream cut after some events, as a dropped connection.
// A drop is a panic with http.ErrAbortHandler, which makes net/http close the connection without an answer.
type faults struct {
	mu sync.Mutex
	// dropSubmits is how many more submissions' responses to drop, after the executor has accepted them.
	dropSubmits int
	// dropped, if set, is called after each submission's response is dropped, as when the coordinator restarts.
	dropped func()
	// cuts is how many lines, events and keep-alives, each of the next event streams delivers before it is cut,
	// in turn; 0 cuts none.
	cuts []int
	// down drops every request while set.
	down bool
}

// middleware wraps h with the faults.
func (f *faults) middleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		down := f.down
		f.mu.Unlock()
		if down {
			panic(http.ErrAbortHandler)
		}
		if r.Method == http.MethodPost && r.URL.Path == pathJobs {
			f.mu.Lock()
			drop := f.dropSubmits > 0
			if drop {
				f.dropSubmits--
			}
			f.mu.Unlock()
			if drop {
				h.ServeHTTP(httptest.NewRecorder(), r) // the job is accepted, and the answer lost
				if f.dropped != nil {
					f.dropped()
				}
				panic(http.ErrAbortHandler)
			}
		}
		if strings.HasSuffix(r.URL.Path, "/events") {
			f.mu.Lock()
			cut := 0
			if len(f.cuts) > 0 {
				cut, f.cuts = f.cuts[0], f.cuts[1:]
			}
			f.mu.Unlock()
			if cut > 0 {
				w = &cutting{ResponseWriter: w, after: cut}
			}
		}
		h.ServeHTTP(w, r)
	})
}

// cutting is a response writer that drops the connection after some flushes: each event is flushed.
type cutting struct {
	http.ResponseWriter
	after   int
	flushed int
}

// Flush flushes, and cuts the connection once enough has been.
func (c *cutting) Flush() {
	c.ResponseWriter.(http.Flusher).Flush()
	c.flushed++
	if c.flushed == c.after {
		panic(http.ErrAbortHandler)
	}
}

// fixture serves ex and a store in memory, through f, and returns a client of them and the store.
// The client has answered capabilities once, so that it retries drops.
func fixture(t *testing.T, ex api.Executor, f *faults) (*Client, *cache.Memory) {
	t.Helper()
	m := cache.NewMemory()
	s := &Server{Executor: ex, Content: m, KeepAlive: 50 * time.Millisecond}
	if f != nil {
		s.Middleware = f.middleware
	}
	server := httptest.NewServer(s.Handler())
	t.Cleanup(server.Close)
	c, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := c.Capabilities(t.Context()); err != nil {
		t.Fatal(err)
	}
	return c, m
}

// TestContentStoreContract runs the content store contract through a server and client, over a store in memory.
func TestContentStoreContract(t *testing.T) {
	cachetest.Content(t, cachetest.Fixture{New: func(t *testing.T) cachetest.Stores {
		c, m := fixture(t, newFakeExecutor(1), nil)
		return cachetest.Stores{Content: c, Unclearable: true, Corrupt: func(t *testing.T, b cache.Blob, data []byte) {
			m.Replace(b, data)
		}}
	}})
}

// TestErrorsCross checks that each sentinel error crosses as itself, for errors.Is, with its message,
// through answers with and without a body, and that any other error crosses by its message.
func TestErrorsCross(t *testing.T) {
	for _, s := range sentinels {
		t.Run(string(s.code), func(t *testing.T) {
			err := fmt.Errorf("something about %w", s.err)
			we, status := encodeError(err)
			if status != s.status || we.Code != s.code || we.Message != err.Error() {
				t.Errorf("encodeError = %+v, %d; want code %s with status %d", we, status, s.code, s.status)
			}
			rec := httptest.NewRecorder()
			writeError(rec, httptest.NewRequest(http.MethodGet, "/", nil), err)
			resp := rec.Result()
			resp.Request = httptest.NewRequest(http.MethodGet, "/", nil)
			if got := readError(resp); !errors.Is(got, s.err) || got.Error() != err.Error() {
				t.Errorf("readError = %v, want %v as %v", got, err, s.err)
			}
			rec = httptest.NewRecorder()
			head := httptest.NewRequest(http.MethodHead, "/", nil)
			writeError(rec, head, err)
			resp = rec.Result()
			resp.Request = head
			if got := readError(resp); !errors.Is(got, s.err) || rec.Body.Len() != 0 {
				t.Errorf("readError of a HEAD answer = %v with body %q, want %v and no body", got, rec.Body, s.err)
			}
			// An error that crossed once keeps its code when it crosses again, as a coordinator relays a worker's.
			if again, _ := encodeError(fmt.Errorf("relayed: %w", we)); again.Code != s.code {
				t.Errorf("an error that crossed encodes again as %+v, want code %s", again, s.code)
			}
		})
	}
	other := errors.New("something else")
	we, status := encodeError(other)
	if we.Code != "" || status != http.StatusInternalServerError || we.Message != other.Error() {
		t.Errorf("encodeError of a plain error = %+v, %d", we, status)
	}
	for _, s := range sentinels {
		if we.Is(s.err) {
			t.Errorf("a plain error is %v", s.err)
		}
	}
}

// TestExecutorThroughHTTP checks the executor's calls through a server and client: capabilities, a job's events
// and outcome, cancel, and the sentinels for an unknown job and a running one.
func TestExecutorThroughHTTP(t *testing.T) {
	ex := newFakeExecutor(4)
	c, _ := fixture(t, ex, nil)
	ctx := t.Context()
	caps, err := c.Capabilities(ctx)
	if err != nil || caps.Instance != "i-1" || caps.GracePeriod != time.Minute {
		t.Fatalf("Capabilities = %+v, %v", caps, err)
	}
	var got []api.Event
	if err := c.Watch(ctx, "job-1", 1, func(e api.Event) error { got = append(got, e); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Position != 1 || got[2].Kind != api.EventDone || got[0].Status != "step 1" {
		t.Errorf("events from 1 = %+v, want positions 1 to 3 ending with the terminal event", got)
	}
	if _, err := c.Result(ctx, "job-1"); !errors.Is(err, api.ErrRunning) {
		t.Errorf("Result of a running job = %v, want %v", err, api.ErrRunning)
	}
	ex.mu.Lock()
	ex.outcomes["job-1"] = api.Outcome{Cancelled: true}
	ex.mu.Unlock()
	if outcome, err := c.Result(ctx, "job-1"); err != nil || !outcome.Cancelled {
		t.Errorf("Result = %+v, %v; want the outcome", outcome, err)
	}
	if err := c.Cancel(ctx, "job-1"); err != nil || len(ex.cancels) != 1 {
		t.Errorf("Cancel = %v with %v cancelled; want the job cancelled", err, ex.cancels)
	}
	for name, call := range map[string]func() error{
		"Watch":  func() error { return c.Watch(ctx, "nope", 0, func(api.Event) error { return nil }) },
		"Cancel": func() error { return c.Cancel(ctx, "nope") },
		"Result": func() error { _, err := c.Result(ctx, "nope"); return err },
	} {
		if err := call(); !errors.Is(err, api.ErrNoJob) {
			t.Errorf("%s of an unknown job = %v, want %v", name, err, api.ErrNoJob)
		}
	}
	id, err := c.Submit(ctx, api.Request{Graph: &api.GraphRequest{}})
	if err != nil || id != "submitted-1" {
		t.Errorf("Submit = %q, %v", id, err)
	}
	if _, err := c.Submit(ctx, api.Request{}); err == nil || !strings.Contains(err.Error(), "refused: no graph") {
		t.Errorf("a refused submission = %v, want the executor's message", err)
	}
	if id, err := c.Prune(ctx); err != nil || id != "prune-2" {
		t.Errorf("Prune = %q, %v", id, err)
	}
	// A watcher's own error ends the watch with that error, as it is.
	stop := errors.New("stop")
	if err := c.Watch(ctx, "job-1", 0, func(api.Event) error { return stop }); err == nil ||
		err.Error() != "stop" {
		t.Errorf("Watch with a failing fn = %v, want %v", err, stop)
	}
	ex.mu.Lock()
	ex.fellAt = 2
	ex.mu.Unlock()
	delivered := 0
	err = c.Watch(ctx, "job-1", 0, func(api.Event) error { delivered++; return nil })
	if !errors.Is(err, api.ErrFellBehind) || delivered != 2 {
		t.Errorf("Watch that fell behind = %v after %d events, want %v after 2", err, delivered, api.ErrFellBehind)
	}
}

// TestSubmitResubmits checks that a submission whose acknowledgement is lost is retried with the same ID,
// so that the executor starts one job, which both attempts name, and that the owner is warned of the drop.
func TestSubmitResubmits(t *testing.T) {
	ex := newFakeExecutor(1)
	f := &faults{dropSubmits: 1}
	c, _ := fixture(t, ex, f)
	id, err := c.Submit(t.Context(), api.Request{Graph: &api.GraphRequest{}})
	if err != nil || id != "submitted-1" {
		t.Fatalf("Submit = %q, %v", id, err)
	}
	if ex.submits != 1 {
		t.Errorf("the executor accepted %d submissions, want 1", ex.submits)
	}
	if again, err := c.Submit(t.Context(), api.Request{Graph: &api.GraphRequest{}}); err != nil || again == id {
		t.Errorf("another submission = %q, %v; want another job", again, err)
	}
}

// TestWatchResumes checks that a watch whose connection drops resumes from the last position delivered,
// so that every event arrives once, and that the owner is warned of the drop and the return.
func TestWatchResumes(t *testing.T) {
	ex := newFakeExecutor(6)
	f := &faults{cuts: []int{2}}
	c, _ := fixture(t, ex, f)
	var warnings []string
	c.Warn = func(msg string) { warnings = append(warnings, msg) }
	var positions []int64
	err := c.Watch(t.Context(), "job-1", 0, func(e api.Event) error {
		positions = append(positions, e.Position)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{0, 1, 2, 3, 4, 5}; fmt.Sprint(positions) != fmt.Sprint(want) {
		t.Errorf("positions delivered = %v, want %v, each once", positions, want)
	}
	if fmt.Sprint(ex.watches) != "[0 2]" {
		t.Errorf("the executor was watched from %v, want from 0, then from 2 after the drop", ex.watches)
	}
	if len(warnings) != 2 || !strings.Contains(warnings[0], "dropped") || !strings.Contains(warnings[1], "back") {
		t.Errorf("warnings = %q, want one of the drop and one of the return", warnings)
	}
}

// TestWatchRestart checks that a watch that reconnects to another instance of the coordinator, or to one that
// doesn't know the job, reports that the coordinator restarted.
func TestWatchRestart(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after func(ex *fakeExecutor)
	}{
		{name: "another instance", after: func(ex *fakeExecutor) { ex.caps.Instance = "i-2" }},
		{name: "unknown job", after: func(ex *fakeExecutor) { delete(ex.jobs, "job-1") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := newFakeExecutor(6)
			f := &faults{cuts: []int{2}}
			c, _ := fixture(t, ex, f)
			if _, err := c.Capabilities(t.Context()); err != nil {
				t.Fatal(err)
			}
			delivered := 0
			err := c.Watch(t.Context(), "job-1", 0, func(e api.Event) error {
				delivered++
				if delivered == 2 {
					ex.mu.Lock()
					tc.after(ex)
					ex.mu.Unlock()
				}
				return nil
			})
			if !errors.Is(err, errRestarted) {
				t.Errorf("Watch = %v, want %v", err, errRestarted)
			}
		})
	}
}

// TestWatchGivesUp checks that a watch whose connection stays down gives up once the grace period has passed,
// saying that the build may have been withdrawn, and that one cancelled meanwhile stops at once.
func TestWatchGivesUp(t *testing.T) {
	ex := newFakeExecutor(6)
	ex.caps.GracePeriod = 300 * time.Millisecond
	f := &faults{cuts: []int{2}}
	c, _ := fixture(t, ex, f)
	if _, err := c.Capabilities(t.Context()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := c.Watch(t.Context(), "job-1", 0, func(e api.Event) error {
		if e.Position == 1 {
			f.mu.Lock()
			f.down = true
			f.mu.Unlock()
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "lost for") || !strings.Contains(err.Error(), "withdrawn") {
		t.Errorf("Watch = %v, want it given up, saying the build may have been withdrawn", err)
	}
	if took := time.Since(start); took < 300*time.Millisecond || took > 5*time.Second {
		t.Errorf("Watch gave up after %s, want after the grace period of 300ms and soon after", took)
	}

	f.mu.Lock()
	f.down = false
	f.cuts = []int{2}
	f.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	err = c.Watch(ctx, "job-1", 0, func(e api.Event) error {
		if e.Position == 1 {
			f.mu.Lock()
			f.down = true
			f.mu.Unlock()
			go func() {
				time.Sleep(50 * time.Millisecond)
				cancel()
			}()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Watch cancelled during a drop = %v, want %v", err, context.Canceled)
	}
}

// TestNewClientRefusesBadURLs checks the message for a coordinator that isn't an HTTP URL.
func TestNewClientRefusesBadURLs(t *testing.T) {
	for _, bad := range []string{"", "coordinator:8080", "ftp://x", "http://"} {
		if _, err := NewClient(bad); err == nil || !strings.Contains(err.Error(), "coordinator") {
			t.Errorf("NewClient(%q) = %v, want it refused", bad, err)
		}
	}
	if c, err := NewClient("http://coordinator:8080/"); err != nil || c.base != "http://coordinator:8080" {
		t.Errorf("NewClient = %+v, %v; want the trailing slash dropped", c, err)
	}
}

// TestUnreachableFailsFast checks that a coordinator that has never answered isn't retried: the first call fails
// at once, saying the coordinator can't be reached, rather than after a minute of retries with the wrong message.
func TestUnreachableFailsFast(t *testing.T) {
	c, err := NewClient("http://127.0.0.1:9") // discard, which nothing listens on here
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = c.Capabilities(t.Context())
	if err == nil || !strings.HasPrefix(err.Error(), "can't reach the coordinator at http://127.0.0.1:9: ") {
		t.Errorf("Capabilities of an unreachable coordinator = %v, want it reported as unreachable", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the failure took %s, want it at once", took)
	}
	if _, err := c.Submit(t.Context(), api.Request{Graph: &api.GraphRequest{}}); err == nil ||
		!strings.Contains(err.Error(), "can't reach the coordinator") {
		t.Errorf("Submit to an unreachable coordinator = %v, want it reported as unreachable", err)
	}
}

// TestTimeoutIsTransport checks that a call that ran out of time waiting for an answer is a transport failure,
// as a worker tells one from a refusal: it doesn't know what the coordinator thinks, so it keeps the attempt until
// its fence, rather than killing it at once as it does on a refusal.
func TestTimeoutIsTransport(t *testing.T) {
	hang := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathCapabilities {
			writeJSON(w, http.StatusOK, api.Capabilities{})
			return
		}
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(hang)
	c, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Capabilities(t.Context()); err != nil {
		t.Fatal(err)
	}
	w := &WorkerClient{c: c}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = w.Heartbeat(ctx, "attempt", Heartbeat{})
	if !IsTransport(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a heartbeat that timed out = %v; want a transport failure that is the deadline", err)
	}
	cancelled, cancelNow := context.WithCancel(t.Context())
	cancelNow()
	if _, err := w.Heartbeat(cancelled, "attempt", Heartbeat{}); IsTransport(err) || !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled heartbeat = %v; want the cancellation, not a transport failure", err)
	}
}

// blockingExecutor is a fake executor whose submissions block until released, so that a test can send a repeat
// of a submission while the first is still being served.
type blockingExecutor struct {
	*fakeExecutor
	release chan struct{}
}

// Submit waits to be released, then submits through the fake.
func (b *blockingExecutor) Submit(ctx context.Context, req api.Request) (api.JobID, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return b.fakeExecutor.Submit(ctx, req)
}

// TestSubmissionRepeatedWhileServed checks that a submission repeated while its first request is still being served
// starts one job, which both requests name, and that a refused submission is forgotten, so that a repeat is tried.
func TestSubmissionRepeatedWhileServed(t *testing.T) {
	ex := &blockingExecutor{fakeExecutor: newFakeExecutor(1), release: make(chan struct{})}
	m := cache.NewMemory()
	server := httptest.NewServer((&Server{Executor: ex, Content: m}).Handler())
	t.Cleanup(server.Close)
	body, err := json.Marshal(api.Request{Graph: &api.GraphRequest{}})
	if err != nil {
		t.Fatal(err)
	}
	post := func(submission string, body []byte) (int, jobHandle) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+pathJobs, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(headerSubmission, submission)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var handle jobHandle
		json.NewDecoder(resp.Body).Decode(&handle)
		return resp.StatusCode, handle
	}
	type answer struct {
		status int
		handle jobHandle
	}
	answers := make(chan answer, 2)
	for range 2 {
		go func() {
			status, handle := post("same", body)
			answers <- answer{status, handle}
		}()
	}
	time.Sleep(100 * time.Millisecond) // both requests are being served, the second waiting for the first
	close(ex.release)
	first, second := <-answers, <-answers
	if first.handle.ID != second.handle.ID || first.handle.ID == "" {
		t.Errorf("the repeated submission got jobs %q and %q, want one job", first.handle.ID, second.handle.ID)
	}
	if ex.submits != 1 {
		t.Errorf("the executor accepted %d submissions, want 1", ex.submits)
	}
	if (first.status == http.StatusCreated) == (second.status == http.StatusCreated) {
		t.Errorf("the answers were %d and %d, want one 201 and one 200", first.status, second.status)
	}

	// A refused submission isn't remembered: the repeat is tried again, and refused again.
	refused, err := json.Marshal(api.Request{})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if status, _ := post("refused", refused); status != http.StatusInternalServerError {
			t.Errorf("a refused submission answered %d, want 500 each time", status)
		}
	}
}

// slowExecutor is a fake executor whose watches pause before each event after the first, for a while,
// so that a test sees whether keep-alive lines hold a quiet stream open.
type slowExecutor struct {
	*fakeExecutor
	pause time.Duration
}

// Watch delivers events through the fake, pausing before each one after the first.
func (s *slowExecutor) Watch(ctx context.Context, id api.JobID, from int64, fn func(api.Event) error) error {
	first := true
	return s.fakeExecutor.Watch(ctx, id, from, func(e api.Event) error {
		if !first {
			select {
			case <-time.After(s.pause):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		first = false
		return fn(e)
	})
}

// TestWatchKeepAlive checks that a quiet stream stays open while the coordinator sends keep-alive lines,
// and that one that sends nothing for the stream timeout is taken for dropped and resumed.
func TestWatchKeepAlive(t *testing.T) {
	ex := &slowExecutor{fakeExecutor: newFakeExecutor(3), pause: 300 * time.Millisecond}
	c, _ := fixture(t, ex, nil) // keep-alive every 50ms
	c.StreamTimeout = 150 * time.Millisecond
	var warnings []string
	c.Warn = func(msg string) { warnings = append(warnings, msg) }
	delivered := 0
	if err := c.Watch(t.Context(), "job-1", 0, func(api.Event) error { delivered++; return nil }); err != nil {
		t.Fatal(err)
	}
	if delivered != 3 || len(warnings) != 0 {
		t.Errorf("a quiet stream delivered %d events with warnings %q; want 3 without a drop", delivered, warnings)
	}

	// Without keep-alive lines, the quiet stream is taken for dropped, and resumed from the last event.
	m := cache.NewMemory()
	server := httptest.NewServer((&Server{Executor: ex, Content: m, KeepAlive: time.Hour}).Handler())
	t.Cleanup(server.Close)
	quiet, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	quiet.StreamTimeout = 150 * time.Millisecond
	quiet.Warn = func(msg string) { warnings = append(warnings, msg) }
	if _, err := quiet.Capabilities(t.Context()); err != nil {
		t.Fatal(err)
	}
	ex.mu.Lock()
	ex.watches = nil
	ex.mu.Unlock()
	delivered = 0
	if err := quiet.Watch(t.Context(), "job-1", 0, func(api.Event) error { delivered++; return nil }); err != nil {
		t.Fatal(err)
	}
	if delivered != 3 || len(warnings) < 2 || !strings.Contains(warnings[0], "nothing arrived") {
		t.Errorf("a silent stream delivered %d events with warnings %q; want 3, resumed after the silence", delivered,
			warnings)
	}
	if len(ex.watches) < 2 || ex.watches[1] != 1 {
		t.Errorf("the executor was watched from %v, want a resume from 1 after the first event", ex.watches)
	}
}

// gapExecutor is a fake executor that retains only the events from first on: a watch from before meets a gap.
type gapExecutor struct {
	*fakeExecutor
	first int64
	asked []int64 // the positions watches asked for, before the gap moved them
}

// Watch delivers a gap for a position before the first retained, then the rest.
func (g *gapExecutor) Watch(ctx context.Context, id api.JobID, from int64, fn func(api.Event) error) error {
	g.mu.Lock()
	g.asked = append(g.asked, from)
	g.mu.Unlock()
	if from < g.first {
		if err := fn(api.Event{Position: from, Kind: api.EventGap, First: g.first}); err != nil {
			return err
		}
		from = g.first
	}
	return g.fakeExecutor.Watch(ctx, id, from, fn)
}

// TestWatchResumesAfterGap checks that a watch cut right after a gap resumes from the first retained position,
// so that the gap isn't delivered again.
func TestWatchResumesAfterGap(t *testing.T) {
	ex := &gapExecutor{fakeExecutor: newFakeExecutor(6), first: 3}
	f := &faults{cuts: []int{1}} // the gap, then the cut
	c, _ := fixture(t, ex, f)
	gaps := 0
	var positions []int64
	err := c.Watch(t.Context(), "job-1", 0, func(e api.Event) error {
		if e.Kind == api.EventGap {
			gaps++
		} else {
			positions = append(positions, e.Position)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if gaps != 1 || fmt.Sprint(positions) != "[3 4 5]" {
		t.Errorf("delivered %d gaps and positions %v, want one gap, then 3 to 5", gaps, positions)
	}
	if want := "[0 3]"; fmt.Sprint(ex.asked) != want {
		t.Errorf("the executor was watched from %v, want %s: the resume starts at the first retained", ex.asked, want)
	}
}

// cuttingBody is a response writer that drops the connection once a blob's body has grown past a size.
type cuttingBody struct {
	http.ResponseWriter
	after   int
	written int
}

// Write writes, and cuts the connection once enough has been sent: what was written reaches the client first.
func (c *cuttingBody) Write(p []byte) (int, error) {
	if c.written+len(p) > c.after {
		n, err := c.ResponseWriter.Write(p[:c.after-c.written])
		c.written += n
		if err == nil {
			c.Flush()
			panic(http.ErrAbortHandler)
		}
		return n, err
	}
	n, err := c.ResponseWriter.Write(p)
	c.written += n
	return n, err
}

// Flush flushes.
func (c *cuttingBody) Flush() {
	c.ResponseWriter.(http.Flusher).Flush()
}

// TestGetResumes checks that a blob whose connection drops part way is read to its end from where it was cut,
// verified, and that one the coordinator finds corrupt as it sends it is reported corrupt rather than retried.
func TestGetResumes(t *testing.T) {
	ex := newFakeExecutor(1)
	m := cache.NewMemory()
	var cuts, ranges int
	var mu sync.Mutex
	s := &Server{Executor: ex, Content: m, Middleware: func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, pathBlobs+"/") {
				mu.Lock()
				if r.Header.Get("Range") != "" {
					ranges++
				}
				cut := cuts == 0
				cuts++
				mu.Unlock()
				if cut {
					w = &cuttingBody{ResponseWriter: w, after: 1000}
				}
			}
			h.ServeHTTP(w, r)
		})
	}}
	server := httptest.NewServer(s.Handler())
	t.Cleanup(server.Close)
	c, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Capabilities(t.Context()); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("0123456789"), 10000)
	b, err := m.PutBytes(t.Context(), data)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Read(t.Context(), b, b.Size)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Read across a drop = %d bytes, %v; want the blob", len(got), err)
	}
	if ranges != 1 {
		t.Errorf("%d range requests were made, want 1, from where the body was cut", ranges)
	}

	// A blob corrupt in the store, found so as it is sent, is reported, not retried.
	m.Replace(b, append(bytes.Clone(data), '!'))
	start := time.Now()
	_, err = c.Read(t.Context(), b, b.Size)
	if !errors.Is(err, cache.ErrCorrupt) {
		t.Errorf("Read of a blob the coordinator finds corrupt = %v, want %v", err, cache.ErrCorrupt)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the corrupt read took %s, want no retries", took)
	}
}

// TestSubmitAfterRestart checks that a submission whose acknowledgement is lost, and whose coordinator restarted
// before the retry, isn't started again by the new instance, which can't know whether the old one accepted it:
// the submission fails saying the coordinator restarted, and a push says that whether it pushed is unknown.
func TestSubmitAfterRestart(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  api.Request
		want string
	}{
		{
			name: "build",
			req:  api.Request{Graph: &api.GraphRequest{}},
			want: "the coordinator restarted before it acknowledged the submission",
		},
		{
			name: "push",
			req:  api.Request{Push: &api.PushRequest{}},
			want: "whether it pushed is unknown; check the destination",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := newFakeExecutor(1)
			f := &faults{dropSubmits: 1, dropped: func() {
				ex.mu.Lock()
				ex.caps.Instance = "i-2"
				ex.mu.Unlock()
			}}
			c, _ := fixture(t, ex, f)
			if _, err := c.Submit(t.Context(), tc.req); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Submit across a restart = %v, want it to say %q", err, tc.want)
			}
			if ex.submits != 1 {
				t.Errorf("the executor accepted %d submissions, want 1: the retry is refused", ex.submits)
			}
		})
	}
	// A client that hasn't retried names no instance: its first submission is accepted by whichever answers.
	ex := newFakeExecutor(1)
	c, _ := fixture(t, ex, nil)
	ex.caps.Instance = "i-2"
	if _, err := c.Submit(t.Context(), api.Request{Graph: &api.GraphRequest{}}); err != nil {
		t.Errorf("a first submission to a restarted coordinator = %v, want it accepted", err)
	}
}

// TestSubmissionsKeptForRetries checks that a server remembers a submission for as long as its client may retry it,
// however many come after, and one whose job is still being made for as long as that takes.
func TestSubmissionsKeptForRetries(t *testing.T) {
	s := &Server{}
	const keep = 200 * time.Millisecond
	first, _ := s.claim("first", keep)
	s.finish(first)
	making, _ := s.claim("making", keep)
	for i := range 5000 {
		sub, _ := s.claim(fmt.Sprint(i), keep)
		s.finish(sub)
	}
	if _, made := s.claim("first", keep); made {
		t.Error("a submission was forgotten for the many after it, within the time its client may retry it")
	}
	time.Sleep(2 * keep)
	if _, made := s.claim("making", keep); made {
		t.Error("a submission whose job is being made was forgotten")
	}
	if _, made := s.claim("first", keep); !made {
		t.Error("a submission was remembered after its client stopped retrying it")
	}
	if n := len(s.submissions); n != 2 {
		t.Errorf("the server remembers %d submissions, want 2: the one being made and the one just claimed", n)
	}
	s.finish(making)
}

// TestWatchSecondOutage checks that a watch that recovered from a drop has the whole grace period for the next:
// the streaming in between doesn't count against it.
func TestWatchSecondOutage(t *testing.T) {
	ex := &slowExecutor{fakeExecutor: newFakeExecutor(12), pause: 100 * time.Millisecond}
	ex.caps.GracePeriod = 400 * time.Millisecond
	// The first stream drops at once; the second streams for longer than the grace period, keep-alives every 50ms
	// among its events, then drops too.
	f := &faults{cuts: []int{1, 14}}
	c, _ := fixture(t, ex, f)
	var warnings []string
	c.Warn = func(msg string) { warnings = append(warnings, msg) }
	var positions []int64
	err := c.Watch(t.Context(), "job-1", 0, func(e api.Event) error {
		positions = append(positions, e.Position)
		return nil
	})
	if err != nil {
		t.Fatalf("Watch across two drops = %v, want it to recover from both", err)
	}
	if len(positions) != 12 || positions[11] != 11 {
		t.Errorf("positions delivered = %v, want 0 to 11, each once", positions)
	}
	if len(ex.watches) != 3 {
		t.Errorf("the executor was watched from %v, want three streams", ex.watches)
	}
	if len(warnings) != 4 {
		t.Errorf("warnings = %q, want a drop and a return for each outage", warnings)
	}
}

// TestWatchSlowCallback checks that a watcher that takes longer than the stream timeout over an event doesn't make
// the watch take a healthy stream for silent.
func TestWatchSlowCallback(t *testing.T) {
	ex := newFakeExecutor(3)
	c, _ := fixture(t, ex, nil)
	c.StreamTimeout = 100 * time.Millisecond
	var warnings []string
	c.Warn = func(msg string) { warnings = append(warnings, msg) }
	delivered := 0
	err := c.Watch(t.Context(), "job-1", 0, func(api.Event) error {
		delivered++
		if delivered == 1 {
			time.Sleep(300 * time.Millisecond)
		}
		return nil
	})
	if err != nil || delivered != 3 {
		t.Errorf("Watch with a slow watcher = %v after %d events, want all 3", err, delivered)
	}
	if len(ex.watches) != 1 || len(warnings) != 0 {
		t.Errorf("the executor was watched from %v, with warnings %q; want one stream, without a drop", ex.watches,
			warnings)
	}
}

// TestGetGivesUp checks that a blob whose body drops every time, after an answer that starts well, is retried with
// backoff, and given up once the grace period has passed, rather than asked for again at once without end.
func TestGetGivesUp(t *testing.T) {
	ex := newFakeExecutor(1)
	ex.caps.GracePeriod = time.Millisecond
	m := cache.NewMemory()
	var gets int
	var mu sync.Mutex
	s := &Server{Executor: ex, Content: m, Middleware: func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, pathBlobs+"/") {
				mu.Lock()
				gets++
				mu.Unlock()
				w = &cuttingBody{ResponseWriter: w, after: 0}
			}
			h.ServeHTTP(w, r)
		})
	}}
	server := httptest.NewServer(s.Handler())
	t.Cleanup(server.Close)
	c, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Capabilities(t.Context()); err != nil {
		t.Fatal(err)
	}
	b, err := m.PutBytes(t.Context(), bytes.Repeat([]byte("x"), 10000))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err = c.Read(ctx, b, b.Size)
	if err == nil || !strings.Contains(err.Error(), "lost for") {
		t.Errorf("Read of a blob whose body always drops = %v, want it given up after the grace period", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gets > 3 {
		t.Errorf("the blob was asked for %d times, want a few at most within a grace period of 1ms", gets)
	}
}

// TestReadManyBatches checks that ReadMany asks for at most maxReadMany blobs a request, in order, so that the
// contents come back in the order asked, and that a batch after the first that fails fails the read.
func TestReadManyBatches(t *testing.T) {
	ex := newFakeExecutor(1)
	m := cache.NewMemory()
	var requests []int
	var mu sync.Mutex
	s := &Server{Executor: ex, Content: m, Middleware: func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == pathReadMany {
				var blobs []cache.Blob
				data, _ := io.ReadAll(r.Body)
				json.Unmarshal(data, &blobs)
				r.Body = io.NopCloser(bytes.NewReader(data))
				mu.Lock()
				requests = append(requests, len(blobs))
				mu.Unlock()
			}
			h.ServeHTTP(w, r)
		})
	}}
	server := httptest.NewServer(s.Handler())
	t.Cleanup(server.Close)
	c, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	var blobs []cache.Blob
	for i := range maxReadMany + 1 {
		b, err := m.PutBytes(ctx, []byte(fmt.Sprint(i)))
		if err != nil {
			t.Fatal(err)
		}
		blobs = append(blobs, b)
	}
	for _, tc := range []struct {
		count int
		want  string
	}{
		{count: maxReadMany - 1, want: "[255]"},
		{count: maxReadMany, want: "[256]"},
		{count: maxReadMany + 1, want: "[256 1]"},
	} {
		requests = nil
		contents, err := c.ReadMany(ctx, blobs[:tc.count], 8)
		if err != nil || len(contents) != tc.count {
			t.Fatalf("ReadMany of %d blobs = %d contents, %v", tc.count, len(contents), err)
		}
		for i, data := range contents {
			if string(data) != fmt.Sprint(i) {
				t.Errorf("ReadMany of %d blobs gave %q at %d, want %q", tc.count, data, i, fmt.Sprint(i))
				break
			}
		}
		if fmt.Sprint(requests) != tc.want {
			t.Errorf("ReadMany of %d blobs made requests of %v blobs, want %s", tc.count, requests, tc.want)
		}
	}
	// The second batch asks for a blob never put: the read fails, after both requests.
	requests = nil
	never := cache.Blob{Digest: digest.FromString("never"), Size: 5}
	if _, err := c.ReadMany(ctx, append(slices.Clone(blobs[:maxReadMany]), never), 8); !errors.Is(err,
		cache.ErrMissing) {
		t.Errorf("ReadMany whose second batch lacks a blob = %v, want %v", err, cache.ErrMissing)
	}
	if fmt.Sprint(requests) != "[256 1]" {
		t.Errorf("ReadMany whose second batch fails made requests of %v blobs, want [256 1]", requests)
	}
}

// TestReleaseCrosses checks that Release reaches the executor, and that an unknown job's release is ErrNoJob.
func TestReleaseCrosses(t *testing.T) {
	ex := newFakeExecutor(3)
	server := httptest.NewServer((&Server{Executor: ex, Content: cache.NewMemory()}).Handler())
	defer server.Close()
	c, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Release(t.Context(), "job-1"); err != nil {
		t.Errorf("Release = %v, want it accepted", err)
	}
	if err := c.Release(t.Context(), "nope"); !errors.Is(err, api.ErrNoJob) {
		t.Errorf("Release of an unknown job = %v, want %v", err, api.ErrNoJob)
	}
}
