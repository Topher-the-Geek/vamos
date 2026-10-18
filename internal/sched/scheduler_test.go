package sched

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/fileset"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/Topher-the-Geek/vamos/internal/tracing"

	"github.com/opencontainers/go-digest"
	"github.com/spf13/afero"
)

// fakeRunner is a Runner, and the Checker of its results, that makes nothing: it answers resolves from a registry
// of its own, gives layers images named by their keys, gives tasks an empty tree, and reports results as a Runner
// must, writing no record.
// It never opens container storage.
type fakeRunner struct {
	content cache.ContentStore
	// registry is what each tag names now: an index, whose images for each platform are named by the index.
	registry map[string]digest.Digest
	// moved, if set, is called after each resolve, so that a test can move a tag between resolutions.
	moved func()
	// fail, if set, is asked before each layer or task runs whether it should fail, and with what.
	fail     func(req Request) error
	resolves []Request       // every resolve run, in order
	runs     []digest.Digest // the keys of the layers and tasks run, in order
	images   map[digest.Digest]bool
	files    map[digest.Digest][]cache.Blob // the files each task's tree holds, by task key
}

// imageOf returns the ID of the image in index for platform.
func imageOf(index digest.Digest, platform string) digest.Digest {
	return digest.FromString(index.String() + " " + platform)
}

// Check reports a result unusable if its image isn't one the fake made, or its files are missing.
func (f *fakeRunner) Check(ctx context.Context, a *Action, result *cache.ActionResult) (error, error) {
	if (a.Kind == KindLayer || a.Kind == KindResolve) && !f.images[result.Image] {
		return fmt.Errorf("its image %s is gone", result.Image), nil
	}
	if result.Output != nil {
		if err := cache.CheckTree(ctx, f.content, *result.Output); err != nil {
			return StoreProblem("its files can't be used", err)
		}
	}
	return nil, nil
}

// Run resolves a tag, or makes a layer's image or a task's files, and reports the result.
func (f *fakeRunner) Run(ctx context.Context, req Request, ev Output) (*Report, error) {
	a := req.Action
	if a.Kind == KindResolve {
		f.resolves = append(f.resolves, req)
		index := a.Index
		if index == "" && !req.Again && req.Tag != nil {
			index = req.Tag.Digest
		}
		if index == "" {
			index = f.registry[a.Image]
			if index == "" {
				return nil, fmt.Errorf("image %q: not found", a.Image)
			}
		}
		if f.moved != nil {
			f.moved()
		}
		id := imageOf(index, a.Platform)
		f.images[id] = true
		result := &cache.ActionResult{Image: id, Platform: a.Platform, Index: index, Epoch: 1,
			Locations: []cache.Location{storeLocation(id.Encoded())}}
		record := &cache.TagRecord{Digest: index, Instances: map[string]cache.TagInstance{}}
		if req.Tag != nil && req.Tag.Digest == index {
			maps.Copy(record.Instances, req.Tag.Instances)
		}
		record.Instances[a.Platform] = cache.TagInstance{Digest: id, Image: id, Platform: a.Platform, Epoch: 1,
			Locations: result.Locations}
		return &Report{Result: result, Tag: a.Image, TagRecord: record}, nil
	}

	f.runs = append(f.runs, req.Key)
	if f.fail != nil {
		if err := f.fail(req); err != nil {
			return nil, err
		}
	}
	ev.Output(cache.StreamStdout, []byte("ran "+req.Key.Encoded()[:8]+"\n"))
	log, err := cache.NewLog()
	if err != nil {
		return nil, err
	}
	defer log.Discard()
	log.Writer(cache.StreamStdout, io.Discard).Write([]byte("ran " + req.Key.Encoded()[:8] + "\n"))
	result := &cache.ActionResult{Epoch: req.Epoch.Unix()}
	if result.Log, err = log.Save(ctx, f.content); err != nil {
		return nil, err
	}
	switch a.Kind {
	case KindLayer:
		result.Image, result.Platform = digest.FromString("image for "+req.Key.String()), a.Platform
		result.Locations = []cache.Location{storeLocation(result.Image.Encoded())}
		f.images[result.Image] = true
	case KindTask:
		if a.Output != nil {
			// Each task's tree holds two files, so that a test can corrupt them, written through req.Rewrite's view.
			tree := &cache.Tree{}
			f.files[req.Key] = nil
			for _, name := range []string{"f", "g"} {
				file, _, err := cache.PutFile(ctx, Rewriting{ContentStore: f.content, Corrupt: req.Rewrite},
					memFile(name+" made by "+req.Key.String()), "/f", 100)
				if err != nil {
					return nil, err
				}
				f.files[req.Key] = append(f.files[req.Key], file)
				tree.Entries = append(tree.Entries,
					cache.TreeEntry{Path: name, Type: cache.EntryFile, Mode: 0o644, Blob: &file})
			}
			b, err := f.content.PutBytes(ctx, tree.Encode())
			if err != nil {
				return nil, err
			}
			result.Output = &b
		}
	}
	return &Report{Result: result}, nil
}

// memFile returns a filesystem holding the file /f with content.
func memFile(content string) afero.Fs {
	fsys := afero.NewMemMapFs()
	afero.WriteFile(fsys, "/f", []byte(content), 0o644)
	return fsys
}

// recordedEvents keeps the statuses and output a Scheduler reports, which targets running at once may send at once.
type recordedEvents struct {
	mu       sync.Mutex
	statuses []string
	output   []string
}

// Status keeps the status, by node.
func (e *recordedEvents) Status(node graph.Ref, status string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.statuses = append(e.statuses, fmt.Sprintf("%d: %s", node, status))
}

// Output keeps the output, by node and stream.
func (e *recordedEvents) Output(node graph.Ref, stream byte, data []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.output = append(e.output, fmt.Sprintf("%d/%d: %s", node, stream, data))
}

// Spans discards spans.
func (e *recordedEvents) Spans([]tracing.Span) {}

// fakeFixture returns stores in memory and a fake Runner on them, whose registry has index for the tag "base:v1".
func fakeFixture(t testing.TB) (*cache.Memory, *fakeRunner) {
	t.Helper()
	m := cache.NewMemory()
	f := &fakeRunner{
		content:  m,
		registry: map[string]digest.Digest{"example.com/base:v1": digest.FromString("index 1")},
		images:   map[digest.Digest]bool{},
		files:    map[digest.Digest][]cache.Blob{},
	}
	return m, f
}

// execution is a Runner that checks its own results, as the local Runner does.
type execution interface {
	Runner
	Checker
}

// schedule returns a Scheduler of g on the stores content and records, running through r, shared, one target
// at a time, as the local executor does, with rebuild for the roots.
func schedule(
	g *graph.Graph, content cache.ContentStore, records cache.RecordStore, r execution, ev Events,
	rebuild bool, timestamp time.Time,
) *Scheduler {
	b := Backend{Content: content, Records: records, Checker: r, Runner: NewSharing(r, content, records)}
	return New(g, b, ev, Options{Rebuild: rebuild, Timestamp: timestamp, Bound: 1})
}

// multiPlatformGraph returns a graph that pulls one tag for two platforms, builds a layer on each,
// and lists both in an index: nodes 0 and 1 are the pulls, 2 and 3 the layers, 4 the index.
func multiPlatformGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := &graph.Graph{Version: graph.Version, ExecPlatform: "linux/amd64", Roots: []graph.Ref{4}}
	for i, platform := range []string{"linux/amd64", "linux/arm64"} {
		g.Nodes = append(g.Nodes, graph.Node{Label: "base", Desc: fmt.Sprintf(`pull "base" (%s)`, platform),
			Pull: &graph.Pull{Image: "example.com/base:v1", Platform: platform}})
		g.Nodes = append(g.Nodes, graph.Node{Label: "app", Desc: fmt.Sprintf(`layer "app" (%s)`, platform),
			Layer: &graph.Layer{Base: graph.Ref(2 * i), Platform: platform, Steps: []graph.Step{
				{Op: graph.OpRun, Display: `run(["make"])`, Argv: []string{"make"}},
			}}})
	}
	// Reorder so that the pulls come first, as the loader lays nodes out.
	g.Nodes = []graph.Node{g.Nodes[0], g.Nodes[2], g.Nodes[1], g.Nodes[3]}
	g.Nodes[2].Layer.Base, g.Nodes[3].Layer.Base = 0, 1
	g.Nodes = append(g.Nodes, graph.Node{Label: "all", Desc: `index "all"`,
		Index: &graph.Index{Manifests: []graph.Ref{2, 3}}})
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	return g
}

// TestSchedulerPinsIndex checks that a tag moved between two platforms' resolutions gives both platforms images
// from the first index, since the second resolution asks for the index the first returned,
// and that the index assembled lists both, for their platforms, without any container storage.
func TestSchedulerPinsIndex(t *testing.T) {
	m, f := fakeFixture(t)
	first := f.registry["example.com/base:v1"]
	// After the first resolution the tag moves, and another job records the move, as a concurrent --rebuild would:
	// only the pin keeps the second platform on the first index.
	f.moved = func() {
		f.registry["example.com/base:v1"] = digest.FromString("index 2")
		testenv.Must(t, m.SetTag(t.Context(), "example.com/base:v1",
			&cache.TagRecord{Digest: digest.FromString("index 2")}))
		f.moved = nil
	}
	ev := &recordedEvents{}
	s := schedule(multiPlatformGraph(t), m, m, f, ev, false, time.Time{})
	results, err := s.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.resolves) != 2 || f.resolves[0].Action.Index != "" || f.resolves[1].Action.Index != first {
		t.Errorf("resolves asked for indexes %q, %q; want none, then the first's", f.resolves[0].Action.Index,
			f.resolves[1].Action.Index)
	}
	imageFor := func(ref graph.Ref) string {
		return digest.FromString("image for " + s.inputs(s.graph.Node(ref)).Key().String()).Encoded()
	}
	// Each manifest carries where its image is, as the layer's result recorded it.
	want := []api.Manifest{
		{Image: imageFor(2), Platform: "linux/amd64", Locations: []cache.Location{storeLocation(imageFor(2))}},
		{Image: imageFor(3), Platform: "linux/arm64", Locations: []cache.Location{storeLocation(imageFor(3))}},
	}
	if len(results) != 1 || !reflect.DeepEqual(results[0].Index, want) {
		t.Errorf("results = %+v, want the index %+v", results, want)
	}
	for i, platform := range []string{"linux/amd64", "linux/arm64"} {
		if got, want := s.images[graph.Ref(i)].id, imageOf(first, platform).Encoded(); got != want {
			t.Errorf("the base for %s is %s, want %s, from the first index", platform, got, want)
		}
	}
	// The job's view of the tag wins: the latest write is the second resolution's, of the pinned index.
	if record, err := m.Tag(t.Context(), "example.com/base:v1"); err != nil || record == nil || record.Digest != first {
		t.Errorf("tag record = %+v, %v; want the first index", record, err)
	}

	// A later job with --rebuild for the pulls resolves afresh, and the moved tag's index serves both platforms.
	f.resolves = nil
	g := multiPlatformGraph(t)
	g.Roots = []graph.Ref{0, 1}
	s = schedule(g, m, m, f, ev, true, time.Time{})
	if _, err := s.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	second := digest.FromString("index 2")
	if !f.resolves[0].Again || f.resolves[1].Again || f.resolves[1].Action.Index != second {
		t.Errorf("resolves again = %v, %v with index %q; want the first afresh and the second pinned to %s",
			f.resolves[0].Again, f.resolves[1].Again, f.resolves[1].Action.Index, second)
	}
	if !slices.Contains(ev.statuses, "0: pulling again") {
		t.Errorf("statuses = %q, want the pull reported as pulled again", ev.statuses)
	}
}

// TestSchedulerFreshResolveForced checks that a pull which resolves its tag afresh, because another pull of the tag
// is rebuilt, is forced even though it isn't a root, so that it never joins a resolution in flight that reads
// the record store's stale index and would pin it for the rebuilt pull.
func TestSchedulerFreshResolveForced(t *testing.T) {
	m, f := fakeFixture(t)
	g := multiPlatformGraph(t)
	g.Roots = []graph.Ref{1} // the second platform's pull alone is rebuilt
	if _, err := schedule(g, m, m, f, &recordedEvents{}, true, time.Time{}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.resolves) != 2 {
		t.Fatalf("%d resolves, want 2", len(f.resolves))
	}
	if first := f.resolves[0]; !first.Again || !first.Force {
		t.Errorf("the first pull resolved with again = %v, force = %v; want a forced fresh resolution",
			first.Again, first.Force)
	}
	if second := f.resolves[1]; second.Again || second.Action.Index != f.registry["example.com/base:v1"] {
		t.Errorf("the rebuilt pull resolved with again = %v and index %q; want it pinned to the fresh index",
			second.Again, second.Action.Index)
	}
}

// TestSchedulerReuse checks that the Scheduler runs a layer and a task once, through the Runner,
// and reuses their results on a second run, replaying their output, and that a result the Runner can't use
// is built again.
func TestSchedulerReuse(t *testing.T) {
	m, f := fakeFixture(t)
	from := graph.Ref(1)
	sel := fileset.DefaultSelection()
	sel.Include = []string{"."}
	g := &graph.Graph{Version: graph.Version, ExecPlatform: "linux/amd64", Roots: []graph.Ref{2}, Nodes: []graph.Node{
		{Label: "base", Desc: `pull "base"`, Pull: &graph.Pull{Image: "example.com/base:v1", Platform: "linux/amd64"}},
		{Label: "gen", Desc: `task "gen"`, Task: &graph.Task{Base: 0, Steps: []graph.Step{
			{Op: graph.OpRun, Display: `run(["gen"])`, Argv: []string{"gen"}},
		}, Output: &graph.Output{Source: "/out", Selection: sel}}},
		{Label: "app", Desc: `layer "app"`, Layer: &graph.Layer{Base: 0, Platform: "linux/amd64", Steps: []graph.Step{
			{Op: graph.OpAdd, Display: `add("gen", "/data")`, From: &from, Dest: "/data"},
		}}},
	}}
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	run := func(timestamp time.Time) *recordedEvents {
		t.Helper()
		ev := &recordedEvents{}
		if _, err := schedule(g, m, m, f, ev, false, timestamp).Run(t.Context()); err != nil {
			t.Fatal(err)
		}
		return ev
	}
	ev := run(time.Time{})
	if want := []string{"1: building", "2: building"}; !slices.Equal(ev.statuses, want) {
		t.Errorf("statuses = %q, want %q", ev.statuses, want)
	}
	if len(f.runs) != 2 {
		t.Fatalf("the Runner ran %d actions, want 2", len(f.runs))
	}
	ev = run(time.Time{})
	if want := []string{"1: cached", "2: cached"}; !slices.Equal(ev.statuses, want) {
		t.Errorf("statuses on the second run = %q, want %q", ev.statuses, want)
	}
	if len(ev.output) != 2 || !strings.HasPrefix(ev.output[0], "1/1: ran ") ||
		!strings.HasPrefix(ev.output[1], "2/1: ran ") {
		t.Errorf("output on the second run = %q, want each target's replayed", ev.output)
	}
	if len(f.runs) != 2 {
		t.Errorf("the Runner ran %d actions after a cached run, want still 2", len(f.runs))
	}

	// The timestamp doesn't change a key, but a layer whose image is gone is built again, with it.
	f.images = map[digest.Digest]bool{}
	ev = run(time.Unix(1_700_000_000, 0))
	if want := "2: building, since the cached result can't be used: its image "; !slices.ContainsFunc(ev.statuses,
		func(s string) bool { return strings.HasPrefix(s, want) }) {
		t.Errorf("statuses = %q, want the layer built again", ev.statuses)
	}
	if len(f.runs) != 3 {
		t.Errorf("the Runner ran %d actions, want 3", len(f.runs))
	}
	result, err := m.Result(t.Context(), f.runs[2])
	if err != nil || result == nil || result.Epoch != 1_700_000_000 {
		t.Errorf("the layer's new result = %+v, %v; want it stamped with the timestamp", result, err)
	}
}

// TestSchedulerStepFailure checks that a step's failure is reported by the target and the step as the BUILD file
// wrote them, and that corrupt sources say what to do.
func TestSchedulerStepFailure(t *testing.T) {
	m, _ := fakeFixture(t)
	tree, err := m.PutBytes(t.Context(), (&cache.Tree{}).Encode())
	if err != nil {
		t.Fatal(err)
	}
	from := graph.Ref(0)
	g := &graph.Graph{Version: graph.Version, ExecPlatform: "linux/amd64", Roots: []graph.Ref{1}, Nodes: []graph.Node{
		{Label: "src", Desc: `glob "src"`, Source: &graph.Source{Tree: tree}},
		{Label: "app", Desc: `layer "app"`, Layer: &graph.Layer{Base: graph.Scratch, Platform: "linux/amd64",
			Steps: []graph.Step{{Op: graph.OpAdd, Display: `add("src", "/data")`, From: &from, Dest: "/data"}}}},
	}}
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	failing := &failingRunner{err: &StepError{Step: 0, Err: cache.Corrupt(tree, "it isn't a tree")}}
	_, err = schedule(g, m, m, failing, &recordedEvents{}, false, time.Time{}).Run(t.Context())
	want := `layer "app": add("src", "/data") failed: blob ` + tree.Digest.String() +
		" is corrupt: it isn't a tree; the project must be captured and submitted again"
	if err == nil || err.Error() != want {
		t.Errorf("run = %v, want %q", err, want)
	}
}

// failingRunner is a Runner whose every Action fails with err.
type failingRunner struct {
	err error
}

// Check finds every result usable.
func (f *failingRunner) Check(context.Context, *Action, *cache.ActionResult) (error, error) {
	return nil, nil
}

// Run fails.
func (f *failingRunner) Run(context.Context, Request, Output) (*Report, error) {
	return nil, f.err
}

// TestSchedulerRegenerates checks that a task whose files a target finds corrupt is run again, with every file
// of its tree written again, through a task that adds another task's files, and that the target is then tried again;
// a task is regenerated once at most.
// Each tree holds two corrupt files, and the read that finds the first removes only it,
// so the second is made again only because the whole tree is rewritten.
func TestSchedulerRegenerates(t *testing.T) {
	m, f := fakeFixture(t)
	sel := fileset.DefaultSelection()
	sel.Include = []string{"."}
	from := func(ref graph.Ref) *graph.Ref { return &ref }
	// Task 1 makes files; task 2 adds them and makes files; layer 3 adds task 2's.
	g := &graph.Graph{Version: graph.Version, ExecPlatform: "linux/amd64", Roots: []graph.Ref{3}, Nodes: []graph.Node{
		{Label: "base", Desc: `pull "base"`, Pull: &graph.Pull{Image: "example.com/base:v1", Platform: "linux/amd64"}},
		{Label: "one", Desc: `task "one"`, Task: &graph.Task{Base: 0, Steps: []graph.Step{
			{Op: graph.OpRun, Display: `run(["one"])`, Argv: []string{"one"}},
		}, Output: &graph.Output{Source: "/out", Selection: sel}}},
		{Label: "two", Desc: `task "two"`, Task: &graph.Task{Base: 0, Steps: []graph.Step{
			{Op: graph.OpAdd, Display: `add("one", "/in")`, From: from(1), Dest: "/in"},
		}, Output: &graph.Output{Source: "/out", Selection: sel}}},
		{Label: "app", Desc: `layer "app"`, Layer: &graph.Layer{Base: 0, Platform: "linux/amd64", Steps: []graph.Step{
			{Op: graph.OpAdd, Display: `add("two", "/data")`, From: from(2), Dest: "/data"},
		}}},
	}}
	testenv.Must(t, g.Validate())
	ev := &recordedEvents{}
	_, err := schedule(g, m, m, f, ev, false, time.Time{}).Run(t.Context())
	testenv.Must(t, err)
	oneKey, twoKey := f.runs[0], f.runs[1]

	// Both tasks' files are corrupt, with the right size, which only a read tells.
	for _, key := range []digest.Digest{oneKey, twoKey} {
		for _, b := range f.files[key] {
			m.Replace(b, []byte(strings.Repeat("x", int(b.Size))))
		}
	}
	// The fake finds a corrupt input as the real Runner would: when it adds the files.
	f.fail = func(req Request) error {
		for i, in := range req.Action.Steps {
			if in.Files == nil {
				continue
			}
			tree, err := cache.ReadTree(t.Context(), m, *in.Files)
			if err != nil {
				return &StepError{Step: i, Err: err}
			}
			for _, b := range tree.Files() {
				if _, err := m.Read(t.Context(), b, b.Size); err != nil {
					return &StepError{Step: i, Err: fmt.Errorf("copying sources from the build cache: f: %w", err)}
				}
			}
		}
		return nil
	}
	// Nothing reads the files until a target runs: the layer is rebuilt, and finds them.
	ev = &recordedEvents{}
	_, err = schedule(g, m, m, f, ev, true, time.Time{}).Run(t.Context())
	testenv.Must(t, err)
	var statuses []string
	for _, s := range ev.statuses {
		parts := strings.SplitN(s, ":", 3)
		statuses = append(statuses, parts[0]+":"+parts[1])
	}
	want := []string{"1: cached", "2: cached", "3: rebuilding",
		// The layer fails on task two's files, which are made again; that fails on task one's, made again first.
		"2: building again, since its cached files are corrupt",
		"1: building again, since its cached files are corrupt",
		"2: building again, since its cached files are corrupt",
		"3: rebuilding"}
	if !slices.Equal(statuses, want) {
		t.Errorf("statuses = %q, want %q", statuses, want)
	}
	for _, key := range []digest.Digest{oneKey, twoKey} {
		for _, b := range f.files[key] {
			if data, err := m.Read(t.Context(), b, b.Size); err != nil {
				t.Errorf("after regenerating, a file of %s = %q, %v; want it made again", key.Encoded()[:8], data, err)
			}
		}
	}

	// A task is regenerated once at most: a Runner that keeps finding corruption fails the job.
	f.fail = func(req Request) error {
		if req.Action.Kind == KindLayer {
			return &StepError{Step: 0, Err: cache.Corrupt(f.files[twoKey][0], "still")}
		}
		return nil
	}
	_, err = schedule(g, m, m, f, &recordedEvents{}, true, time.Time{}).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), `layer "app": add("two", "/data") failed: blob `) ||
		strings.Contains(err.Error(), "captured and submitted again") {
		t.Errorf("run = %v, want the layer's failure, without advice meant for a glob's files", err)
	}
}

// TestSchedulerCleanupFailureStopsRecovery checks that a corrupt input whose target's container couldn't be removed
// fails the job with both failures, rather than being regenerated, which would hide the cleanup failure on success.
func TestSchedulerCleanupFailureStopsRecovery(t *testing.T) {
	m, f := fakeFixture(t)
	sel := fileset.DefaultSelection()
	sel.Include = []string{"."}
	from := graph.Ref(1)
	g := &graph.Graph{Version: graph.Version, ExecPlatform: "linux/amd64", Roots: []graph.Ref{2}, Nodes: []graph.Node{
		{Label: "base", Desc: `pull "base"`, Pull: &graph.Pull{Image: "example.com/base:v1", Platform: "linux/amd64"}},
		{Label: "gen", Desc: `task "gen"`, Task: &graph.Task{Base: 0, Steps: []graph.Step{
			{Op: graph.OpRun, Display: `run(["gen"])`, Argv: []string{"gen"}},
		}, Output: &graph.Output{Source: "/out", Selection: sel}}},
		{Label: "app", Desc: `layer "app"`, Layer: &graph.Layer{Base: 0, Platform: "linux/amd64", Steps: []graph.Step{
			{Op: graph.OpAdd, Display: `add("gen", "/data")`, From: &from, Dest: "/data"},
		}}},
	}}
	testenv.Must(t, g.Validate())
	_, err := schedule(g, m, m, f, &recordedEvents{}, false, time.Time{}).Run(t.Context())
	testenv.Must(t, err)
	genKey := f.runs[0]

	// The layer finds the task's files corrupt once, and its container can't be removed; a retry would succeed.
	failed := false
	f.fail = func(req Request) error {
		if req.Action.Kind != KindLayer || failed {
			return nil
		}
		failed = true
		return &StepError{Step: 0, Err: cache.Corrupt(f.files[genKey][0], "it changed"),
			Cleanup: errors.New("device busy")}
	}
	runs := len(f.runs)
	_, err = schedule(g, m, m, f, &recordedEvents{}, true, time.Time{}).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), `layer "app": add("gen", "/data") failed: blob `) ||
		!strings.HasSuffix(err.Error(), "; removing its container: device busy") {
		t.Errorf("run = %v, want the layer's failure with the cleanup failure", err)
	}
	if got := f.runs[runs:]; len(got) != 1 || got[0] == genKey {
		t.Errorf("the Runner ran %d actions, want only the layer, with the task not regenerated", len(got))
	}
}

// TestSchedulerStoreUnavailable checks that a record store that can't be reached fails the job,
// rather than counting as a miss.
func TestSchedulerStoreUnavailable(t *testing.T) {
	m, f := fakeFixture(t)
	g := &graph.Graph{Version: graph.Version, ExecPlatform: "linux/amd64", Roots: []graph.Ref{1}, Nodes: []graph.Node{
		{Label: "base", Desc: `pull "base"`, Pull: &graph.Pull{Image: "example.com/base:v1", Platform: "linux/amd64"}},
		{Label: "app", Desc: `layer "app"`, Layer: &graph.Layer{Base: 0, Platform: "linux/amd64"}},
	}}
	testenv.Must(t, g.Validate())
	broken := &brokenRecords{RecordStore: m, err: errors.New("disk on fire")}
	_, err := schedule(g, m, broken, f, &recordedEvents{}, false, time.Time{}).Run(t.Context())
	if want := `layer "app": reading the build cache: disk on fire`; err == nil || err.Error() != want {
		t.Errorf("run = %v, want %q", err, want)
	}
	if len(f.runs) != 0 {
		t.Errorf("the Runner ran %d actions, want none", len(f.runs))
	}
}

// TestSchedulerContentUnavailable checks that a content store that can't be reached while a cached result is
// checked or its log read fails the job, rather than making the result unusable and running the target again.
func TestSchedulerContentUnavailable(t *testing.T) {
	sel := fileset.DefaultSelection()
	sel.Include = []string{"."}
	g := &graph.Graph{Version: graph.Version, ExecPlatform: "linux/amd64", Roots: []graph.Ref{1}, Nodes: []graph.Node{
		{Label: "base", Desc: `pull "base"`, Pull: &graph.Pull{Image: "example.com/base:v1", Platform: "linux/amd64"}},
		{Label: "gen", Desc: `task "gen"`, Task: &graph.Task{Base: 0, Steps: []graph.Step{
			{Op: graph.OpRun, Display: `run(["gen"])`, Argv: []string{"gen"}},
		}, Output: &graph.Output{Source: "/out", Selection: sel}}},
	}}
	testenv.Must(t, g.Validate())
	for _, tc := range []struct {
		name string
		// runnerBroken breaks the store the Runner checks the task's files in; otherwise the Scheduler's,
		// from which it reads the task's log once the Runner finds its files present.
		runnerBroken bool
	}{
		{
			name:         "files",
			runnerBroken: true,
		},
		{
			name: "log",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f := fakeFixture(t)
			_, err := schedule(g, m, m, f, &recordedEvents{}, false, time.Time{}).Run(t.Context())
			testenv.Must(t, err)
			runs := len(f.runs)
			fire := errors.New("disk on fire")
			broken := &brokenContent{ContentStore: m, err: fire}
			var content cache.ContentStore = m
			if tc.runnerBroken {
				f.content = broken
			} else {
				content = broken
			}
			_, err = schedule(g, content, m, f, &recordedEvents{}, false, time.Time{}).Run(t.Context())
			if want := `task "gen": reading the build cache: `; !errors.Is(err, fire) ||
				!strings.HasPrefix(err.Error(), want) {
				t.Errorf("run = %v, want %q and the store's error", err, want)
			}
			if len(f.runs) != runs {
				t.Errorf("the Runner ran %d actions, want none", len(f.runs)-runs)
			}
		})
	}
}

// brokenContent is a content store that can't be reached: nothing in it can be checked or read.
type brokenContent struct {
	cache.ContentStore
	err error
}

// Has fails.
func (b *brokenContent) Has(context.Context, cache.Blob) error {
	return b.err
}

// Get fails.
func (b *brokenContent) Get(context.Context, cache.Blob) (io.ReadCloser, error) {
	return nil, b.err
}

// Read fails.
func (b *brokenContent) Read(context.Context, cache.Blob, int64) ([]byte, error) {
	return nil, b.err
}

// brokenRecords is a record store whose results can't be read.
type brokenRecords struct {
	cache.RecordStore
	err error
}

// Result fails.
func (b *brokenRecords) Result(context.Context, digest.Digest) (*cache.ActionResult, error) {
	return nil, b.err
}

// TestSchedulerIndexMismatch checks that an index whose member's image isn't for the platform asked for is refused,
// naming both, and that a resolve that reports no platform fails the pull.
func TestSchedulerIndexMismatch(t *testing.T) {
	m, f := fakeFixture(t)
	g := multiPlatformGraph(t)
	// The fake makes images for the platform asked; make the second layer's image lie about its platform.
	lying := &lyingRunner{fakeRunner: f, platform: "linux/s390x"}
	_, err := schedule(g, m, m, lying, &recordedEvents{}, false, time.Time{}).Run(t.Context())
	if want := `index "all": the image of app is for linux/s390x, not linux/arm64`; err == nil || err.Error() != want {
		t.Errorf("run = %v, want %q", err, want)
	}

	// On stores of its own: the tag resolved above is recorded, and would need no resolution.
	m, f = fakeFixture(t)
	empty := &lyingRunner{fakeRunner: f, platform: ""}
	_, err = schedule(g, m, m, empty, &recordedEvents{}, false, time.Time{}).Run(t.Context())
	want := `pull "base" (linux/amd64): the resolution of example.com/base:v1 reported no image, platform or index`
	if err == nil || err.Error() != want {
		t.Errorf("run = %v, want %q", err, want)
	}
}

// lyingRunner is a fake Runner whose results for linux/arm64 claim another platform,
// or whose resolves claim none if platform is "".
type lyingRunner struct {
	*fakeRunner
	platform string
}

// Run runs through the fake, then rewrites the platform of what is made.
func (l *lyingRunner) Run(ctx context.Context, req Request, ev Output) (*Report, error) {
	rep, err := l.fakeRunner.Run(ctx, req, ev)
	if err != nil {
		return nil, err
	}
	if l.platform == "" && req.Action.Kind == KindResolve {
		rep.Result.Platform = ""
	} else if l.platform != "" && req.Action.Kind == KindLayer && req.Action.Platform == "linux/arm64" {
		rep.Result.Platform = l.platform
	}
	return rep, nil
}

// TestSchedulerCachedResolve checks that a pull whose tag record holds a complete instance for its platform,
// whose image the Checker finds present, asks the Runner nothing; that one whose image is gone resolves again,
// without reaching the registry when the record serves; and that an incomplete instance, as records written before
// hold, is completed by a resolution.
func TestSchedulerCachedResolve(t *testing.T) {
	m, f := fakeFixture(t)
	g := multiPlatformGraph(t)
	testenv.Must(t, must2(schedule(g, m, m, f, &recordedEvents{}, false, time.Time{}).Run(t.Context())))
	if len(f.resolves) != 2 {
		t.Fatalf("the first run resolved %d times, want 2", len(f.resolves))
	}
	record, err := m.Tag(t.Context(), "example.com/base:v1")
	testenv.Must(t, err)
	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		if i := record.Instances[platform]; !i.Complete() || i.Image != imageOf(record.Digest, platform) {
			t.Errorf("the instance for %s = %+v, want it complete, with the image resolved", platform, i)
		}
	}

	f.resolves = nil
	testenv.Must(t, must2(schedule(g, m, m, f, &recordedEvents{}, false, time.Time{}).Run(t.Context())))
	if len(f.resolves) != 0 {
		t.Errorf("a run with the tag recorded resolved %d times, want none", len(f.resolves))
	}

	// The image for one platform is gone: that pull resolves again, as the record says, not afresh.
	delete(f.images, imageOf(record.Digest, "linux/arm64"))
	f.registry["example.com/base:v1"] = digest.FromString("index 2") // which a fresh resolution would find
	testenv.Must(t, must2(schedule(g, m, m, f, &recordedEvents{}, false, time.Time{}).Run(t.Context())))
	if len(f.resolves) != 1 || f.resolves[0].Action.Platform != "linux/arm64" || f.resolves[0].Again ||
		f.resolves[0].Tag == nil || f.resolves[0].Tag.Digest != record.Digest {
		t.Errorf("resolves after an image went missing = %+v, want one, for linux/arm64, given the record", f.resolves)
	}
	if again, err := m.Tag(t.Context(), "example.com/base:v1"); err != nil || again.Digest != record.Digest ||
		!again.Instances["linux/amd64"].Complete() {
		t.Errorf("tag record after resolving again = %+v, %v; want the same index, and the other instance kept",
			again, err)
	}

	// An instance that records only its manifest's digest is completed, and then serves.
	legacy := &cache.TagRecord{Digest: record.Digest, Instances: map[string]cache.TagInstance{
		"linux/amd64": {Digest: record.Instances["linux/amd64"].Digest},
	}}
	testenv.Must(t, m.SetTag(t.Context(), "example.com/base:v1", legacy))
	f.resolves = nil
	testenv.Must(t, must2(schedule(g, m, m, f, &recordedEvents{}, false, time.Time{}).Run(t.Context())))
	if len(f.resolves) != 2 {
		t.Errorf("a run with incomplete instances resolved %d times, want 2", len(f.resolves))
	}
	completed, err := m.Tag(t.Context(), "example.com/base:v1")
	testenv.Must(t, err)
	if completed.Digest != record.Digest || !completed.Instances["linux/amd64"].Complete() ||
		!completed.Instances["linux/arm64"].Complete() {
		t.Errorf("tag record after completing = %+v, want the recorded index with both instances complete", completed)
	}
}

// must2 returns the error of a call that returns a value and an error.
func must2[T any](_ T, err error) error {
	return err
}

// parallelGraph returns a graph of a pull, two independent tasks on it, a layer that adds both tasks' files,
// and a third task that nothing needs, a root too: nodes 0 the pull, 1 and 2 the tasks, 3 the layer, 4 the task.
func parallelGraph(t *testing.T) *graph.Graph {
	t.Helper()
	sel := fileset.DefaultSelection()
	sel.Include = []string{"."}
	from := func(ref graph.Ref) *graph.Ref { return &ref }
	g := &graph.Graph{Version: graph.Version, ExecPlatform: "linux/amd64", Roots: []graph.Ref{3, 4}}
	g.Nodes = []graph.Node{
		{Label: "base", Desc: `pull "base"`, Pull: &graph.Pull{Image: "example.com/base:v1", Platform: "linux/amd64"}},
		{Label: "one", Desc: `task "one"`, Task: &graph.Task{Base: 0, Steps: []graph.Step{
			{Op: graph.OpRun, Display: `run(["one"])`, Argv: []string{"one"}},
		}, Output: &graph.Output{Source: "/out", Selection: sel}}},
		{Label: "two", Desc: `task "two"`, Task: &graph.Task{Base: 0, Steps: []graph.Step{
			{Op: graph.OpRun, Display: `run(["two"])`, Argv: []string{"two"}},
		}, Output: &graph.Output{Source: "/out", Selection: sel}}},
		{Label: "app", Desc: `layer "app"`, Layer: &graph.Layer{Base: 0, Platform: "linux/amd64", Steps: []graph.Step{
			{Op: graph.OpAdd, Display: `add("one", "/one")`, From: from(1), Dest: "/one"},
			{Op: graph.OpAdd, Display: `add("two", "/two")`, From: from(2), Dest: "/two"},
		}}},
		{Label: "three", Desc: `task "three"`, Task: &graph.Task{Base: 0, Steps: []graph.Step{
			{Op: graph.OpRun, Display: `run(["three"])`, Argv: []string{"three"}},
		}, Output: &graph.Output{Source: "/out", Selection: sel}}},
	}
	testenv.Must(t, g.Validate())
	return g
}

// gatedRunner is a fake Runner whose layers and tasks wait, once started, to be released one at a time,
// so that a test sees which run at once.
type gatedRunner struct {
	*fakeRunner
	mu      sync.Mutex
	started []digest.Digest // the keys of the runs started, in order
	gates   map[digest.Digest]chan struct{}
	waiting chan struct{} // receives once for each run that starts waiting
}

// newGatedRunner returns a gated Runner over f.
func newGatedRunner(f *fakeRunner) *gatedRunner {
	return &gatedRunner{fakeRunner: f, gates: map[digest.Digest]chan struct{}{}, waiting: make(chan struct{}, 16)}
}

// gate returns the channel that the run of key waits on.
func (r *gatedRunner) gate(key digest.Digest) chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gates[key] == nil {
		r.gates[key] = make(chan struct{})
	}
	return r.gates[key]
}

// Run waits for the run of its key to be released, unless it is a resolve, then runs through the fake.
func (r *gatedRunner) Run(ctx context.Context, req Request, ev Output) (*Report, error) {
	if req.Action.Kind != KindResolve {
		r.mu.Lock()
		r.started = append(r.started, req.Key)
		r.mu.Unlock()
		r.waiting <- struct{}{}
		select {
		case <-r.gate(req.Key):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock() // the fake's bookkeeping isn't safe for runs at once
	return r.fakeRunner.Run(ctx, req, ev)
}

// startedKeys returns the keys of the runs started so far.
func (r *gatedRunner) startedKeys() []digest.Digest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.started)
}

// TestSchedulerParallel checks that with a bound of two, two independent tasks run at once, and the layer that
// adds both runs once they are done; and that with a bound of one, targets run one at a time, in node order.
func TestSchedulerParallel(t *testing.T) {
	g := parallelGraph(t)
	for _, tc := range []struct {
		name  string
		bound int
	}{
		{name: "bound 2", bound: 2},
		{name: "bound 1", bound: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f := fakeFixture(t)
			r := newGatedRunner(f)
			b := Backend{Content: m, Records: m, Checker: f, Runner: NewSharing(r, m, m)}
			done := make(chan error, 1)
			go func() {
				_, err := New(g, b, &recordedEvents{}, Options{Bound: tc.bound}).Run(t.Context())
				done <- err
			}()
			for range tc.bound {
				<-r.waiting
			}
			// The pull is resolved and recorded by now, so the tasks' keys can be computed from the record;
			// the layer's depends on the tasks' trees, so it is known only once it starts.
			keys := map[string]digest.Digest{}
			for i, name := range []string{"", "one", "two", "", "three"} {
				if name != "" {
					keys[name] = recordedKey(g, m, f, graph.Ref(i))
				}
			}
			started := r.startedKeys()
			if len(started) != tc.bound {
				t.Fatalf("%d runs started, want %d, the bound", len(started), tc.bound)
			}
			if tc.bound == 2 && !(slices.Contains(started, keys["one"]) && slices.Contains(started, keys["two"])) {
				t.Errorf("the runs started are %v, want both tasks the layer needs, the lowest nodes ready", started)
			}
			// Release each run as it starts; the layer may start only once both its tasks have been released.
			released := map[digest.Digest]bool{}
			var order []digest.Digest
			for len(order) < 4 {
				if len(released) == len(r.startedKeys()) {
					<-r.waiting
				}
				for _, key := range r.startedKeys() {
					if !released[key] {
						if key != keys["one"] && key != keys["two"] && key != keys["three"] {
							keys["app"] = key
							if !released[keys["one"]] || !released[keys["two"]] {
								t.Error("the layer started before both tasks it adds were done")
							}
						}
						released[key] = true
						order = append(order, key)
						close(r.gate(key))
						break
					}
				}
			}
			testenv.Must(t, <-done)
			nodeOrder := []digest.Digest{keys["one"], keys["two"], keys["app"], keys["three"]}
			if tc.bound == 1 && !slices.Equal(order, nodeOrder) {
				t.Errorf("runs started in the order %v, want node order", order)
			}
			if len(f.runs) != 4 {
				t.Errorf("the Runner ran %d actions, want 4", len(f.runs))
			}
		})
	}
}

// handledLog keeps the nodes whose completions a scheduler's walk has handled, in order.
type handledLog struct {
	mu   sync.Mutex
	refs []graph.Ref
}

// handled keeps c's node; it is the scheduler's handled hook.
func (h *handledLog) handled(c completion) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.refs = append(h.refs, c.ref)
}

// has reports whether a completion of ref has been handled.
func (h *handledLog) has(ref graph.Ref) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Contains(h.refs, ref)
}

// count returns how many completions of ref have been handled.
func (h *handledLog) count(ref graph.Ref) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.refs {
		if r == ref {
			n++
		}
	}
	return n
}

// TestSchedulerParallelFailure checks that once a target fails while another runs, nothing more is dispatched,
// though a target is ready, the running target finishes and its result is recorded, so that the rerun reuses it,
// and the job fails with the first failure, whatever fails after.
func TestSchedulerParallelFailure(t *testing.T) {
	g := parallelGraph(t)
	for _, tc := range []struct {
		name     string
		twoFails bool
	}{
		{name: "sibling succeeds"},
		{name: "sibling fails too", twoFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f := fakeFixture(t)
			r := newGatedRunner(f)
			b := Backend{Content: m, Records: m, Checker: f, Runner: NewSharing(r, m, m)}
			f.fail = func(req Request) error {
				switch name := req.Action.Steps[0].Argv[0]; {
				case name == "one", name == "two" && tc.twoFails:
					return &StepError{Step: 0, Err: errors.New(name + " exited 1")}
				}
				return nil
			}
			done := make(chan error, 1)
			s := New(g, b, &recordedEvents{}, Options{Bound: 2})
			h := &handledLog{}
			s.handled = h.handled
			go func() {
				_, err := s.Run(t.Context())
				done <- err
			}()
			<-r.waiting
			<-r.waiting
			oneKey, twoKey := recordedKey(g, m, f, 1), recordedKey(g, m, f, 2)
			close(r.gate(oneKey))
			// The failure is known once the Scheduler handles it; task three is ready, and must not start.
			waitFor(t, func() bool { return h.has(1) })
			close(r.gate(twoKey))
			err := <-done
			if want := `task "one": run(["one"]) failed: one exited 1`; err == nil || err.Error() != want {
				t.Errorf("run = %v, want %q, the first failure", err, want)
			}
			if started := r.startedKeys(); len(started) != 2 {
				t.Errorf("%d runs started, want only the two tasks: nothing is dispatched after a failure",
					len(started))
			}
			if record, err := m.Result(t.Context(), twoKey); (record != nil) == tc.twoFails || err != nil {
				t.Errorf("the sibling's result = %v, %v; want it recorded if and only if it succeeded", record, err)
			}
			if tc.twoFails {
				return
			}

			// The rerun reuses the sibling, and fails again on the task; task three, never run, runs freely.
			close(r.gate(recordedKey(g, m, f, 4)))
			ev := &recordedEvents{}
			go func() {
				_, err := New(g, b, ev, Options{Bound: 2}).Run(t.Context())
				done <- err
			}()
			if err := <-done; err == nil {
				t.Error("the rerun succeeded")
			}
			if !slices.Contains(ev.statuses, "2: cached") || !slices.Contains(ev.statuses, "1: building") {
				t.Errorf("statuses of the rerun = %q, want the sibling cached and the task building", ev.statuses)
			}
		})
	}
}

// TestSchedulerParallelCancel checks that a job cancelled while targets run starts nothing more, waits for them,
// and fails as cancelled, naming a node.
func TestSchedulerParallelCancel(t *testing.T) {
	g := parallelGraph(t)
	m, f := fakeFixture(t)
	r := newGatedRunner(f)
	b := Backend{Content: m, Records: m, Checker: f, Runner: NewSharing(r, m, m)}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := New(g, b, &recordedEvents{}, Options{Bound: 2}).Run(ctx)
		done <- err
	}()
	<-r.waiting
	<-r.waiting
	cancel()
	err := <-done
	var f2 *failure
	if !errors.Is(err, context.Canceled) || !errors.As(err, &f2) || len(f2.Nodes) != 1 {
		t.Errorf("run = %v, want the cancellation as a failure naming a node", err)
	}
	if started := r.startedKeys(); len(started) != 2 {
		t.Errorf("%d runs started, want only the two running when the job was cancelled", len(started))
	}
	if len(f.runs) != 0 {
		t.Errorf("the Runner finished %d actions, want none, cancelled", len(f.runs))
	}
}

// serialRunner is a fake Runner safe for runs at once, which the fake's bookkeeping isn't.
type serialRunner struct {
	*fakeRunner
	mu sync.Mutex
}

// Run runs through the fake, one run at a time.
func (r *serialRunner) Run(ctx context.Context, req Request, ev Output) (*Report, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fakeRunner.Run(ctx, req, ev)
}

// TestSchedulerParallelRegenerates checks that with a bound of two, a layer that finds both tasks' files corrupt
// has each task run again once, and succeeds once they are made again.
func TestSchedulerParallelRegenerates(t *testing.T) {
	g := parallelGraph(t)
	m, f := fakeFixture(t)
	r := &serialRunner{fakeRunner: f}
	b := Backend{Content: m, Records: m, Checker: f, Runner: NewSharing(r, m, m)}
	_, err := New(g, b, &recordedEvents{}, Options{Bound: 2}).Run(t.Context())
	testenv.Must(t, err)
	oneKey, twoKey := recordedKey(g, m, f, 1), recordedKey(g, m, f, 2)
	for _, key := range []digest.Digest{oneKey, twoKey} {
		for _, blob := range f.files[key] {
			m.Replace(blob, []byte(strings.Repeat("x", int(blob.Size))))
		}
	}
	// The fake finds a corrupt input as the real Runner would: when it adds the files.
	f.fail = func(req Request) error {
		for i, in := range req.Action.Steps {
			if in.Files == nil {
				continue
			}
			tree, err := cache.ReadTree(t.Context(), m, *in.Files)
			if err != nil {
				return &StepError{Step: i, Err: err}
			}
			for _, blob := range tree.Files() {
				if _, err := m.Read(t.Context(), blob, blob.Size); err != nil {
					return &StepError{Step: i, Err: err}
				}
			}
		}
		return nil
	}
	runs := len(f.runs)
	ev := &recordedEvents{}
	_, err = New(g, b, ev, Options{Bound: 2, Rebuild: true}).Run(t.Context())
	testenv.Must(t, err)
	// Both tasks are made again once each, and the layer runs until it finds no corruption: three times at most.
	count := map[digest.Digest]int{}
	for _, key := range f.runs[runs:] {
		count[key]++
	}
	if count[oneKey] != 1 || count[twoKey] != 1 {
		t.Errorf("the tasks ran %d and %d times after their files were found corrupt, want once each",
			count[oneKey], count[twoKey])
	}
	for _, key := range []digest.Digest{oneKey, twoKey} {
		for _, blob := range f.files[key] {
			if _, err := m.Read(t.Context(), blob, blob.Size); err != nil {
				t.Errorf("after regenerating, a file of %s: %v; want it made again", key.Encoded()[:8], err)
			}
		}
	}
}

// TestSchedulerParallelRegeneratesStale checks that two targets running at once that both find a task's files corrupt
// have the task made again once, whether the second failure, stale since its target started before the task was
// made again, arrives while the task is made again or after, and that both targets succeed when tried again.
func TestSchedulerParallelRegeneratesStale(t *testing.T) {
	sel := fileset.DefaultSelection()
	sel.Include = []string{"."}
	from := func(ref graph.Ref) *graph.Ref { return &ref }
	g := &graph.Graph{Version: graph.Version, ExecPlatform: "linux/amd64", Roots: []graph.Ref{2, 3}}
	g.Nodes = []graph.Node{
		{Label: "base", Desc: `pull "base"`, Pull: &graph.Pull{Image: "example.com/base:v1", Platform: "linux/amd64"}},
		{Label: "one", Desc: `task "one"`, Task: &graph.Task{Base: 0, Steps: []graph.Step{
			{Op: graph.OpRun, Display: `run(["one"])`, Argv: []string{"one"}},
		}, Output: &graph.Output{Source: "/out", Selection: sel}}},
		{Label: "a", Desc: `layer "a"`, Layer: &graph.Layer{Base: 0, Platform: "linux/amd64", Steps: []graph.Step{
			{Op: graph.OpAdd, Display: `add("one", "/a")`, From: from(1), Dest: "/a"},
		}}},
		{Label: "b", Desc: `layer "b"`, Layer: &graph.Layer{Base: 0, Platform: "linux/amd64", Steps: []graph.Step{
			{Op: graph.OpAdd, Display: `add("one", "/b")`, From: from(1), Dest: "/b"},
		}}},
	}
	testenv.Must(t, g.Validate())
	for _, tc := range []struct {
		name  string
		after bool
	}{
		{name: "during regeneration"},
		{name: "after regeneration", after: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f := fakeFixture(t)
			_, err := schedule(g, m, m, f, &recordedEvents{}, false, time.Time{}).Run(t.Context())
			testenv.Must(t, err)
			oneKey := recordedKey(g, m, f, 1)
			for _, blob := range f.files[oneKey] {
				m.Replace(blob, []byte(strings.Repeat("x", int(blob.Size))))
			}
			// The error is made rather than read: a store that has read a corrupt blob no longer has it.
			corruptErr := cache.Corrupt(f.files[oneKey][0], "its content doesn't match its digest")
			// Each layer's first attempt read the files before they were made again, and so finds them corrupt.
			attempts := map[digest.Digest]int{}
			f.fail = func(req Request) error {
				if attempts[req.Key]++; req.Action.Kind == KindLayer && attempts[req.Key] == 1 {
					return &StepError{Step: 0, Err: corruptErr}
				}
				return nil
			}
			runs := len(f.runs)
			r := newGatedRunner(f)
			b := Backend{Content: m, Records: m, Checker: f, Runner: NewSharing(r, m, m)}
			s := New(g, b, &recordedEvents{}, Options{Bound: 2, Rebuild: true})
			h := &handledLog{}
			s.handled = h.handled
			done := make(chan error, 1)
			go func() {
				_, err := s.Run(t.Context())
				done <- err
			}()
			<-r.waiting
			<-r.waiting
			started := r.startedKeys()
			close(r.gate(started[0]))
			<-r.waiting // the task, made again
			if tc.after {
				close(r.gate(oneKey))
				waitFor(t, func() bool { return h.count(1) == 2 }) // cached, then made again
				close(r.gate(started[1]))
			} else {
				close(r.gate(started[1]))
				waitFor(t, func() bool { return h.has(2) && h.has(3) })
				if h.count(1) != 1 {
					t.Fatal("the task was made again before the second failure was handled")
				}
				close(r.gate(oneKey))
			}
			testenv.Must(t, <-done)
			count := map[digest.Digest]int{}
			for _, key := range f.runs[runs:] {
				count[key]++
			}
			if count[oneKey] != 1 || count[started[0]] != 2 || count[started[1]] != 2 {
				t.Errorf("the task ran %d times and the layers %d and %d, want once and twice each",
					count[oneKey], count[started[0]], count[started[1]])
			}
		})
	}
}

// movingRecords is a record store whose tag records move to another index from the second read on,
// as another job's --rebuild between two pulls of one tag would move them.
type movingRecords struct {
	cache.RecordStore
	to    digest.Digest
	reads int
}

// Tag returns the record as the store has it the first time, and with the digest moved after that.
func (r *movingRecords) Tag(ctx context.Context, name string) (*cache.TagRecord, error) {
	record, err := r.RecordStore.Tag(ctx, name)
	r.reads++
	if err != nil || record == nil || r.reads == 1 {
		return record, err
	}
	moved := *record
	moved.Digest = r.to
	moved.Instances = map[string]cache.TagInstance{}
	for platform, i := range record.Instances {
		moved.Instances[platform] = i // complete, for another index: not what the pin asks
	}
	return &moved, nil
}

// TestSchedulerResolvePinnedRecordMoved checks that a pull whose tag's record has moved to another index than the
// one pinned earlier in the job doesn't use the record's instance, though complete, but resolves the pinned index
// through the Runner, which is given the record.
func TestSchedulerResolvePinnedRecordMoved(t *testing.T) {
	m, f := fakeFixture(t)
	g := multiPlatformGraph(t)
	first := f.registry["example.com/base:v1"]
	testenv.Must(t, must2(schedule(g, m, m, f, &recordedEvents{}, false, time.Time{}).Run(t.Context())))
	f.resolves = nil
	second := digest.FromString("index 2")
	moving := &movingRecords{RecordStore: m, to: second}
	b := Backend{Content: m, Records: moving, Checker: f, Runner: NewSharing(f, m, moving)}
	s := New(g, b, &recordedEvents{}, Options{Bound: 1})
	testenv.Must(t, must2(s.Run(t.Context())))
	if len(f.resolves) != 1 || f.resolves[0].Action.Platform != "linux/arm64" || f.resolves[0].Action.Index != first ||
		f.resolves[0].Tag == nil || f.resolves[0].Tag.Digest != second {
		t.Errorf("resolves = %+v; want one, for the second platform, pinned to the first index and given the moved "+
			"record",
			f.resolves)
	}
	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		if got, want := s.images[graph.Ref(slices.Index([]string{"linux/amd64", "linux/arm64"}, platform))].id,
			imageOf(first, platform).Encoded(); got != want {
			t.Errorf("the base for %s is %s, want %s, from the pinned index", platform, got, want)
		}
	}
}

// TestSchedulerEmulation checks that, where the executor requires permission for emulation, a layer or task whose
// run() steps are for another platform than the execution platform fails before running, naming both platforms
// and both ways out, unless the build permits it; without the requirement, as locally, it runs.
func TestSchedulerEmulation(t *testing.T) {
	// The execution platform is linux/amd64; the arm64 layer runs a command, the amd64 one doesn't need permission,
	// and a layer on Scratch that only adds files never does.
	g := multiPlatformGraph(t)
	g.Roots = []graph.Ref{2, 3}
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{
			name: "refused",
			opts: Options{OptIn: true},
			want: `layer "app" (linux/arm64): its commands would run under emulation: its image is for linux/arm64, ` +
				"and this build runs commands on linux/amd64; give --platform linux/amd64 to build for the platform " +
				"that runs them, or --emulate to allow emulation",
		},
		{
			name: "permitted",
			opts: Options{OptIn: true, Emulate: true},
		},
		{
			name: "not required",
			opts: Options{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f := fakeFixture(t)
			b := Backend{Content: m, Records: m, Checker: f, Runner: NewSharing(f, m, m)}
			_, err := New(g, b, &recordedEvents{}, tc.opts).Run(t.Context())
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("run = %v, want success", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Errorf("run = %v, want %q", err, tc.want)
			}
			if tc.want != "" && slices.Contains(f.runs, recordedKey(g, m, f, 3)) {
				t.Error("the layer ran though its emulation was refused")
			}
		})
	}
}

// recordedKey returns the key of the layer or task ref of g, whose pulls resolved to what the stores record,
// and whose other inputs are its glob's files alone.
func recordedKey(g *graph.Graph, m *cache.Memory, f *fakeRunner, ref graph.Ref) digest.Digest {
	s := schedule(g, m, m, f, &recordedEvents{}, false, time.Time{})
	for i, n := range g.Nodes {
		if n.Pull != nil {
			record, _ := m.Tag(context.Background(), n.Pull.Image)
			if record != nil {
				s.images[graph.Ref(i)] = builtImage{id: record.Instances[n.Pull.Platform].Image.Encoded(),
					platform: n.Pull.Platform}
			}
		}
	}
	return s.inputs(g.Node(ref)).Key()
}

// TestSchedulerReuseWithoutOutput checks that a task without output, which makes neither an image nor files,
// is reused on a second run as any other, without a flow to trace.
func TestSchedulerReuseWithoutOutput(t *testing.T) {
	m, f := fakeFixture(t)
	g := &graph.Graph{Version: graph.Version, ExecPlatform: "linux/amd64", Roots: []graph.Ref{1}, Nodes: []graph.Node{
		{Label: "base", Desc: `pull "base"`, Pull: &graph.Pull{Image: "example.com/base:v1", Platform: "linux/amd64"}},
		{Label: "test", Desc: `task "test"`, Task: &graph.Task{Base: 0, Steps: []graph.Step{
			{Op: graph.OpRun, Display: `run(["test"])`, Argv: []string{"test"}},
		}}},
	}}
	testenv.Must(t, g.Validate())
	traced := tracing.WithRecorder(t.Context(), tracing.NewRecorder("executor", func(tracing.Span) {}), "")
	for i, want := range []string{"1: building", "1: cached"} {
		ev := &recordedEvents{}
		results, err := schedule(g, m, m, f, ev, false, time.Time{}).Run(traced)
		testenv.Must(t, err)
		if !slices.Equal(ev.statuses, []string{want}) {
			t.Errorf("statuses of run %d = %q, want %q", i, ev.statuses, []string{want})
		}
		if len(results) != 1 || results[0].Files != nil || results[0].Image != "" {
			t.Errorf("results of run %d = %+v, want one that made nothing", i, results)
		}
	}
	if len(f.runs) != 1 {
		t.Errorf("the Runner ran %d actions, want 1", len(f.runs))
	}
}

// BenchmarkSchedulerCached measures the walk of a large graph whose every target is cached: many pulls, and many
// tasks on them, each adding the files of the one before, so that scheduling, not running, is what is timed.
func BenchmarkSchedulerCached(b *testing.B) {
	const pulls, tasks = 50, 2000
	m, f := fakeFixture(b)
	sel := fileset.DefaultSelection()
	sel.Include = []string{"."}
	g := &graph.Graph{Version: graph.Version, ExecPlatform: "linux/amd64"}
	for i := range pulls {
		image := fmt.Sprintf("example.com/base%d:v1", i)
		f.registry[image] = digest.FromString(image)
		g.Nodes = append(g.Nodes, graph.Node{Label: "base", Desc: fmt.Sprintf(`pull "base%d"`, i),
			Pull: &graph.Pull{Image: image, Platform: "linux/amd64"}})
	}
	for i := range tasks {
		name := fmt.Sprintf("t%d", i)
		steps := []graph.Step{{Op: graph.OpRun, Display: `run(["` + name + `"])`, Argv: []string{name}}}
		if i > 0 {
			from := graph.Ref(pulls + i - 1)
			steps = append(steps, graph.Step{Op: graph.OpAdd, Display: `add("prev", "/prev")`, From: &from,
				Dest: "/prev"})
		}
		g.Nodes = append(g.Nodes, graph.Node{Label: name, Desc: `task "` + name + `"`, Task: &graph.Task{
			Base: graph.Ref(i % pulls), Steps: steps, Output: &graph.Output{Source: "/out", Selection: sel}}})
		g.Roots = append(g.Roots, graph.Ref(pulls+i))
	}
	if err := g.Validate(); err != nil {
		b.Fatal(err)
	}
	backend := Backend{Content: m, Records: m, Checker: f, Runner: NewSharing(f, m, m)}
	if _, err := New(g, backend, &recordedEvents{}, Options{Bound: 1}).Run(b.Context()); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, err := New(g, backend, &recordedEvents{}, Options{Bound: 8}).Run(b.Context()); err != nil {
			b.Fatal(err)
		}
	}
}

// storeLocation returns the location of the image with ID id in a local store, as the local runner records it.
func storeLocation(id string) cache.Location {
	return cache.Location{Kind: cache.LocationStore, Reference: "localhost/vamos-cache:" + id}
}
