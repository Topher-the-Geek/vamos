package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
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

// fakeRunner is a runner that makes nothing: it answers resolves from a registry of its own,
// gives layers images named by their keys, gives tasks an empty tree, and publishes results as a runner must.
// It never opens container storage.
type fakeRunner struct {
	content cache.ContentStore
	records cache.RecordStore
	// registry is what each tag names now: an index, whose images for each platform are named by the index.
	registry map[string]digest.Digest
	// moved, if set, is called after each resolve, so that a test can move a tag between resolutions.
	moved func()
	// fail, if set, is asked before each layer or task runs whether it should fail, and with what.
	fail     func(req actionRequest) error
	resolves []actionRequest // every resolve run, in order
	runs     []digest.Digest // the keys of the layers and tasks run, in order
	images   map[digest.Digest]bool
	files    map[digest.Digest][]cache.Blob // the files each task's tree holds, by task key
}

// imageOf returns the ID of the image in index for platform.
func imageOf(index digest.Digest, platform string) digest.Digest {
	return digest.FromString(index.String() + " " + platform)
}

// check reports a result unusable if its image isn't one the fake made, or its files are missing.
func (f *fakeRunner) check(ctx context.Context, a *action, result *cache.ActionResult) (error, error) {
	if a.Kind == kindLayer && !f.images[result.Image] {
		return fmt.Errorf("its image %s is gone", result.Image), nil
	}
	if result.Output != nil {
		if err := cache.CheckTree(ctx, f.content, *result.Output); err != nil {
			return storeProblem("its files can't be used", err)
		}
	}
	return nil, nil
}

// run resolves a tag, or makes a layer's image or a task's files, and publishes the result.
func (f *fakeRunner) run(ctx context.Context, req actionRequest, ev actionEvents) (*cache.ActionResult, error) {
	a := req.action
	if a.Kind == kindResolve {
		f.resolves = append(f.resolves, req)
		index := a.Index
		if index == "" && !req.again {
			if record, err := f.records.Tag(ctx, a.Image); err != nil {
				return nil, err
			} else if record != nil {
				index = record.Digest
			}
		}
		if index == "" {
			index = f.registry[a.Image]
			if index == "" {
				return nil, fmt.Errorf("image %q: not found", a.Image)
			}
		}
		if err := f.records.SetTag(ctx, a.Image, &cache.TagRecord{Digest: index}); err != nil {
			return nil, err
		}
		if f.moved != nil {
			f.moved()
		}
		id := imageOf(index, a.Platform)
		f.images[id] = true
		return &cache.ActionResult{Image: id, Platform: a.Platform, Index: index, Epoch: 1}, nil
	}

	f.runs = append(f.runs, req.key)
	if f.fail != nil {
		if err := f.fail(req); err != nil {
			return nil, err
		}
	}
	ev.Output(cache.StreamStdout, []byte("ran "+req.key.Encoded()[:8]+"\n"))
	log, err := cache.NewLog()
	if err != nil {
		return nil, err
	}
	defer log.Discard()
	log.Writer(cache.StreamStdout, io.Discard).Write([]byte("ran " + req.key.Encoded()[:8] + "\n"))
	result := &cache.ActionResult{Epoch: req.epoch.Unix()}
	if result.Log, err = log.Save(ctx, f.content); err != nil {
		return nil, err
	}
	switch a.Kind {
	case kindLayer:
		result.Image, result.Platform = digest.FromString("image for "+req.key.String()), a.Platform
		f.images[result.Image] = true
	case kindTask:
		if a.Output != nil {
			// Each task's tree holds two files, so that a test can corrupt them, written through req.rewrite's view.
			tree := &cache.Tree{}
			f.files[req.key] = nil
			for _, name := range []string{"f", "g"} {
				file, _, err := cache.PutFile(ctx, rewriting{ContentStore: f.content, corrupt: req.rewrite},
					memFile(name+" made by "+req.key.String()), "/f", 100)
				if err != nil {
					return nil, err
				}
				f.files[req.key] = append(f.files[req.key], file)
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
	return result, f.records.SetResult(ctx, req.key, result)
}

// memFile returns a filesystem holding the file /f with content.
func memFile(content string) afero.Fs {
	fsys := afero.NewMemMapFs()
	afero.WriteFile(fsys, "/f", []byte(content), 0o644)
	return fsys
}

// recordedEvents keeps the statuses and output a scheduler reports.
type recordedEvents struct {
	statuses []string
	output   []string
}

// Status keeps the status, by node.
func (e *recordedEvents) Status(node graph.Ref, status string) {
	e.statuses = append(e.statuses, fmt.Sprintf("%d: %s", node, status))
}

// Output keeps the output, by node and stream.
func (e *recordedEvents) Output(node graph.Ref, stream byte, data []byte) {
	e.output = append(e.output, fmt.Sprintf("%d/%d: %s", node, stream, data))
}

// Spans discards spans.
func (e *recordedEvents) Spans([]tracing.Span) {}

// fakeFixture returns stores in memory and a fake runner on them, whose registry has index for the tag "base:v1".
func fakeFixture(t *testing.T) (*cache.Memory, *fakeRunner) {
	t.Helper()
	m := cache.NewMemory()
	f := &fakeRunner{
		content:  m,
		records:  m,
		registry: map[string]digest.Digest{"example.com/base:v1": digest.FromString("index 1")},
		images:   map[digest.Digest]bool{},
		files:    map[digest.Digest][]cache.Blob{},
	}
	return m, f
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
	s := newScheduler(multiPlatformGraph(t), m, m, f, ev, false, time.Time{})
	results, err := s.run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.resolves) != 2 || f.resolves[0].action.Index != "" || f.resolves[1].action.Index != first {
		t.Errorf("resolves asked for indexes %q, %q; want none, then the first's", f.resolves[0].action.Index,
			f.resolves[1].action.Index)
	}
	imageFor := func(ref graph.Ref) string {
		return digest.FromString("image for " + s.inputs(s.graph.Node(ref)).key().String()).Encoded()
	}
	want := []api.Manifest{
		{Image: imageFor(2), Platform: "linux/amd64"},
		{Image: imageFor(3), Platform: "linux/arm64"},
	}
	if len(results) != 1 || !slices.Equal(results[0].Index, want) {
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
	s = newScheduler(g, m, m, f, ev, true, time.Time{})
	if _, err := s.run(t.Context()); err != nil {
		t.Fatal(err)
	}
	second := digest.FromString("index 2")
	if !f.resolves[0].again || f.resolves[1].again || f.resolves[1].action.Index != second {
		t.Errorf("resolves again = %v, %v with index %q; want the first afresh and the second pinned to %s",
			f.resolves[0].again, f.resolves[1].again, f.resolves[1].action.Index, second)
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
	if _, err := newScheduler(g, m, m, f, &recordedEvents{}, true, time.Time{}).run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.resolves) != 2 {
		t.Fatalf("%d resolves, want 2", len(f.resolves))
	}
	if first := f.resolves[0]; !first.again || !first.force {
		t.Errorf("the first pull resolved with again = %v, force = %v; want a forced fresh resolution",
			first.again, first.force)
	}
	if second := f.resolves[1]; second.again || second.action.Index != f.registry["example.com/base:v1"] {
		t.Errorf("the rebuilt pull resolved with again = %v and index %q; want it pinned to the fresh index",
			second.again, second.action.Index)
	}
}

// TestSchedulerReuse checks that the scheduler runs a layer and a task once, through the runner,
// and reuses their results on a second run, replaying their output, and that a result the runner can't use
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
		if _, err := newScheduler(g, m, m, f, ev, false, timestamp).run(t.Context()); err != nil {
			t.Fatal(err)
		}
		return ev
	}
	ev := run(time.Time{})
	if want := []string{"1: building", "2: building"}; !slices.Equal(ev.statuses, want) {
		t.Errorf("statuses = %q, want %q", ev.statuses, want)
	}
	if len(f.runs) != 2 {
		t.Fatalf("the runner ran %d actions, want 2", len(f.runs))
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
		t.Errorf("the runner ran %d actions after a cached run, want still 2", len(f.runs))
	}

	// The timestamp doesn't change a key, but a layer whose image is gone is built again, with it.
	f.images = map[digest.Digest]bool{}
	ev = run(time.Unix(1_700_000_000, 0))
	if want := "2: building, since the cached result can't be used: its image "; !slices.ContainsFunc(ev.statuses,
		func(s string) bool { return strings.HasPrefix(s, want) }) {
		t.Errorf("statuses = %q, want the layer built again", ev.statuses)
	}
	if len(f.runs) != 3 {
		t.Errorf("the runner ran %d actions, want 3", len(f.runs))
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
	failing := &failingRunner{err: &stepError{step: 0, err: cache.Corrupt(tree, "it isn't a tree")}}
	_, err = newScheduler(g, m, m, failing, &recordedEvents{}, false, time.Time{}).run(t.Context())
	want := `layer "app": add("src", "/data") failed: blob ` + tree.Digest.String() +
		" is corrupt: it isn't a tree; the project must be captured and submitted again"
	if err == nil || err.Error() != want {
		t.Errorf("run = %v, want %q", err, want)
	}
}

// failingRunner is a runner whose every action fails with err.
type failingRunner struct {
	err error
}

// check finds every result usable.
func (f *failingRunner) check(context.Context, *action, *cache.ActionResult) (error, error) {
	return nil, nil
}

// run fails.
func (f *failingRunner) run(context.Context, actionRequest, actionEvents) (*cache.ActionResult, error) {
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
	_, err := newScheduler(g, m, m, f, ev, false, time.Time{}).run(t.Context())
	testenv.Must(t, err)
	oneKey, twoKey := f.runs[0], f.runs[1]

	// Both tasks' files are corrupt, with the right size, which only a read tells.
	for _, key := range []digest.Digest{oneKey, twoKey} {
		for _, b := range f.files[key] {
			m.Replace(b, []byte(strings.Repeat("x", int(b.Size))))
		}
	}
	// The fake finds a corrupt input as the real runner would: when it adds the files.
	f.fail = func(req actionRequest) error {
		for i, in := range req.action.Steps {
			if in.Files == nil {
				continue
			}
			tree, err := cache.ReadTree(t.Context(), m, *in.Files)
			if err != nil {
				return &stepError{step: i, err: err}
			}
			for _, b := range tree.Files() {
				if _, err := m.Read(t.Context(), b, b.Size); err != nil {
					return &stepError{step: i, err: fmt.Errorf("copying sources from the build cache: f: %w", err)}
				}
			}
		}
		return nil
	}
	// Nothing reads the files until a target runs: the layer is rebuilt, and finds them.
	ev = &recordedEvents{}
	_, err = newScheduler(g, m, m, f, ev, true, time.Time{}).run(t.Context())
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

	// A task is regenerated once at most: a runner that keeps finding corruption fails the job.
	f.fail = func(req actionRequest) error {
		if req.action.Kind == kindLayer {
			return &stepError{step: 0, err: cache.Corrupt(f.files[twoKey][0], "still")}
		}
		return nil
	}
	_, err = newScheduler(g, m, m, f, &recordedEvents{}, true, time.Time{}).run(t.Context())
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
	_, err := newScheduler(g, m, m, f, &recordedEvents{}, false, time.Time{}).run(t.Context())
	testenv.Must(t, err)
	genKey := f.runs[0]

	// The layer finds the task's files corrupt once, and its container can't be removed; a retry would succeed.
	failed := false
	f.fail = func(req actionRequest) error {
		if req.action.Kind != kindLayer || failed {
			return nil
		}
		failed = true
		return &stepError{step: 0, err: cache.Corrupt(f.files[genKey][0], "it changed"),
			cleanup: errors.New("device busy")}
	}
	runs := len(f.runs)
	_, err = newScheduler(g, m, m, f, &recordedEvents{}, true, time.Time{}).run(t.Context())
	if err == nil || !strings.Contains(err.Error(), `layer "app": add("gen", "/data") failed: blob `) ||
		!strings.HasSuffix(err.Error(), "; removing its container: device busy") {
		t.Errorf("run = %v, want the layer's failure with the cleanup failure", err)
	}
	if got := f.runs[runs:]; len(got) != 1 || got[0] == genKey {
		t.Errorf("the runner ran %d actions, want only the layer, with the task not regenerated", len(got))
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
	_, err := newScheduler(g, m, broken, f, &recordedEvents{}, false, time.Time{}).run(t.Context())
	if want := `layer "app": reading the build cache: disk on fire`; err == nil || err.Error() != want {
		t.Errorf("run = %v, want %q", err, want)
	}
	if len(f.runs) != 0 {
		t.Errorf("the runner ran %d actions, want none", len(f.runs))
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
		// runnerBroken breaks the store the runner checks the task's files in; otherwise the scheduler's,
		// from which it reads the task's log once the runner finds its files present.
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
			_, err := newScheduler(g, m, m, f, &recordedEvents{}, false, time.Time{}).run(t.Context())
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
			_, err = newScheduler(g, content, m, f, &recordedEvents{}, false, time.Time{}).run(t.Context())
			if want := `task "gen": reading the build cache: `; !errors.Is(err, fire) ||
				!strings.HasPrefix(err.Error(), want) {
				t.Errorf("run = %v, want %q and the store's error", err, want)
			}
			if len(f.runs) != runs {
				t.Errorf("the runner ran %d actions, want none", len(f.runs)-runs)
			}
		})
	}
}

// TestRunnerCheckContent checks that the local runner's check tells a task's files that are corrupt,
// which make its result unusable, from a content store that can't be reached, which is an error.
func TestRunnerCheckContent(t *testing.T) {
	m := cache.NewMemory()
	a := &action{Version: actionVersion, Kind: kindTask, Output: fileset.DefaultSelection()}
	file, _, err := cache.PutFile(t.Context(), m, memFile("data"), "/f", 100)
	testenv.Must(t, err)
	tree, err := m.PutBytes(t.Context(), (&cache.Tree{Entries: []cache.TreeEntry{
		{Path: "f", Type: cache.EntryFile, Mode: 0o644, Blob: &file},
	}}).Encode())
	testenv.Must(t, err)
	result := &cache.ActionResult{Output: &tree}

	r := &localRunner{content: m}
	if problem, err := r.check(t.Context(), a, result); problem != nil || err != nil {
		t.Errorf("check = %v, %v; want the result usable", problem, err)
	}
	fire := errors.New("disk on fire")
	r.content = &brokenContent{ContentStore: m, err: fire}
	if problem, err := r.check(t.Context(), a, result); problem != nil || !errors.Is(err, fire) {
		t.Errorf("check with the store unreachable = %v, %v; want only the store's error", problem, err)
	}
	m.Replace(file, nil)
	r.content = m
	if problem, err := r.check(t.Context(), a, result); err != nil || problem == nil ||
		!strings.HasPrefix(problem.Error(), "its files can't be used from the cache: f: ") {
		t.Errorf("check with a file corrupt = %v, %v; want the result unusable", problem, err)
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
	_, err := newScheduler(g, m, m, lying, &recordedEvents{}, false, time.Time{}).run(t.Context())
	if want := `index "all": the image of app is for linux/s390x, not linux/arm64`; err == nil || err.Error() != want {
		t.Errorf("run = %v, want %q", err, want)
	}

	empty := &lyingRunner{fakeRunner: f, platform: ""}
	_, err = newScheduler(g, m, m, empty, &recordedEvents{}, false, time.Time{}).run(t.Context())
	want := `pull "base" (linux/amd64): the resolution of example.com/base:v1 reported no image, platform or index`
	if err == nil || err.Error() != want {
		t.Errorf("run = %v, want %q", err, want)
	}
}

// lyingRunner is a fake runner whose results for linux/arm64 claim another platform,
// or whose resolves claim none if platform is "".
type lyingRunner struct {
	*fakeRunner
	platform string
}

// run runs through the fake, then rewrites the platform of what is made.
func (l *lyingRunner) run(ctx context.Context, req actionRequest, ev actionEvents) (*cache.ActionResult, error) {
	result, err := l.fakeRunner.run(ctx, req, ev)
	if err != nil {
		return nil, err
	}
	if l.platform == "" && req.action.Kind == kindResolve {
		result.Platform = ""
	} else if l.platform != "" && req.action.Kind == kindLayer && req.action.Platform == "linux/arm64" {
		result.Platform = l.platform
	}
	return result, nil
}
