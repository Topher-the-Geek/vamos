package apitest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/fileset"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/tracing"

	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// Fixture makes the executors that Contract tests, and puts content where they read it.
type Fixture struct {
	// New returns a fresh executor, on stores of its own, which t's cleanup closes.
	// It reaches registries over plain HTTP, since the test runs one that way.
	New func(t *testing.T) api.Executor
	// Put stores data where executors that New makes read content, returning its blob.
	Put func(t *testing.T, ex api.Executor, data []byte) cache.Blob
	// Missing returns which of blobs the content store that New's executors publish to lacks.
	Missing func(t *testing.T, ex api.Executor, blobs []cache.Blob) []cache.Blob
	// Platform is the execution platform that New's executors report.
	Platform imgspecv1.Platform
	// Registry serves a registry that New's executors can push to over plain HTTP.
	Registry http.Handler
	// Insecure makes the executor's environment reach the registry at host, "host:port", over plain HTTP,
	// as registries.conf marks it; the executor builds its registry settings from its environment alone.
	Insecure func(t *testing.T, host string)
	// Retention is how many events New's executors retain for each job; the retention tests need it small,
	// no more than 8, and the graphs they run make more events than that.
	Retention int
	// Buffered says that events may be held between the executor and a watcher's function, as a transport holds
	// them in its connection, so that a slow watcher isn't dropped as the contract describes until the buffers fill;
	// the retention test then proves only the gap from an expired position.
	Buffered bool
	// RequireBuilds skips t, or fails it, if the host can't run the builds that New's executors run,
	// such as one without buildah; it is nil if every host can.
	// Only the cases that run a graph, pull, push, export or prune call it: the rest need nothing of the host.
	RequireBuilds func(t *testing.T)
	// NoExport says that New's executors refuse export jobs, as a coordinator does, since every image they build
	// is at a registry that the result locates it at, for the client to deliver from; the export cases then check
	// the refusal and the location instead of an export.
	NoExport bool
}

// requireBuilds calls f.RequireBuilds, if it is set.
func (f Fixture) requireBuilds(t *testing.T) {
	t.Helper()
	if f.RequireBuilds != nil {
		f.RequireBuilds(t)
	}
}

// Contract checks that executors that f makes keep the contract of api.Executor:
// capabilities, a graph run watched to its terminal event and read back, refusals before any work,
// unknown jobs, cancellation and prune, that values cross by copy, and that a watch stops when its context ends.
func Contract(t *testing.T, f Fixture) {
	t.Run("capabilities", func(t *testing.T) { testCapabilities(t, f) })
	t.Run("graph job", func(t *testing.T) { testGraphJob(t, f) })
	t.Run("refused", func(t *testing.T) { testRefused(t, f) })
	t.Run("unknown job", func(t *testing.T) { testUnknownJob(t, f) })
	t.Run("cancel", func(t *testing.T) { testCancel(t, f) })
	t.Run("release", func(t *testing.T) { testRelease(t, f) })
	t.Run("failure", func(t *testing.T) { testFailure(t, f) })
	t.Run("retention", func(t *testing.T) { testRetention(t, f) })
	t.Run("export", func(t *testing.T) { testExport(t, f) })
	t.Run("push", func(t *testing.T) { testPush(t, f) })
	t.Run("prune", func(t *testing.T) { testPrune(t, f) })
	t.Run("values owned", func(t *testing.T) { testValuesOwned(t, f) })
	t.Run("delivery values owned", func(t *testing.T) { testDeliveryValuesOwned(t, f) })
	t.Run("watch cancelled", func(t *testing.T) { testWatchCancelled(t, f) })
}

// unreadableJob submits to ex a graph job, traced, whose graph ex doesn't have,
// which fails on any host, before building anything, and so needs nothing of the host,
// and returns its handle and the request, which the caller may change once Submit has returned.
func unreadableJob(t *testing.T, f Fixture, ex api.Executor, content string) (api.JobID, *api.GraphRequest) {
	t.Helper()
	req := &api.GraphRequest{
		Graph:    cache.Blob{Digest: digest.FromString(content), Size: int64(len(content))},
		Version:  graph.Version,
		Platform: f.Platform,
		Trace:    &tracing.CallContext{Parent: "client-1"}, // so that the job has spans as well as its outcome
	}
	id, err := ex.Submit(t.Context(), api.Request{Graph: req})
	if err != nil {
		t.Fatal(err)
	}
	return id, req
}

// testValuesOwned checks that values cross the executor boundary by copy, as a transport would carry them:
// changing a request once it is submitted changes nothing the job does, and changing what the executor returned,
// its capabilities, the events delivered or an outcome, changes nothing it returns later.
func testValuesOwned(t *testing.T, f Fixture) {
	ex := f.New(t)
	ctx := t.Context()

	c, err := ex.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range c.GraphVersions {
		c.GraphVersions[i] = -1
	}
	if again, err := ex.Capabilities(ctx); err != nil || !slices.Contains(again.GraphVersions, graph.Version) {
		t.Errorf("Capabilities after changing the versions returned = %+v, %v; want them unchanged", again, err)
	}

	id, req := unreadableJob(t, f, ex, "owned")
	submitted := req.Graph
	req.Graph = cache.Blob{Digest: digest.FromString("changed"), Size: 7}
	req.Trace.Parent = "changed"
	events := watchAll(t, ex, id, 0)
	last := events[len(events)-1]
	if last.Outcome == nil || last.Outcome.Failure == nil ||
		!strings.Contains(last.Outcome.Failure.Message, submitted.Digest.String()) {
		t.Fatalf("last event = %+v, want a failure naming the graph submitted, %s, not the one the request was "+
			"changed to after", last, submitted.Digest)
	}
	returnedOwned(t, ex, id)
}

// returnedOwned checks that changing what ex returns about the job id, which has ended, its events delivered
// or its outcome, however deep, changes nothing it returns later.
func returnedOwned(t *testing.T, ex api.Executor, id api.JobID) {
	t.Helper()
	ctx := t.Context()
	want, err := ex.Result(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	wantEvents := watchAll(t, ex, id, 0)

	scribble := func(e *api.Event) {
		e.Status = "changed"
		for i := range e.Nodes {
			e.Nodes[i] = -1
		}
		for i := range e.Data {
			e.Data[i] = '!'
		}
		for i := range e.Spans {
			e.Spans[i].Name = "changed"
			for k := range e.Spans[i].Args {
				e.Spans[i].Args[k] = "changed"
			}
		}
		if e.Outcome != nil {
			scribbleOutcome(e.Outcome)
		}
	}
	err = ex.Watch(ctx, id, 0, func(e api.Event) error {
		scribble(&e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ex.Result(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	scribbleOutcome(&got)
	if got, err := ex.Result(ctx, id); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Result after changing what was returned = %+v, %v; want %+v", got, err, want)
	}
	if got := watchAll(t, ex, id, 0); !reflect.DeepEqual(got, wantEvents) {
		t.Errorf("events after changing those delivered = %+v; want %+v", got, wantEvents)
	}
}

// testDeliveryValuesOwned checks that push and export requests cross the executor boundary by copy:
// changing one once it is submitted changes nothing the job does,
// and changing what a successful push or export returned changes nothing returned later.
func testDeliveryValuesOwned(t *testing.T, f Fixture) {
	f.requireBuilds(t)
	ex := f.New(t)
	ctx := t.Context()
	server := httptest.NewServer(f.Registry)
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	f.insecure(t, host)
	layer := runGraph(t, ex, layerGraph(t, f, ex, "owned delivery"))[0]
	other := strings.Repeat("0", 64) // an image ID that names nothing

	if !f.NoExport {
		testExportValuesOwned(t, f, ex, layer, other)
	}
	push := &api.PushRequest{Result: layer, Destination: host + "/owned:v1"}
	id, err := ex.Submit(ctx, api.Request{Push: push})
	if err != nil {
		t.Fatal(err)
	}
	push.Result.Image, push.Destination = other, host+"/changed:v1"
	watchAll(t, ex, id, 0)
	outcome, err := ex.Result(ctx, id)
	if err != nil || outcome.Failure != nil || outcome.Pushed == nil || outcome.Pushed.Image != layer.Image {
		t.Fatalf("a push whose request changed after Submit = %+v, %v; want image %s pushed", outcome, err,
			layer.Image)
	}
	for repo, want := range map[string]int{"owned": http.StatusOK, "changed": http.StatusNotFound} {
		resp, err := http.Head(server.URL + "/v2/" + repo + "/manifests/v1")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("the registry answers %d for %s:v1, want %d", resp.StatusCode, repo, want)
		}
	}
	returnedOwned(t, ex, id)
}

// scribbleOutcome changes everything that o holds, however deep.
func scribbleOutcome(o *api.Outcome) {
	o.Cancelled = !o.Cancelled
	for i := range o.Results {
		r := &o.Results[i]
		r.Image = "changed"
		for j := range r.Locations {
			r.Locations[j].Reference = "changed"
		}
		for j := range r.Index {
			r.Index[j].Image = "changed"
			for k := range r.Index[j].Locations {
				r.Index[j].Locations[k].Reference = "changed"
			}
		}
		if r.Files != nil {
			r.Files.Size = -1
		}
	}
	if o.Pushed != nil {
		o.Pushed.Image, o.Pushed.Index = "changed", "changed"
	}
	if e := o.Exported; e != nil {
		e.MediaType = "changed"
		for i := range e.Blobs {
			e.Blobs[i].Size = -1
		}
		if e.Files != nil {
			e.Files.Size = -1
		}
	}
	if o.Pruned != nil {
		o.Pruned.Blobs = -1
	}
	if f := o.Failure; f != nil {
		f.Message = "changed"
		for i := range f.Nodes {
			f.Nodes[i] = -1
		}
		if f.Step != nil {
			*f.Step = -1
		}
		if f.Log != nil {
			f.Log.Size = -1
		}
	}
}

// testWatchCancelled checks that a watch whose context ends stops before the next event,
// though the job has ended and every event is waiting to be delivered.
func testWatchCancelled(t *testing.T, f Fixture) {
	ex := f.New(t)
	id, _ := unreadableJob(t, f, ex, "cancelled watch")
	if events := watchAll(t, ex, id, 0); len(events) < 2 {
		t.Fatalf("the job has %d events, want at least 2 for the test to mean anything", len(events))
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	delivered := 0
	err := ex.Watch(ctx, id, 0, func(api.Event) error {
		delivered++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || delivered != 1 {
		t.Errorf("Watch whose context ended in its first delivery = %v after %d events; want %v after 1",
			err, delivered, context.Canceled)
	}
}

// insecure calls f.Insecure for host, failing the test if the fixture has none.
func (f Fixture) insecure(t *testing.T, host string) {
	t.Helper()
	if f.Insecure == nil {
		t.Fatal("the fixture can't make the executor reach a registry over plain HTTP; set Insecure")
	}
	f.Insecure(t, host)
}

// testCapabilities checks what an executor reports about itself.
func testCapabilities(t *testing.T, f Fixture) {
	ex := f.New(t)
	before := time.Now()
	c, err := ex.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !c.Offers(f.Platform) {
		t.Errorf("platforms = %v, want them to include %s", c.Platforms, platforms.Format(f.Platform))
	}
	if !slices.Contains(c.GraphVersions, graph.Version) {
		t.Errorf("graph versions = %v, want them to include %d", c.GraphVersions, graph.Version)
	}
	if c.Clock.Before(before.Add(-time.Minute)) || c.Clock.After(time.Now().Add(time.Minute)) {
		t.Errorf("clock = %s, want about now", c.Clock)
	}
}

// taskGraph stores a graph in ex, through f, whose root is a task on scratch that adds a file and keeps it,
// so that running it needs no registry or shell, and returns the graph's request.
func taskGraph(t *testing.T, f Fixture, ex api.Executor, content string) api.GraphRequest {
	t.Helper()
	file := f.Put(t, ex, []byte(content))
	tree := &cache.Tree{Entries: []cache.TreeEntry{{Path: "a.txt", Type: cache.EntryFile, Mode: 0o644, Blob: &file}}}
	stored := f.Put(t, ex, tree.Encode())
	from := graph.Ref(0)
	sel := fileset.DefaultSelection()
	sel.Include = []string{"."}
	g := &graph.Graph{
		Version:      graph.Version,
		ExecPlatform: platforms.Format(f.Platform),
		Nodes: []graph.Node{
			{Label: "src", Desc: `glob "src"`, Pos: "BUILD:1:1", Source: &graph.Source{Tree: stored}},
			{Label: "copy", Desc: `task "copy"`, Pos: "BUILD:2:1", Task: &graph.Task{
				Base:   graph.Scratch,
				Steps:  []graph.Step{{Op: graph.OpAdd, Display: `add("src", "/out")`, From: &from, Dest: "/out"}},
				Output: &graph.Output{Source: "/out", Selection: sel},
			}},
		},
		Roots: []graph.Ref{1},
	}
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	return api.GraphRequest{Graph: f.Put(t, ex, graph.Encode(g)), Version: graph.Version, Platform: f.Platform}
}

// watchAll watches the job id from position from until its terminal event, returning the events delivered.
func watchAll(t *testing.T, ex api.Executor, id api.JobID, from int64) []api.Event {
	t.Helper()
	var events []api.Event
	err := ex.Watch(t.Context(), id, from, func(e api.Event) error {
		events = append(events, e)
		return nil
	})
	if err != nil {
		t.Fatalf("watching job %s from %d: %v", id, from, err)
	}
	return events
}

// testGraphJob runs a graph and checks the events delivered, their positions, the outcome and what reads it back.
func testGraphJob(t *testing.T, f Fixture) {
	f.requireBuilds(t)
	ex := f.New(t)
	ctx := t.Context()
	req := taskGraph(t, f, ex, "hello")
	id, err := ex.Submit(ctx, api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	events := watchAll(t, ex, id, 0)
	if len(events) == 0 {
		t.Fatal("no events delivered")
	}
	var statuses []string
	for i, e := range events {
		if e.Position != int64(i) {
			t.Errorf("event %d has position %d", i, e.Position)
		}
		if e.Kind == api.EventStatus {
			statuses = append(statuses, fmt.Sprintf("%v: %s", e.Nodes, e.Status))
		}
		if e.Kind == api.EventDone && i != len(events)-1 {
			t.Errorf("event %d is the terminal event, but %d follow it", i, len(events)-1-i)
		}
	}
	if want := "[1]: building"; !slices.Contains(statuses, want) {
		t.Errorf("statuses = %q, want them to include %q", statuses, want)
	}
	last := events[len(events)-1]
	if last.Kind != api.EventDone || last.Outcome == nil {
		t.Fatalf("last event = %+v, want the terminal event with an outcome", last)
	}
	outcome := *last.Outcome
	if outcome.Failure != nil || outcome.Cancelled {
		t.Fatalf("outcome = %+v, want success", outcome)
	}
	if len(outcome.Results) != 1 || outcome.Results[0].Node != 1 || outcome.Results[0].Files == nil {
		t.Fatalf("results = %+v, want the task's files", outcome.Results)
	}

	got, err := ex.Result(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Results, outcome.Results) {
		t.Errorf("Result = %+v, want the terminal event's outcome, %+v", got, outcome)
	}
	// Watching again, from any position, delivers the same events from there.
	from := int64(len(events) / 2)
	again := watchAll(t, ex, id, from)
	if len(again) != len(events)-int(from) || again[0].Position != from {
		t.Errorf("watching from %d delivered positions %v, want %d..%d", from, positions(again), from,
			len(events)-1)
	}
	if past := watchAll(t, ex, id, int64(len(events))); len(past) != 0 {
		t.Errorf("watching from past the terminal event delivered %v", positions(past))
	}

	// A second run reuses the result.
	id, err = ex.Submit(ctx, api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	cached := false
	for _, e := range watchAll(t, ex, id, 0) {
		cached = cached || e.Kind == api.EventStatus && e.Status == "cached"
	}
	if !cached {
		t.Errorf("the second run didn't report the task cached")
	}
}

// runGraph submits req and watches the job to its end, returning its results, which must be a success.
func runGraph(t *testing.T, ex api.Executor, req api.GraphRequest) []api.Result {
	t.Helper()
	ctx := t.Context()
	id, err := ex.Submit(ctx, api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	watchAll(t, ex, id, 0)
	outcome, err := ex.Result(ctx, id)
	if err != nil || outcome.Failure != nil {
		t.Fatalf("Result = %+v, %v; want success", outcome, err)
	}
	return outcome.Results
}

// layerGraph stores a graph in ex, through f, whose root is a layer on scratch that adds a file,
// so that building it needs no registry or shell, and returns the graph's request.
func layerGraph(t *testing.T, f Fixture, ex api.Executor, content string) api.GraphRequest {
	t.Helper()
	file := f.Put(t, ex, []byte(content))
	tree := &cache.Tree{Entries: []cache.TreeEntry{{Path: "a.txt", Type: cache.EntryFile, Mode: 0o644, Blob: &file}}}
	stored := f.Put(t, ex, tree.Encode())
	from := graph.Ref(0)
	g := &graph.Graph{
		Version:      graph.Version,
		ExecPlatform: platforms.Format(f.Platform),
		Nodes: []graph.Node{
			{Label: "src", Desc: `glob "src"`, Pos: "BUILD:1:1", Source: &graph.Source{Tree: stored}},
			{Label: "app", Desc: `layer "app"`, Pos: "BUILD:2:1", Layer: &graph.Layer{
				Base:     graph.Scratch,
				Platform: platforms.Format(f.Platform),
				Steps:    []graph.Step{{Op: graph.OpAdd, Display: `add("src", "/data")`, From: &from, Dest: "/data"}},
			}},
		},
		Roots: []graph.Ref{1},
	}
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	return api.GraphRequest{Graph: f.Put(t, ex, graph.Encode(g)), Version: graph.Version, Platform: f.Platform}
}

// finishJob submits req, a push or export, watches it to its end and returns its outcome, which must be a success.
func finishJob(t *testing.T, ex api.Executor, req api.Request) api.Outcome {
	t.Helper()
	ctx := t.Context()
	id, err := ex.Submit(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	watchAll(t, ex, id, 0)
	outcome, err := ex.Result(ctx, id)
	if err != nil || outcome.Failure != nil {
		t.Fatalf("Result = %+v, %v; want success", outcome, err)
	}
	return outcome
}

// testExportValuesOwned checks that an export job owns its request's values, as testDeliveryValuesOwned does for
// a push: layer is a built layer, and other an image ID that names nothing.
func testExportValuesOwned(t *testing.T, f Fixture, ex api.Executor, layer api.Result, other string) {
	t.Helper()
	ctx := t.Context()
	export := &api.ExportRequest{Result: layer, Trace: &tracing.CallContext{Parent: "client-1"}}
	id, err := ex.Submit(ctx, api.Request{Export: export})
	if err != nil {
		t.Fatal(err)
	}
	export.Result.Image, export.Result.Node, export.Trace.Parent = other, 7, "changed"
	watchAll(t, ex, id, 0)
	outcome, err := ex.Result(ctx, id)
	if err != nil || outcome.Failure != nil || outcome.Exported == nil || outcome.Exported.Manifest.Digest == "" {
		t.Fatalf("an export whose request changed after Submit = %+v, %v; want the image submitted exported",
			outcome, err)
	}
	returnedOwned(t, ex, id)

	// A task's files are a pointer, which the caller still holds.
	task := runGraph(t, ex, taskGraph(t, f, ex, "owned delivery"))[0]
	files := *task.Files
	export = &api.ExportRequest{Result: task}
	if id, err = ex.Submit(ctx, api.Request{Export: export}); err != nil {
		t.Fatal(err)
	}
	*export.Result.Files = cache.Blob{Digest: digest.FromString("changed"), Size: 7}
	watchAll(t, ex, id, 0)
	outcome, err = ex.Result(ctx, id)
	if err != nil || outcome.Failure != nil || outcome.Exported == nil || outcome.Exported.Files == nil ||
		*outcome.Exported.Files != files {
		t.Fatalf("a task export whose files changed after Submit = %+v, %v; want the files submitted, %s",
			outcome, err, files.Digest)
	}
	returnedOwned(t, ex, id)
}

// testExport checks that an export job publishes a task's tree, and an image's manifest and blobs,
// to the content store, as content references the client can read, and that nothing else crosses.
// An executor that exports nothing must refuse the job before it starts, and locate every image at a registry.
func testExport(t *testing.T, f Fixture) {
	f.requireBuilds(t)
	ex := f.New(t)
	ctx := t.Context()
	if f.NoExport {
		layer := runGraph(t, ex, layerGraph(t, f, ex, "located"))[0]
		atRegistry := func(l cache.Location) bool { return l.Kind == cache.LocationRegistry }
		if !slices.ContainsFunc(layer.Locations, atRegistry) {
			t.Errorf("locations = %+v, want the image at a registry, since the executor exports nothing",
				layer.Locations)
		}
		id, err := ex.Submit(ctx, api.Request{Export: &api.ExportRequest{Result: layer}})
		if err == nil || !strings.Contains(err.Error(), "exports nothing") {
			t.Errorf("Submit of an export = %q, %v; want it refused as exporting nothing", id, err)
		}
		return
	}
	task := runGraph(t, ex, taskGraph(t, f, ex, "exported"))
	outcome := finishJob(t, ex, api.Request{Export: &api.ExportRequest{Result: task[0]}})
	exp := outcome.Exported
	if exp == nil || exp.Files == nil || *exp.Files != *task[0].Files || exp.Manifest.Digest != "" {
		t.Errorf("exported = %+v, want the task's tree %s alone", exp, task[0].Files.Digest)
	}

	layer := runGraph(t, ex, layerGraph(t, f, ex, "exported"))
	outcome = finishJob(t, ex, api.Request{Export: &api.ExportRequest{Result: layer[0]}})
	exp = outcome.Exported
	if exp == nil || exp.Manifest.Digest == "" || exp.MediaType == "" || len(exp.Blobs) < 2 || exp.Files != nil {
		t.Fatalf("exported = %+v, want the image's manifest, media type, and its config and layer", exp)
	}
	if missing := f.Missing(t, ex, append([]cache.Blob{exp.Manifest}, exp.Blobs...)); len(missing) != 0 {
		t.Errorf("the content store lacks exported blobs %v", missing)
	}
	// Exporting again publishes the same content.
	again := finishJob(t, ex, api.Request{Export: &api.ExportRequest{Result: layer[0]}}).Exported
	if !reflect.DeepEqual(again, exp) {
		t.Errorf("exported again = %+v, want %+v", again, exp)
	}

	// A result that made nothing can't be exported.
	id, err := ex.Submit(ctx, api.Request{Export: &api.ExportRequest{Result: api.Result{Node: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	watchAll(t, ex, id, 0)
	if outcome, err := ex.Result(ctx, id); err != nil || outcome.Failure == nil ||
		!strings.Contains(outcome.Failure.Message, "made no image") {
		t.Errorf("exporting an empty result = %+v, %v; want a failure saying it made no image", outcome, err)
	}
}

// testPush checks that a push job sends an image to a registry, reporting its ID there,
// and that a destination that isn't a registry reference is refused before any work.
func testPush(t *testing.T, f Fixture) {
	f.requireBuilds(t)
	ex := f.New(t)
	ctx := t.Context()
	server := httptest.NewServer(f.Registry)
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	f.insecure(t, host)
	layer := runGraph(t, ex, layerGraph(t, f, ex, "pushed"))
	push := api.PushRequest{Result: layer[0], Destination: host + "/app:v1"}
	outcome := finishJob(t, ex, api.Request{Push: &push})
	if outcome.Pushed == nil || outcome.Pushed.Image != layer[0].Image || outcome.Pushed.Index != "" {
		t.Errorf("pushed = %+v, want the image's ID, %s", outcome.Pushed, layer[0].Image)
	}
	// A destination is a full reference with a tag: the client resolves short names and adds :latest.
	for _, bad := range []string{"", "not a reference!", host + "/app@sha256:" + strings.Repeat("0", 64),
		host + "/app", "app:v1"} {
		req := api.PushRequest{Result: layer[0], Destination: bad}
		if id, err := ex.Submit(ctx, api.Request{Push: &req}); err == nil {
			t.Errorf("Submit accepted the push destination %q as job %s", bad, id)
		}
	}
	if _, err := ex.Submit(ctx, api.Request{Graph: &api.GraphRequest{}, Push: &api.PushRequest{}}); err == nil {
		t.Error("Submit accepted a request asking for two jobs")
	}
}

// manyTasksGraph stores a graph in ex, through f, of count tasks on scratch, each adding the same file, of data,
// so that running it makes at least count status events, and returns the graph's request.
func manyTasksGraph(t *testing.T, f Fixture, ex api.Executor, count int, data string) api.GraphRequest {
	t.Helper()
	file := f.Put(t, ex, []byte(data))
	tree := &cache.Tree{Entries: []cache.TreeEntry{{Path: "a.txt", Type: cache.EntryFile, Mode: 0o644, Blob: &file}}}
	stored := f.Put(t, ex, tree.Encode())
	from := graph.Ref(0)
	g := &graph.Graph{
		Version:      graph.Version,
		ExecPlatform: platforms.Format(f.Platform),
		Nodes: []graph.Node{
			{Label: "src", Desc: `glob "src"`, Pos: "BUILD:1:1", Source: &graph.Source{Tree: stored}},
		},
	}
	for i := range count {
		g.Nodes = append(g.Nodes, graph.Node{Label: fmt.Sprintf("t%d", i), Desc: fmt.Sprintf(`task "t%d"`, i),
			Pos: fmt.Sprintf("BUILD:%d:1", i+2), Task: &graph.Task{
				Base: graph.Scratch,
				Steps: []graph.Step{{Op: graph.OpAdd, Display: `add("src", "/out")`, From: &from,
					Dest: fmt.Sprintf("/out%d", i)}},
			}})
		g.Roots = append(g.Roots, graph.Ref(i+1))
	}
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	return api.GraphRequest{Graph: f.Put(t, ex, graph.Encode(g)), Version: graph.Version, Platform: f.Platform}
}

// testRetention checks that an executor retains a bounded number of a job's events: a watch from an expired
// position gets a gap event naming the first retained one, then the rest; a watcher that falls behind while the
// job runs is dropped with ErrFellBehind, unless events are buffered on their way, and watching again from where it
// was meets a gap.
func testRetention(t *testing.T, f Fixture) {
	if f.Retention <= 0 || f.Retention > 8 {
		t.Skip("the fixture doesn't retain few enough events")
	}
	f.requireBuilds(t)
	ex := f.New(t)
	ctx := t.Context()

	var id api.JobID
	var expired int64 // a position the job no longer retains once it has ended
	if f.Buffered {
		// The job makes more events than are retained, so its first has expired once it ends.
		req := manyTasksGraph(t, f, ex, 2*f.Retention, "retained")
		var err error
		if id, err = ex.Submit(ctx, api.Request{Graph: &req}); err != nil {
			t.Fatal(err)
		}
		watchAll(t, ex, id, 0)
	} else {
		id, expired = slowWatcher(t, f, ex)
	}
	outcome, err := ex.Result(ctx, id)
	if err != nil || outcome.Failure != nil {
		t.Fatalf("Result = %+v, %v; want success", outcome, err)
	}

	// Watching from the expired position meets a gap, then the retained events to the terminal one.
	events := watchAll(t, ex, id, expired)
	if len(events) < 2 || events[0].Kind != api.EventGap || events[0].First <= events[0].Position {
		t.Fatalf("watching from an expired position delivered %+v first, want a gap naming a later position", events[0])
	}
	first := events[0].First
	if len(events)-1 > f.Retention {
		t.Errorf("%d events retained, want at most %d", len(events)-1, f.Retention)
	}
	for i, e := range events[1:] {
		if e.Position != first+int64(i) {
			t.Errorf("event %d after the gap has position %d, want %d", i, e.Position, first+int64(i))
		}
	}
	if last := events[len(events)-1]; last.Kind != api.EventDone {
		t.Errorf("the last event is %+v, want the terminal event", last)
	}
	// Watching from a retained position gets no gap.
	if retained := watchAll(t, ex, id, first); len(retained) == 0 || retained[0].Kind == api.EventGap {
		t.Errorf("watching from the first retained position, %d, delivered %+v first", first, retained)
	}
}

// slowWatcher runs a job on ex and watches it slowly: the watcher reads the first event, then waits for the job to
// end before reading on, so that it falls behind and is dropped with ErrFellBehind, and returns the job and the
// position after the last event the watcher saw, which has expired.
// The contract doesn't make the job wait for a watcher, so one that starts only after the job has made
// more events than are retained meets a gap first, and isn't dropped; the job is then run again, afresh.
func slowWatcher(t *testing.T, f Fixture, ex api.Executor) (api.JobID, int64) {
	t.Helper()
	ctx := t.Context()
	var id api.JobID
	var seen []api.Event
	var err error
	for attempt := range 3 {
		req := manyTasksGraph(t, f, ex, 2*f.Retention, fmt.Sprintf("retained %d", attempt))
		if id, err = ex.Submit(ctx, api.Request{Graph: &req}); err != nil {
			t.Fatal(err)
		}
		seen = nil
		err = ex.Watch(ctx, id, 0, func(e api.Event) error {
			seen = append(seen, e)
			for {
				if _, err := ex.Result(ctx, id); !errors.Is(err, api.ErrRunning) {
					return nil
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
		if len(seen) == 0 || seen[0].Kind != api.EventGap {
			break
		}
	}
	if len(seen) > 0 && seen[0].Kind == api.EventGap {
		t.Fatal("every job outran the watcher, which began only after its first events had expired")
	}
	if !errors.Is(err, api.ErrFellBehind) {
		t.Fatalf("a slow watcher's Watch = %v, want %v", err, api.ErrFellBehind)
	}
	if len(seen) == 0 {
		t.Fatal("the slow watcher saw no event before being dropped")
	}
	return id, seen[len(seen)-1].Position + 1
}

// positions returns the positions of events.
func positions(events []api.Event) []int64 {
	var ps []int64
	for _, e := range events {
		ps = append(ps, e.Position)
	}
	return ps
}

// testRefused checks that a graph the executor can't run is refused by Submit, before any work starts.
func testRefused(t *testing.T, f Fixture) {
	ex := f.New(t)
	ctx := t.Context()
	req := taskGraph(t, f, ex, "refused")
	for _, tc := range []struct {
		name   string
		change func(*api.GraphRequest)
		want   string
	}{
		{
			name:   "unsupported graph version",
			change: func(r *api.GraphRequest) { r.Version = graph.Version + 1000 },
			want:   fmt.Sprintf("graph version %d", graph.Version+1000),
		},
		{
			name:   "another platform",
			change: func(r *api.GraphRequest) { r.Platform = imgspecv1.Platform{OS: "plan9", Architecture: "mips"} },
			want:   "plan9/mips",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := req
			tc.change(&r)
			id, err := ex.Submit(ctx, api.Request{Graph: &r})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Submit = %q, %v; want an error naming %q", id, err, tc.want)
			}
			if id != "" {
				if _, err := ex.Result(ctx, id); !errors.Is(err, api.ErrNoJob) {
					t.Errorf("a refused submission returned the job %q, whose result is %v", id, err)
				}
			}
		})
	}
	// A result to push or export names images by their IDs; anything else names nothing the executor made.
	valid := strings.Repeat("a", 64)
	for _, res := range []api.Result{
		{Image: "x"},
		{Image: strings.Repeat("A", 64)},
		{Image: "sha256:" + valid},
		{Index: []api.Manifest{{Image: valid, Platform: "linux/amd64"}, {Image: "x", Platform: "linux/arm64"}}},
	} {
		for _, r := range []api.Request{
			{Export: &api.ExportRequest{Result: res}},
			{Push: &api.PushRequest{Result: res, Destination: "example.com/app:v1"}},
		} {
			id, err := ex.Submit(ctx, r)
			if err == nil || !strings.Contains(err.Error(), "image ID") {
				t.Errorf("Submit of %+v = %q, %v; want the image ID refused", res, id, err)
			}
		}
	}
	t.Run("nothing asked", func(t *testing.T) {
		if _, err := ex.Submit(ctx, api.Request{}); err == nil {
			t.Error("Submit accepted a request that asks for nothing")
		}
	})
}

// testUnknownJob checks that every call about a job the executor doesn't have fails with ErrNoJob.
func testUnknownJob(t *testing.T, f Fixture) {
	ex := f.New(t)
	ctx := t.Context()
	const id api.JobID = "no-such-job"
	if err := ex.Watch(ctx, id, 0, func(api.Event) error { return nil }); !errors.Is(err, api.ErrNoJob) {
		t.Errorf("Watch = %v, want %v", err, api.ErrNoJob)
	}
	if err := ex.Cancel(ctx, id); !errors.Is(err, api.ErrNoJob) {
		t.Errorf("Cancel = %v, want %v", err, api.ErrNoJob)
	}
	if _, err := ex.Result(ctx, id); !errors.Is(err, api.ErrNoJob) {
		t.Errorf("Result = %v, want %v", err, api.ErrNoJob)
	}
	if err := ex.Release(ctx, id); !errors.Is(err, api.ErrNoJob) {
		t.Errorf("Release = %v, want %v", err, api.ErrNoJob)
	}
}

// testRelease checks that a finished job can be released, after which its calls either still answer or say it
// expired, never that it is unknown, and that a watch past the terminal event delivers nothing and returns.
func testRelease(t *testing.T, f Fixture) {
	f.requireBuilds(t)
	ex := f.New(t)
	ctx := t.Context()
	req := taskGraph(t, f, ex, "released")
	id, err := ex.Submit(ctx, api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	events := watchAll(t, ex, id, 0)
	last := events[len(events)-1]
	if last.Kind != api.EventDone {
		t.Fatalf("the last event = %+v, want the terminal one", last)
	}
	if err := ex.Watch(ctx, id, last.Position+1, func(e api.Event) error {
		return fmt.Errorf("a watch past the terminal event delivered %+v", e)
	}); err != nil {
		t.Error(err)
	}
	if err := ex.Release(ctx, id); err != nil {
		t.Fatalf("Release = %v, want it accepted", err)
	}
	if _, err := ex.Result(ctx, id); err != nil && !errors.Is(err, api.ErrExpired) {
		t.Errorf("Result after Release = %v, want the outcome or %v", err, api.ErrExpired)
	}
}

// testCancel runs a graph whose pull waits on a registry that never answers, checks that Result says it is running,
// cancels it, and checks that its terminal event reports the cancellation.
func testCancel(t *testing.T, f Fixture) {
	f.requireBuilds(t)
	ex := f.New(t)
	ctx := t.Context()
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	f.insecure(t, host)
	req := pullGraph(t, f, ex, host+"/base:v1")
	id, err := ex.Submit(ctx, api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Result(ctx, id); !errors.Is(err, api.ErrRunning) {
		t.Errorf("Result of a running job = %v, want %v", err, api.ErrRunning)
	}
	// The context of a watch governs the watch alone: its end leaves the job running.
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := ex.Watch(short, id, 0, func(api.Event) error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Watch with a context that ended = %v, want %v", err, context.DeadlineExceeded)
	}
	if _, err := ex.Result(ctx, id); !errors.Is(err, api.ErrRunning) {
		t.Errorf("Result after a watch's context ended = %v, want %v, the job still running", err, api.ErrRunning)
	}
	if err := ex.Watch(ctx, id, -1, func(api.Event) error { return nil }); err == nil {
		t.Error("Watch from a negative position succeeded")
	}
	if err := ex.Cancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	events := watchAll(t, ex, id, 0)
	last := events[len(events)-1]
	if last.Kind != api.EventDone || last.Outcome == nil || !last.Outcome.Cancelled {
		t.Errorf("last event = %+v, want the terminal event reporting the cancellation", last)
	}
	if outcome, err := ex.Result(ctx, id); err != nil || !outcome.Cancelled {
		t.Errorf("Result = %+v, %v; want the cancellation", outcome, err)
	}
	// Cancelling a job that has ended changes nothing.
	if err := ex.Cancel(ctx, id); err != nil {
		t.Errorf("Cancel of an ended job = %v", err)
	}
}

// pullGraph stores a graph in ex, through f, whose root pulls image, and returns the graph's request.
func pullGraph(t *testing.T, f Fixture, ex api.Executor, image string) api.GraphRequest {
	t.Helper()
	g := &graph.Graph{
		Version:      graph.Version,
		ExecPlatform: platforms.Format(f.Platform),
		Nodes: []graph.Node{{Label: "base", Desc: `pull "base"`, Pos: "BUILD:1:1", Pull: &graph.Pull{
			Image:    image,
			Platform: platforms.Format(f.Platform),
		}}},
		Roots: []graph.Ref{0},
	}
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	return api.GraphRequest{Graph: f.Put(t, ex, graph.Encode(g)), Version: graph.Version, Platform: f.Platform}
}

// testFailure runs a graph whose pull names an image its registry lacks, and checks that the job fails,
// not cancelled, with a failure that Result returns too, and that a job which succeeded isn't marked cancelled
// by a cancel after its end.
func testFailure(t *testing.T, f Fixture) {
	f.requireBuilds(t)
	ex := f.New(t)
	ctx := t.Context()
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	f.insecure(t, host)
	req := pullGraph(t, f, ex, host+"/missing:v1")
	id, err := ex.Submit(ctx, api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	events := watchAll(t, ex, id, 0)
	last := events[len(events)-1]
	if last.Kind != api.EventDone || last.Outcome == nil || last.Outcome.Failure == nil || last.Outcome.Cancelled {
		t.Fatalf("last event = %+v, want the terminal event reporting a failure", last)
	}
	// The failure names the node as data; the client renders it against its graph.
	if f := last.Outcome.Failure; !slices.Equal(f.Nodes, []graph.Ref{0}) || !strings.Contains(f.Message, "missing:v1") {
		t.Errorf("failure = %+v, want it to name node 0 and the image", f)
	}
	outcome, err := ex.Result(ctx, id)
	if err != nil || outcome.Failure == nil || outcome.Failure.Message != last.Outcome.Failure.Message {
		t.Errorf("Result = %+v, %v; want the failure", outcome, err)
	}
	if err := ex.Cancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if outcome, err := ex.Result(ctx, id); err != nil || outcome.Cancelled {
		t.Errorf("Result after cancelling an ended job = %+v, %v; want it unchanged", outcome, err)
	}

	// A watcher whose fn fails stops with that error.
	stop := errors.New("stop")
	err = ex.Watch(ctx, id, 0, func(api.Event) error { return stop })
	if err == nil || err.Error() != stop.Error() {
		t.Errorf("Watch with a failing fn = %v, want %v", err, stop)
	}

	// A graph the executor can't read fails the job, naming the blob, rather than being refused.
	gone := req
	gone.Graph = cache.Blob{Digest: digest.FromString("gone"), Size: 10}
	id, err = ex.Submit(ctx, api.Request{Graph: &gone})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err = ex.Result(ctx, id)
	for errors.Is(err, api.ErrRunning) {
		watchAll(t, ex, id, 0)
		outcome, err = ex.Result(ctx, id)
	}
	if err != nil || outcome.Failure == nil || !strings.Contains(outcome.Failure.Message, gone.Graph.Digest.String()) {
		t.Errorf("Result of a job with a missing graph = %+v, %v; want a failure naming the blob", outcome, err)
	}

	// A graph larger than an executor decodes fails the job without being read.
	huge := req
	huge.Graph.Size = 1 << 40
	id, err = ex.Submit(ctx, api.Request{Graph: &huge})
	if err != nil {
		t.Fatal(err)
	}
	watchAll(t, ex, id, 0)
	outcome, err = ex.Result(ctx, id)
	if err != nil || outcome.Failure == nil || !strings.Contains(outcome.Failure.Message, "more bytes than allowed") {
		t.Errorf("Result of a job with an oversized graph = %+v, %v; want a failure saying so", outcome, err)
	}
}

// settleWait bounds how long pruneSettled asks again, and settlePause is how long it waits between asks.
const (
	settleWait  = 15 * time.Second
	settlePause = 50 * time.Millisecond
)

// pruneSettled starts a prune on ex, asking again while ex answers ErrSettling, as a coordinator does while the
// attempts of a cancelled build are still ending on its workers.
func pruneSettled(t *testing.T, ex api.Executor) (api.JobID, error) {
	t.Helper()
	ctx := t.Context()
	for start := time.Now(); ; {
		id, err := ex.Prune(ctx)
		if !errors.Is(err, api.ErrSettling) || time.Since(start) > settleWait {
			return id, err
		}
		time.Sleep(settlePause)
	}
}

// testPrune checks that a prune is a job whose outcome counts what it removed, and that it is refused
// while another job runs.
func testPrune(t *testing.T, f Fixture) {
	f.requireBuilds(t)
	ex := f.New(t)
	ctx := t.Context()
	req := taskGraph(t, f, ex, "pruned")
	id, err := ex.Submit(ctx, api.Request{Graph: &req})
	if err != nil {
		t.Fatal(err)
	}
	watchAll(t, ex, id, 0)
	if err := ex.Release(ctx, id); err != nil { // the client is done with it: a held build keeps a prune out
		t.Fatal(err)
	}

	// A job that waits on a registry that never answers is running while the prune is asked for.
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	f.insecure(t, host)
	pull := pullGraph(t, f, ex, host+"/base:v1")
	running, err := ex.Submit(ctx, api.Request{Graph: &pull})
	if err != nil {
		t.Fatal(err)
	}
	// It is refused for the running job, or, on a coordinator, for the job's attempt on a worker: either keeps it out.
	if id, err := ex.Prune(ctx); err == nil ||
		!(strings.Contains(err.Error(), "other jobs are running") || errors.Is(err, api.ErrSettling)) {
		t.Errorf("a prune while a job runs = %q, %v; want it refused", id, err)
	}
	if err := ex.Cancel(ctx, running); err != nil {
		t.Fatal(err)
	}
	watchAll(t, ex, running, 0)
	// An executor that holds builds for their clients, as a coordinator does, refuses a prune until the build is
	// released; one that doesn't, as the local executor, whose lock covers delivery, accepts it at once.
	id, err = ex.Prune(ctx)
	if err != nil {
		if !strings.Contains(err.Error(), "other jobs are running") && !errors.Is(err, api.ErrSettling) {
			t.Errorf("a prune while a finished build is held = %q, %v; want it refused as open, or accepted", id, err)
		}
		if err := ex.Release(ctx, running); err != nil {
			t.Fatal(err)
		}
		if id, err = pruneSettled(t, ex); err != nil {
			t.Fatal(err)
		}
	}
	events := watchAll(t, ex, id, 0)
	last := events[len(events)-1]
	if last.Kind != api.EventDone || last.Outcome == nil || last.Outcome.Pruned == nil {
		t.Fatalf("last event = %+v, want the terminal event with what was pruned", last)
	}
	if p := last.Outcome.Pruned; p.Results == 0 || p.Blobs == 0 {
		t.Errorf("pruned %+v, want the task's result and blobs", p)
	}
}
