package coord

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/remote"
	"github.com/Topher-the-Geek/vamos/internal/sched"
)

// aPublication publishes two images, each to its own repository, under the tags v1 and latest.
var aPublication = api.PublishRequest{
	Images: []api.PublishImage{
		{Result: api.Result{Node: 1, Image: strings.Repeat("a", 64)}, Repository: "example.com/team/app"},
		{Result: api.Result{Node: 2, Image: strings.Repeat("b", 64)}, Repository: "example.com/team/worker"},
	},
	Tags: []string{"v1", "latest"},
}

// publishJob submits req to c as a publication, watches it to its end and returns its outcome.
func publishJob(t *testing.T, c *Coordinator, req api.PublishRequest) api.Outcome {
	t.Helper()
	ctx := t.Context()
	id, err := c.Submit(ctx, api.Request{Publish: &req})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Watch(ctx, id, 0, func(api.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	outcome, err := c.Result(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

// failure is an attempt's failure, saying msg.
func failure(msg string) *sched.AttemptFailure {
	return &sched.AttemptFailure{Message: msg}
}

// uploaded is what a fake worker reports uploading to repo: a manifest naming it, and an image ID.
func uploaded(repo string) *remote.Uploaded {
	return &remote.Uploaded{Manifest: []byte("manifest of " + repo), Image: "id of " + repo}
}

// tagged is the outcome a fake worker reports of tagging work: every destination written.
func tagged(work *remote.TagWork) *api.Outcome {
	published := &api.Published{}
	for i, image := range work.Images {
		for _, tag := range work.Tags {
			published.Destinations = append(published.Destinations, api.Destination{
				Repository: image.Repository, Tag: tag, Image: work.Uploads[i].Image, Written: true,
			})
		}
	}
	return &api.Outcome{Published: published}
}

// TestPublishUploadsAtOnce checks that a publication's uploads run as attempts at the same time, on any workers,
// and that once both have succeeded one attempt writes every tag, given what each upload wrote, in order,
// whose outcome is the publication's.
func TestPublishUploadsAtOnce(t *testing.T) {
	f := newFixture(t, Config{})
	var mu sync.Mutex
	var tagWork []*remote.TagWork
	both := make(chan struct{})
	var arrived sync.WaitGroup
	arrived.Add(2)
	go func() {
		arrived.Wait()
		close(both)
	}()
	handle := func(a *remote.Assignment, killed func() bool) remote.AttemptReport {
		switch a.Push.Mode {
		case remote.ModeUpload:
			arrived.Done()
			select {
			case <-both: // the other upload is running too
			case <-time.After(5 * time.Second):
				return remote.AttemptReport{Failure: failure("the other upload never started")}
			}
			return remote.AttemptReport{Uploaded: uploaded(a.Push.Repository)}
		case remote.ModeTag:
			mu.Lock()
			tagWork = append(tagWork, a.Push.Tag)
			mu.Unlock()
			return remote.AttemptReport{Tagged: tagged(a.Push.Tag)}
		}
		return remote.AttemptReport{Failure: failure("unexpected work")}
	}
	startWorker(t, f, amd64, 1, handle)
	startWorker(t, f, arm64, 1, handle) // a push copies between registries, which a worker of any platform can

	outcome := publishJob(t, f.c, aPublication)
	if outcome.Failure != nil || outcome.Published == nil || len(outcome.Published.Destinations) != 4 {
		t.Fatalf("outcome = %+v, want every destination published", outcome)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tagWork) != 1 {
		t.Fatalf("%d tag attempts, want 1", len(tagWork))
	}
	w := tagWork[0]
	if len(w.Uploads) != 2 || string(w.Uploads[0].Manifest) != "manifest of example.com/team/app" ||
		string(w.Uploads[1].Manifest) != "manifest of example.com/team/worker" || len(w.Tags) != 2 {
		t.Errorf("the tag attempt was given %+v, want each upload's manifest, in order, and both tags", w)
	}
}

// TestPublishUploadsWithinBound checks that a coordinator runs no more of a publication's uploads at once than
// Bound lets a build dispatch targets, however many workers are free, and one at a time if Bound is less than 1.
func TestPublishUploadsWithinBound(t *testing.T) {
	for _, bound := range []int{1, -1} {
		t.Run(fmt.Sprint(bound), func(t *testing.T) { testPublishUploadsWithin(t, bound) })
	}
}

// testPublishUploadsWithin checks that a coordinator of Bound bound runs one of a publication's uploads at a time.
func testPublishUploadsWithin(t *testing.T, bound int) {
	f := newFixture(t, Config{Bound: bound})
	var mu sync.Mutex
	running, most := 0, 0
	startWorker(t, f, amd64, 2, func(a *remote.Assignment, killed func() bool) remote.AttemptReport {
		if a.Push.Mode == remote.ModeTag {
			return remote.AttemptReport{Tagged: tagged(a.Push.Tag)}
		}
		mu.Lock()
		running++
		most = max(most, running)
		mu.Unlock()
		time.Sleep(100 * time.Millisecond) // long enough for the other upload to start, were it let
		mu.Lock()
		running--
		mu.Unlock()
		return remote.AttemptReport{Uploaded: uploaded(a.Push.Repository)}
	})
	outcome := publishJob(t, f.c, aPublication)
	if outcome.Failure != nil {
		t.Fatalf("outcome = %+v, want every destination published", outcome.Failure)
	}
	mu.Lock()
	defer mu.Unlock()
	if most != 1 {
		t.Errorf("%d uploads ran at once, want 1, as Bound %d says", most, bound)
	}
}

// TestPublishFailedUploadWritesNoTag checks that a publication one of whose uploads fails fails, naming it,
// and makes no tag attempt.
func TestPublishFailedUploadWritesNoTag(t *testing.T) {
	f := newFixture(t, Config{})
	var mu sync.Mutex
	tagAttempts := 0
	startWorker(t, f, amd64, 2, func(a *remote.Assignment, killed func() bool) remote.AttemptReport {
		switch {
		case a.Push.Mode == remote.ModeTag:
			mu.Lock()
			tagAttempts++
			mu.Unlock()
			return remote.AttemptReport{Tagged: tagged(a.Push.Tag)}
		case a.Push.Repository == "example.com/team/worker":
			return remote.AttemptReport{Failure: failure("uploading to example.com/team/worker: denied")}
		}
		return remote.AttemptReport{Uploaded: uploaded(a.Push.Repository)}
	})
	outcome := publishJob(t, f.c, aPublication)
	if outcome.Failure == nil || !strings.Contains(outcome.Failure.Message, "example.com/team/worker: denied") ||
		len(outcome.Failure.Nodes) != 1 || outcome.Failure.Nodes[0] != 2 || outcome.Published != nil {
		t.Errorf("outcome = %+v, want a failure of node 2 naming the upload, and nothing published", outcome)
	}
	mu.Lock()
	defer mu.Unlock()
	if tagAttempts != 0 {
		t.Errorf("%d tag attempts after a failed upload, want none", tagAttempts)
	}
}

// TestPublishRetriesLostUpload checks that an upload whose worker is lost is tried again on another worker,
// since an upload by digest changes nothing a user sees, and that the publication then succeeds.
func TestPublishRetriesLostUpload(t *testing.T) {
	f := newFixture(t, Config{Retries: 1})
	release := make(chan struct{})
	dead := newWorker(t, f, amd64, 1, func(a *remote.Assignment, killed func() bool) remote.AttemptReport {
		<-release // the upload hangs until the test ends, long after its lease
		return remote.AttemptReport{Uploaded: uploaded(a.Push.Repository)}
	})
	dead.silent = true
	dead.maxTake = 1
	dead.start()
	t.Cleanup(func() { close(release) }) // before the worker's cleanup, which waits for its attempt
	done := make(chan api.Outcome, 1)
	go func() { done <- publishJob(t, f.c, aPublication) }()
	waitFor(t, func() bool { return dead.takenCount() >= 1 })
	live := startWorker(t, f, amd64, 2, func(a *remote.Assignment, killed func() bool) remote.AttemptReport {
		if a.Push.Mode == remote.ModeTag {
			return remote.AttemptReport{Tagged: tagged(a.Push.Tag)}
		}
		return remote.AttemptReport{Uploaded: uploaded(a.Push.Repository)}
	})
	var outcome api.Outcome
	select {
	case outcome = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the publication didn't end")
	}
	if outcome.Failure != nil {
		t.Fatalf("outcome = %+v, want success after the upload was tried again", outcome.Failure)
	}
	if n := live.takenCount(); n != 3 {
		t.Errorf("the live worker took %d attempts, want 3: both uploads, one again, and the tags", n)
	}
}

// TestPublishLostWhileTagging checks that a publication whose worker is lost while it writes the tags fails,
// saying whether its tags were written is unknown, rather than tagging again.
func TestPublishLostWhileTagging(t *testing.T) {
	f := newFixture(t, Config{Retries: 2})
	release := make(chan struct{})
	var mu sync.Mutex
	tagAttempts := 0
	w := newWorker(t, f, amd64, 2, func(a *remote.Assignment, killed func() bool) remote.AttemptReport {
		if a.Push.Mode == remote.ModeUpload {
			return remote.AttemptReport{Uploaded: uploaded(a.Push.Repository)}
		}
		mu.Lock()
		tagAttempts++
		mu.Unlock()
		<-release // the tagging hangs until the test ends, long after its lease
		return remote.AttemptReport{Tagged: tagged(a.Push.Tag)}
	})
	// It heartbeats for the uploads, then stops, as if it died while tagging.
	w.silentOn = func(a *remote.Assignment) bool { return a.Push.Mode == remote.ModeTag }
	w.start()
	t.Cleanup(func() { close(release) }) // before the worker's cleanup, which waits for its attempt
	outcome := publishJob(t, f.c, aPublication)
	if outcome.Failure == nil ||
		!strings.Contains(outcome.Failure.Message, "unknown whether the publication's tags were written") {
		t.Errorf("outcome = %+v, want a failure saying the tags may have been written", outcome)
	}
	mu.Lock()
	defer mu.Unlock()
	if tagAttempts != 1 {
		t.Errorf("%d tag attempts, want 1: tagging is never tried again", tagAttempts)
	}
}

// TestPublishNeedsAPublishingWorker checks that a publication's uploads and tagging go only to a worker that says it
// makes them, so that one from before publications waits rather than misreading them, while any worker pushes.
func TestPublishNeedsAPublishingWorker(t *testing.T) {
	f := newFixture(t, Config{})
	old := &worker{id: "old", info: remote.WorkerInfo{Platform: amd64, Capacity: 1}}
	current := &worker{id: "current", info: remote.WorkerInfo{Platform: amd64, Capacity: 1, Publishes: true}}
	for _, tc := range []struct {
		mode    remote.PushMode
		old     bool
		current bool
	}{
		{
			mode:    remote.ModePush,
			old:     true,
			current: true,
		},
		{
			mode:    remote.ModeUpload,
			current: true,
		},
		{
			mode:    remote.ModeTag,
			current: true,
		},
	} {
		at := &attempt{push: &remote.PushWork{Mode: tc.mode}}
		f.c.pool.mu.Lock()
		gotOld, gotCurrent := f.c.pool.suits(old, at), f.c.pool.suits(current, at)
		f.c.pool.mu.Unlock()
		if gotOld != tc.old || gotCurrent != tc.current {
			t.Errorf("mode %q suits the old worker %t and the current one %t, want %t and %t",
				tc.mode, gotOld, gotCurrent, tc.old, tc.current)
		}
	}
}

// TestPublishSaysItWaits checks that a publication whose uploads wait for a worker that publishes says so, as each
// image's output, rather than waiting silently, and then runs once one registers.
func TestPublishSaysItWaits(t *testing.T) {
	f := newFixture(t, Config{})
	ctx := t.Context()
	id, err := f.c.Submit(ctx, api.Request{Publish: &aPublication})
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	var once sync.Once
	watched := make(chan error, 1)
	go func() {
		watched <- f.c.Watch(ctx, id, 0, func(e api.Event) error {
			if e.Kind == api.EventOutput && slices.Equal(e.Nodes, []graph.Ref{1}) &&
				strings.Contains(string(e.Data), "waiting for a worker that publishes") {
				once.Do(func() { close(waiting) })
			}
			return nil
		})
	}()
	select {
	case <-waiting:
	case err := <-watched:
		t.Fatalf("the publication ended without waiting for a worker: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the publication never said it waits for a worker that publishes")
	}
	startWorker(t, f, amd64, 2, func(a *remote.Assignment, killed func() bool) remote.AttemptReport {
		if a.Push.Mode == remote.ModeTag {
			return remote.AttemptReport{Tagged: tagged(a.Push.Tag)}
		}
		return remote.AttemptReport{Uploaded: uploaded(a.Push.Repository)}
	})
	if err := <-watched; err != nil {
		t.Fatal(err)
	}
	if outcome, err := f.c.Result(ctx, id); err != nil || outcome.Failure != nil {
		t.Fatalf("outcome = %+v, %v, want every destination published", outcome, err)
	}
}

// TestPublishTaggingTooLarge checks that a publication whose tagging a worker would refuse as too large fails before
// any tag is written, saying so, rather than losing the tag attempt.
func TestPublishTaggingTooLarge(t *testing.T) {
	f := newFixture(t, Config{})
	var mu sync.Mutex
	tagAttempts := 0
	startWorker(t, f, amd64, 2, func(a *remote.Assignment, killed func() bool) remote.AttemptReport {
		if a.Push.Mode == remote.ModeTag {
			mu.Lock()
			tagAttempts++
			mu.Unlock()
			return remote.AttemptReport{Tagged: tagged(a.Push.Tag)}
		}
		// Each manifest takes more than half of what a worker takes, once encoded.
		return remote.AttemptReport{Uploaded: &remote.Uploaded{Manifest: make([]byte, remote.MaxJSONBody/2),
			Image: "id"}}
	})
	outcome := publishJob(t, f.c, aPublication)
	if outcome.Failure == nil || !strings.Contains(outcome.Failure.Message, "so no tag is written") {
		t.Errorf("outcome = %+v, want a failure saying the tagging is too large", outcome)
	}
	mu.Lock()
	defer mu.Unlock()
	if tagAttempts != 0 {
		t.Errorf("%d tag attempts, want none", tagAttempts)
	}
}

// TestPublishRefusedWhenBusy checks that a publication is one job against MaxBuilds: refused, before any attempt
// starts, while the coordinator's one job is open, and admitted as one job, however many repositories and tags it
// has, once it isn't.
func TestPublishRefusedWhenBusy(t *testing.T) {
	f := newFixture(t, Config{MaxBuilds: 1})
	gate := make(chan struct{})
	pushing := make(chan struct{}, 1)
	var mu sync.Mutex
	publishing := 0
	startWorker(t, f, amd64, 4, func(a *remote.Assignment, killed func() bool) remote.AttemptReport {
		switch a.Push.Mode {
		case remote.ModePush:
			pushing <- struct{}{}
			<-gate
			return remote.AttemptReport{Pushed: &api.Pushed{Image: a.Push.Result.Image}}
		case remote.ModeUpload:
			mu.Lock()
			publishing++
			mu.Unlock()
			return remote.AttemptReport{Uploaded: uploaded(a.Push.Repository)}
		}
		mu.Lock()
		publishing++
		mu.Unlock()
		return remote.AttemptReport{Tagged: tagged(a.Push.Tag)}
	})
	ctx := t.Context()
	res := api.Result{Image: strings.Repeat("a", 64)}
	push, err := f.c.Submit(ctx, api.Request{Push: &api.PushRequest{Result: res, Destination: "example.com/app:v1"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-pushing:
	case <-time.After(10 * time.Second):
		close(gate)
		t.Fatal("the push never reached the worker")
	}
	req := aPublication
	if _, err := f.c.Submit(ctx, api.Request{Publish: &req}); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Errorf("a publication while the one job is open = %v, want it refused as busy", err)
	}
	mu.Lock()
	if publishing != 0 {
		t.Errorf("%d attempts of the refused publication started, want none", publishing)
	}
	mu.Unlock()
	close(gate)
	if err := f.c.Watch(ctx, push, 0, func(api.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if outcome := publishJob(t, f.c, aPublication); outcome.Failure != nil {
		t.Errorf("the publication once the push ended = %+v, want it admitted as one job and published", outcome)
	}
}
