package cli

import (
	"cmp"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/tracing"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"go.podman.io/storage"
)

// traceEvent is an event of a trace file, as Perfetto reads it.
type traceEvent struct {
	Phase   string         `json:"ph"`
	Name    string         `json:"name"`
	Cat     string         `json:"cat"`
	ID      string         `json:"id"`
	PID     int            `json:"pid"`
	TID     int            `json:"tid"`
	TS      float64        `json:"ts"`
	Dur     float64        `json:"dur"`
	BindID  string         `json:"bind_id"`
	FlowIn  bool           `json:"flow_in"`
	FlowOut bool           `json:"flow_out"`
	Args    map[string]any `json:"args"`
}

// traceSlice is a span of a trace: a slice on a track, within its parent there, an async slice, or an instant.
type traceSlice struct {
	traceEvent
	process, track string
	end            float64
	parent         *traceSlice // the slice it is within, on its track
	children       []*traceSlice
}

// trace is a trace file, read as Perfetto would.
type trace struct {
	slices []*traceSlice    // in order of their start
	flows  [][2]*traceSlice // from what produced something to what used it
	events int              // other than metadata
}

// readTrace reads the trace file at path.
func readTrace(t *testing.T, path string) *trace {
	t.Helper()
	data, err := os.ReadFile(path)
	must(t, err)
	var file struct {
		TraceEvents []traceEvent `json:"traceEvents"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("reading trace: %v", err)
	}
	processes := map[int]string{}
	tracks := map[[2]int]string{}
	tr := &trace{}
	ends := map[string]float64{} // of async slices, by ID
	for _, e := range file.TraceEvents {
		switch e.Phase {
		case "M":
			switch e.Name {
			case "process_name":
				processes[e.PID] = e.Args["name"].(string)
			case "thread_name":
				tracks[[2]int{e.PID, e.TID}] = e.Args["name"].(string)
			}
			continue
		case "e":
			ends[e.ID] = e.TS
		case "X", "b", "i":
			tr.slices = append(tr.slices, &traceSlice{traceEvent: e, end: e.TS + e.Dur})
		default:
			t.Errorf("trace has an event of unexpected phase %q: %+v", e.Phase, e)
		}
		tr.events++
	}
	slices.SortStableFunc(tr.slices, func(a, b *traceSlice) int {
		return cmp.Or(cmp.Compare(a.TS, b.TS), cmp.Compare(b.Dur, a.Dur))
	})
	// Each slice's parent is the innermost slice on its track that it is within, as Perfetto nests them.
	open := map[[2]int][]*traceSlice{}
	outs := map[string]*traceSlice{}
	for _, s := range tr.slices {
		s.process, s.track = processes[s.PID], tracks[[2]int{s.PID, s.TID}]
		if s.Phase == "b" {
			end, ok := ends[s.ID]
			if !ok {
				t.Errorf("async slice %q has no end", s.Name)
			}
			s.end = end
			continue
		}
		key := [2]int{s.PID, s.TID}
		stack := open[key]
		// As Perfetto does, a slice that starts as another ends is after it, not within it.
		for len(stack) > 0 && stack[len(stack)-1].end <= s.TS {
			stack = stack[:len(stack)-1]
		}
		if len(stack) > 0 {
			p := stack[len(stack)-1]
			if s.end > p.end {
				t.Errorf("%q overlaps %q on track %q without being within it", s.Name, p.Name, s.track)
			}
			s.parent = p
			p.children = append(p.children, s)
		}
		if s.Phase == "X" {
			open[key] = append(stack, s)
		} else {
			open[key] = stack
		}
		if s.FlowOut {
			outs[s.BindID] = s
		}
	}
	for _, s := range tr.slices {
		if s.FlowIn {
			if out := outs[s.BindID]; out != nil {
				tr.flows = append(tr.flows, [2]*traceSlice{out, s})
			} else {
				t.Errorf("%q uses something no slice produced", s.Name)
			}
		}
	}
	return tr
}

// all returns the slices of process named name, or starting with name, if it ends in "*".
func (tr *trace) all(process, name string) []*traceSlice {
	var found []*traceSlice
	for _, s := range tr.slices {
		prefix, wild := strings.CutSuffix(name, "*")
		if s.process == process && (s.Name == name || wild && strings.HasPrefix(s.Name, prefix)) {
			found = append(found, s)
		}
	}
	return found
}

// one returns the only slice of process named name, failing the test unless there is exactly one.
func (tr *trace) one(t *testing.T, process, name string) *traceSlice {
	t.Helper()
	found := tr.all(process, name)
	if len(found) != 1 {
		t.Fatalf("trace has %d slices of %s named %q, want 1; it has:\n%s", len(found), process, name, tr)
	}
	return found[0]
}

// String lists the trace's slices, indented by depth, for messages.
func (tr *trace) String() string {
	var b strings.Builder
	for _, s := range tr.slices {
		depth := 0
		for p := s.parent; p != nil; p = p.parent {
			depth++
		}
		fmt.Fprintf(&b, "%s[%s] %s%s (%s)\n", s.process, s.track, strings.Repeat("  ", depth), s.Name, s.Phase)
	}
	return b.String()
}

// childNames returns the names of the slices directly within s.
func (s *traceSlice) childNames() []string {
	var names []string
	for _, c := range s.children {
		names = append(names, c.Name)
	}
	return names
}

// within reports whether s takes place within the time of other, by the trace's clock.
func (s *traceSlice) within(other *traceSlice) bool {
	return s.TS >= other.TS && s.end <= other.end
}

// arg returns s's argument key, as JSON decodes it, failing the test if it has none.
func (s *traceSlice) arg(t *testing.T, key string) any {
	t.Helper()
	v, ok := s.Args[key]
	if !ok {
		t.Fatalf("%q has no argument %q; it has %v", s.Name, key, s.Args)
	}
	return v
}

// traceBuild is a project whose layer adds a glob's files and a task's, with a value unique to the run,
// so that it is built rather than reused from the build cache.
func traceBuild(nonce string) map[string]string {
	return map[string]string{
		"BUILD": pullBusybox + `
glob(name="src", include=["src"])
task(
    name="gen",
    base="busybox",
    steps=[
        env(NONCE="` + nonce + `"),
        add("src", "/in"),
        run(["sh", "-c", "mkdir /out && cat /in/src/* > /out/all.txt"]),
    ],
    source="/out",
    include=["all.txt"],
)
layer(
    name="app",
    base="busybox",
    steps=[add("gen", "/data"), env(NONCE="` + nonce + `"), workdir("/data"), run(["cat", "all.txt"])],
    command=["cat", "/data/all.txt"],
)
`,
		"src/a.txt": "a\n",
		"src/b.txt": "bb\n",
	}
}

// TestTraceBuild checks the trace of a build: the client's load, holding its globs, the executor's run,
// and each target on its own track, holding its steps, as the BUILD file wrote them,
// in order, with arrows from what each target or glob made to the steps that use it.
// Built again, the targets are reused, which the trace says.
func TestTraceBuild(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, traceBuild(newNonce()))
	// Pull busybox first, so that the traced build finds it present even when this test runs alone.
	vamos(t, fsys, "build", "busybox", "-C", projectDir).ran(t)
	path := filepath.Join(t.TempDir(), "trace.json")
	vamos(t, fsys, "build", "app", "--trace", path, "-C", projectDir).ran(t)
	tr := readTrace(t, path)

	root := tr.one(t, "vamos", "vamos build app")
	if got, want := root.childNames(), []string{"load", "store graph", "run"}; !slices.Equal(got, want) {
		t.Errorf("the command's spans = %q, want %q", got, want)
	}
	glob := tr.one(t, "vamos", `glob "src"`)
	if glob.parent == nil || glob.parent.Name != "load" {
		t.Errorf("the glob isn't within load:\n%s", tr)
	}
	if got := glob.arg(t, "files"); got != float64(2) {
		t.Errorf("the glob's files = %v, want 2", got)
	}
	if got := glob.arg(t, "bytes"); got != float64(5) {
		t.Errorf("the glob's bytes = %v, want 5", got)
	}
	// The glob's tree is the one the graph names.
	r := vamos(t, fsys, "graph", "app", "-C", projectDir).ran(t)
	g, err := graph.Decode([]byte(r.stdout))
	must(t, err)
	for _, n := range g.Nodes {
		if n.Source != nil && glob.arg(t, "tree") != n.Source.Tree.Digest.String() {
			t.Errorf("the glob's tree = %v, want %s, as the graph has it", glob.Args["tree"], n.Source.Tree.Digest)
		}
	}

	run := tr.one(t, "executor", "run")
	if clientRun := tr.one(t, "vamos", "run"); !run.within(clientRun) {
		t.Error("the executor's run isn't within the client's call to it")
	}
	pull := tr.one(t, "executor", `pull "busybox"`)
	if pull.track != `pull "busybox"` {
		t.Errorf("the pull is on track %q, want its own", pull.track)
	}
	if got := pull.arg(t, "already present"); got != true {
		t.Errorf("busybox, pulled before, has already present = %v", got)
	}
	if blobs := tr.all("executor", "blob *"); len(blobs) != 0 {
		t.Errorf("a build pulling only images already present copied %d blobs", len(blobs))
	}

	task := tr.one(t, "executor", `task "gen"`)
	assertChildren(t, task, "cache lookup", "start", `env(NONCE="*`, `add("src", "/in")`,
		`run(["sh", "-c", "mkdir /out && cat /in/src/* > /out/all.txt"])`, "collect output", "cache write")
	if collect := task.children[5]; collect.arg(t, "files") != float64(1) || collect.arg(t, "bytes") != float64(5) {
		t.Errorf("collect output = %v, want 1 file of 5 bytes", collect.Args)
	}
	layer := tr.one(t, "executor", `layer "app"`)
	if layer.track != `layer "app"` || layer.arg(t, "outcome") != "building" {
		t.Errorf("layer on track %q, outcome %v; want its own track, and building", layer.track, layer.Args["outcome"])
	}
	assertChildren(t, layer, "cache lookup", "start", `add("gen", "/data")`, `env(NONCE="*`, `workdir("/data")`,
		`run(["cat", "all.txt"])`, "commit", "cache write")
	add := layer.children[2]
	if got := add.childNames(); !slices.Equal(got, []string{"checkout", "copy"}) {
		t.Errorf("add's spans = %q, want checkout and copy", got)
	}
	if add.arg(t, "files") != float64(1) || add.arg(t, "dest") != "/data" {
		t.Errorf("add = %v, want 1 file added to /data", add.Args)
	}
	if run := layer.children[5]; run.arg(t, "stdout bytes") != float64(5) {
		t.Errorf("run = %v, want 5 bytes printed", run.Args)
	}
	commit := layer.children[6]
	id, _ := commit.arg(t, "image ID").(string)
	if id == "" || layer.arg(t, "image ID") != id {
		t.Errorf("commit's image ID %q isn't the layer's %v", id, layer.Args["image ID"])
	}
	withStore(t, func(store storage.Store) {
		if _, err := store.Image(id); err != nil {
			t.Errorf("committed image %s isn't in the store: %v", id, err)
		}
	})

	assertFlows(t, tr, []string{
		`glob "src" -> add("src", "/in")`,
		`pull "busybox" -> start`, // the task's
		`pull "busybox" -> start`, // the layer's
		`collect output -> add("gen", "/data")`,
	})

	// Built again, the targets are reused, and still make what the steps that use them need.
	vamos(t, fsys, "build", "app", "--trace", path, "-C", projectDir).ran(t)
	tr = readTrace(t, path)
	layer = tr.one(t, "executor", `layer "app"`)
	if layer.arg(t, "outcome") != "cached" || layer.arg(t, "image ID") != id {
		t.Errorf("the layer built again = %v, want it cached, with image %s", layer.Args, id)
	}
	if got := layer.childNames(); !slices.Equal(got, []string{"cache lookup"}) {
		t.Errorf("the cached layer's spans = %q, want only its lookup", got)
	}
	assertFlows(t, tr, nil) // nothing used what the cached targets made, since the layer wasn't built
}

// assertChildren fails the test unless the slices directly within s are named want,
// in order, each name ending in "*" a prefix.
func assertChildren(t *testing.T, s *traceSlice, want ...string) {
	t.Helper()
	got := s.childNames()
	ok := len(got) == len(want)
	for i := 0; ok && i < len(want); i++ {
		prefix, wild := strings.CutSuffix(want[i], "*")
		ok = got[i] == want[i] || wild && strings.HasPrefix(got[i], prefix)
	}
	if !ok {
		t.Fatalf("the spans of %q = %q, want %q", s.Name, got, want)
	}
}

// assertFlows fails the test unless the trace's arrows are want, each "from -> to", in any order.
func assertFlows(t *testing.T, tr *trace, want []string) {
	t.Helper()
	var got []string
	for _, f := range tr.flows {
		got = append(got, f[0].Name+" -> "+f[1].Name)
	}
	slices.Sort(got)
	want = slices.Clone(want)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("arrows = %q, want %q", got, want)
	}
}

// TestTracePush checks the trace of pushes to a registry, from the executor and from the client:
// the executor's job is part of the client's span for sending the image, and the copy to the registry,
// the executor's for a push and the client's after an export, has a span for each blob,
// whose bytes sent are what the registry's manifest lists.
// Pushed again, every layer is skipped, as the registry has it.
func TestTracePush(t *testing.T) {
	requireBuildah(t)
	for _, tt := range []struct {
		from    string
		track   string // of the executor's job
		process string // that copies to the registry
	}{
		{
			from:    "executor",
			track:   "push",
			process: "executor",
		},
		{
			from:    "client",
			track:   "export",
			process: "vamos",
		},
	} {
		t.Run(tt.from, func(t *testing.T) {
			_, host := insecureRegistry(t, nil)
			image := host + "/team/app:v1"
			fsys := project(t, helloBuild)
			path := filepath.Join(t.TempDir(), "trace.json")
			push := func() *trace {
				t.Helper()
				vamos(t, fsys, "push", "app", image, "--push-from", tt.from, "--tls-verify=false",
					"--trace", path, "-C", projectDir).succeeds(t)
				return readTrace(t, path)
			}
			tr := push()

			sending := tr.one(t, "vamos", "pushing to "+image)
			executor := tr.one(t, "executor", tt.track+" to *")
			if executor.track != tt.track || !executor.within(sending) {
				t.Errorf("the executor's %q, on track %q, isn't within the client's %q",
					executor.Name, executor.track, sending.Name)
			}
			copied := tr.one(t, tt.process, "copy to docker://*")
			if !copied.within(sending) {
				t.Errorf("the copy to the registry, %q, isn't within the client's %q", copied.Name, sending.Name)
			}
			ref, err := name.ParseReference(image, name.Insecure)
			must(t, err)
			manifest, err := remote.Get(ref)
			must(t, err)
			if got := copied.arg(t, "manifest"); got != manifest.Digest.String() {
				t.Errorf("the copy's manifest = %v, want the registry's %s", got, manifest.Digest)
			}
			img, err := manifest.Image()
			must(t, err)
			m, err := img.Manifest()
			must(t, err)
			sizes := map[int64]bool{m.Config.Size: true}
			for _, l := range m.Layers {
				sizes[l.Size] = true
			}
			// The blobs within the copy to the registry; an export copies blobs to the content store too.
			var blobs []*traceSlice
			for _, b := range tr.all(tt.process, "blob *") {
				if b.within(copied) {
					blobs = append(blobs, b)
				}
			}
			if len(blobs) != len(m.Layers)+1 {
				t.Errorf("%d blob spans, want one for the config and each of %d layers", len(blobs), len(m.Layers))
			}
			for _, b := range blobs {
				if b.Phase != "b" {
					t.Errorf("blob %q isn't an async span", b.Name)
				}
				if sent := int64(b.arg(t, "bytes sent").(float64)); !sizes[sent] {
					t.Errorf("blob %q sent %d bytes, which no blob in the registry's manifest has", b.Name, sent)
				}
			}

			// Pushed again, the layers are skipped.
			// containers/image sends the config again, as it never looks for one at the destination,
			// though a registry that has it takes nothing.
			tr = push()
			copied = tr.one(t, tt.process, "copy to docker://*")
			blobs = nil
			for _, b := range tr.all(tt.process, "blob *") {
				if b.within(copied) {
					blobs = append(blobs, b)
				}
			}
			if len(blobs) != 1 || blobs[0].arg(t, "source digest") != m.Config.Digest.String() {
				t.Errorf("pushed again, %d blobs were copied, want only the config, %s", len(blobs), m.Config.Digest)
			}
			skipped := tr.all(tt.process, "skipped blob *")
			if len(skipped) != len(m.Layers) {
				t.Errorf("pushed again, %d blobs were skipped, want each of %d layers", len(skipped), len(m.Layers))
			}
			for _, s := range skipped {
				if s.Phase != "i" || s.parent == nil || !strings.HasPrefix(s.parent.Name, "copy to ") {
					t.Errorf("skipped blob %q isn't an instant within the copy", s.Name)
				}
			}
		})
	}
}

// TestTracePull checks that pulling an image the store lacks traces resolving its tag, and copying its blobs.
func TestTracePull(t *testing.T) {
	requireBuildah(t)
	_, host := insecureRegistry(t, nil)
	images := pushIndex(t, host+"/base:v1", native) // random, so not pulled before
	fsys := project(t, map[string]string{
		"BUILD": fmt.Sprintf(`pull(name="base", image="%s/base", tag="v1")`, host),
	})
	path := filepath.Join(t.TempDir(), "trace.json")
	vamos(t, fsys, "build", "base", "--tls-verify=false", "--trace", path, "-C", projectDir).ran(t)
	tr := readTrace(t, path)

	pull := tr.one(t, "executor", `pull "base"`)
	if got := pull.childNames(); len(got) < 2 || !strings.HasPrefix(got[0], "resolve ") ||
		!strings.HasPrefix(got[1], "copy to ") {
		t.Errorf("the pull's spans = %q, want it to resolve its tag, then copy the image", got)
	}
	m, err := images[native].Manifest()
	must(t, err)
	digest, err := images[native].Digest()
	must(t, err)
	if got := pull.arg(t, "instance"); got != digest.String() {
		t.Errorf("the pull's instance = %v, want %s", got, digest)
	}
	blobs := tr.all("executor", "blob *")
	if len(blobs) != len(m.Layers)+1 {
		t.Errorf("%d blob spans, want one for the config and each of %d layers", len(blobs), len(m.Layers))
	}
	for _, b := range blobs {
		if !b.within(pull) {
			t.Errorf("blob %q isn't within the pull", b.Name)
		}
	}
}

// TestTraceFailure checks that a failing build still writes its trace,
// which ends at the failing step, with its error, and that the build's error is the one reported.
func TestTraceFailure(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox + `layer(name="app", base="busybox", steps=[run(["sh", "-c", "exit 3"]), ` +
			`env(NONCE="` + newNonce() + `")])`,
	})
	path := filepath.Join(t.TempDir(), "trace.json")
	vamos(t, fsys, "build", "app", "--trace", path, "-C", projectDir).
		failsWith(t, false, `layer "app": run(["sh", "-c", "exit 3"]) failed`)
	tr := readTrace(t, path)
	layer := tr.one(t, "executor", `layer "app"`)
	assertChildren(t, layer, "cache lookup", "start", `run(["sh", "-c", "exit 3"])`)
	if err, _ := layer.children[2].arg(t, "error").(string); !strings.Contains(err, "exit status 3") {
		t.Errorf("the failed step's error = %q, want its exit status", err)
	}
	if err, _ := tr.one(t, "vamos", "vamos build app").arg(t, "error").(string); !strings.Contains(err, "failed") {
		t.Errorf("the command's error = %q", err)
	}
}

// TestTraceDestinations checks the traces of commands that write their result on the client:
// save, whose executor's copy to the tarball is within the client's span for writing it,
// and export, whose span says which tree it wrote.
// Neither trace lands in the command's output.
func TestTraceDestinations(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, traceBuild(newNonce()))
	for _, tt := range []struct {
		name   string
		output string // within a temporary folder
		args   func(output string) []string
		check  func(t *testing.T, tr *trace, output string)
	}{
		{
			name:   "save",
			output: "app.tar",
			args:   func(output string) []string { return []string{"save", "app", output} },
			check: func(t *testing.T, tr *trace, output string) {
				writing := tr.one(t, "vamos", "writing "+output)
				if exported := tr.one(t, "executor", "export to the content store"); !exported.within(writing) {
					t.Errorf("the executor's export isn't within the client's %q", writing.Name)
				}
				if copied := tr.one(t, "vamos", "copy to docker-archive:*"); !copied.within(writing) {
					t.Errorf("the client's copy to the tarball isn't within its %q", writing.Name)
				}
			},
		},
		{
			name:   "export",
			output: "out",
			args:   func(output string) []string { return []string{"export", "gen", output} },
			check: func(t *testing.T, tr *trace, output string) {
				exporting := tr.one(t, "vamos", "exporting to "+output)
				if tree, _ := exporting.arg(t, "files").(string); !strings.HasPrefix(tree, "sha256:") {
					t.Errorf("the export's files = %q, want the tree's digest", tree)
				}
				if _, err := os.Stat(filepath.Join(output, "all.txt")); err != nil {
					t.Errorf("the task's file wasn't exported: %v", err)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			output := filepath.Join(dir, tt.output)
			path := filepath.Join(t.TempDir(), "trace.json")
			vamos(t, fsys, append(tt.args(output), "--trace", path, "-C", projectDir)...).ran(t)
			tt.check(t, readTrace(t, path), output)
		})
	}
}

// TestFinishTrace checks which error a traced command returns when its trace can't be written:
// that failure if the command succeeded, or else the command's own, with the trace's failure only warned about.
func TestFinishTrace(t *testing.T) {
	failed := errors.New("the build failed")
	for _, tt := range []struct {
		name    string
		err     error
		want    string
		warning string
	}{
		{
			name: "command succeeded",
			err:  nil,
			want: "writing trace t.json: ",
		},
		{
			name:    "command failed",
			err:     failed,
			want:    failed.Error(),
			warning: "vamos: warning: writing trace t.json: ",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// The folder is gone by the time the trace is written.
			session := &traceSession{
				path:     filepath.Join(t.TempDir(), "gone", "t.json"),
				recorder: tracing.NewRecorder(clientProcess, nil),
			}
			var stderr strings.Builder
			err := session.finishTrace(tt.err, "t.json", &stderr)
			if err == nil || !strings.HasPrefix(err.Error(), tt.want) {
				t.Errorf("error = %v, want %q", err, tt.want)
			}
			if got := stderr.String(); tt.warning == "" && got != "" || !strings.HasPrefix(got, tt.warning) {
				t.Errorf("stderr = %q, want %q", got, tt.warning)
			}
		})
	}
}

// TestTraceMistakes checks that a trace that can't be written,
// or would overwrite the command's output, is reported before anything is loaded or built, leaving no files.
func TestTraceMistakes(t *testing.T) {
	// A BUILD file that fails to load, so that an error about the trace shows it was checked first.
	fsys := project(t, map[string]string{"BUILD": "nope("})
	out := t.TempDir()
	tar := filepath.Join(out, "app.tar")
	linked := filepath.Join(t.TempDir(), "linked") // to out
	must(t, os.Symlink(out, linked))
	// An export's folder holding a link out of it, which the export replaces with whatever the task makes there.
	exported := filepath.Join(t.TempDir(), "exported")
	must(t, os.Mkdir(exported, 0o755))
	must(t, os.Symlink(out, filepath.Join(exported, "sub")))
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "missing folder",
			args: []string{"build", "app", "--trace", filepath.Join(out, "missing", "t.json")},
			want: "--trace " + filepath.Join(out, "missing", "t.json") + ": cannot write " +
				filepath.Join(out, "missing", "t.json") + ": directory " + filepath.Join(out, "missing") +
				" does not exist",
		},
		{
			name: "a folder",
			args: []string{"build", "app", "--trace", out},
			want: "--trace " + out + ": cannot write " + out + ": it is a directory",
		},
		{
			name: "save's tarball",
			args: []string{"save", "app", tar, "--trace", tar},
			want: "--trace " + tar + ": it is also the file that save writes",
		},
		{
			name: "save's tarball, through a link",
			args: []string{"save", "app", tar, "--trace", filepath.Join(linked, "app.tar")},
			want: "it is also the file that save writes",
		},
		{
			name: "export's folder",
			args: []string{"export", "gen", filepath.Join(out, "files"), "--trace", filepath.Join(out, "files")},
			want: "--trace " + filepath.Join(out, "files") + ": it is within the folder that export replaces",
		},
		{
			name: "within export's folder",
			args: []string{"export", "gen", filepath.Join(out, "files"), "--trace",
				filepath.Join(out, "files", "t.json")},
			want: "it is within the folder that export replaces",
		},
		{
			name: "within export's folder, through a link out of it",
			args: []string{"export", "gen", exported, "--replace", "--trace", filepath.Join(exported, "sub", "t.json")},
			want: "it is within the folder that export replaces",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			vamos(t, fsys, append(tt.args, "-C", projectDir)...).failsWith(t, false, tt.want)
			assertNoFiles(t, out)
		})
	}
}

// TestTracePerfetto checks that Perfetto's own importer reads a trace as vamos means it:
// the same slices, nested the same way, the same arrows, and an async track for each blob.
// It needs trace_processor_shell, which the devcontainer has.
func TestTracePerfetto(t *testing.T) {
	requireBuildah(t)
	shell, err := exec.LookPath("trace_processor_shell")
	if err != nil {
		t.Skip("trace_processor_shell isn't installed")
	}
	_, host := insecureRegistry(t, nil)
	image := host + "/team/app:v1"
	path := filepath.Join(t.TempDir(), "trace.json")
	vamos(t, project(t, traceBuild(newNonce())), "push", "app", image, "--tls-verify=false", "--trace", path,
		"-C", projectDir).succeeds(t)
	tr := readTrace(t, path)

	query := func(sql string) [][]string {
		t.Helper()
		cmd := exec.Command(shell, "-q", "/dev/stdin", path)
		cmd.Stdin = strings.NewReader(sql)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("trace_processor_shell: %v", err)
		}
		var rows [][]string
		for i, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if i == 0 {
				continue // the column names
			}
			var row []string
			for _, field := range strings.Split(line, ",") {
				row = append(row, strings.Trim(field, `"`))
			}
			rows = append(rows, row)
		}
		return rows
	}
	count := func(sql string) int {
		t.Helper()
		rows := query(sql)
		n, err := strconv.Atoi(rows[0][0])
		must(t, err)
		return n
	}

	if got := count("select count(*) from slice"); got != len(tr.slices) {
		t.Errorf("Perfetto read %d slices, want %d:\n%s", got, len(tr.slices), tr)
	}
	// Each slice's parent, as Perfetto nests them on their tracks, is the one readTrace found.
	want := map[string]int{}
	for _, s := range tr.slices {
		if s.parent != nil {
			want[s.parent.Name+" > "+s.Name]++
		}
	}
	got := map[string]int{}
	// As hex, since trace_processor_shell doesn't escape the quotes in names.
	for _, row := range query("select hex(p.name || ' > ' || s.name) from slice s join slice p on s.parent_id = p.id") {
		name, err := hex.DecodeString(row[0])
		must(t, err)
		got[string(name)]++
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("Perfetto nests %q %d times, want %d", k, got[k], n)
		}
	}
	for k := range got {
		if want[k] == 0 {
			t.Errorf("Perfetto nests %q, which vamos doesn't", k)
		}
	}
	if got := count("select count(*) from flow"); got != len(tr.flows) || got == 0 {
		t.Errorf("Perfetto read %d arrows, want %d", got, len(tr.flows))
	}
	blobs := len(tr.all("executor", "blob *"))
	if got := count("select count(*) from slice where name like 'blob %' and depth = 0"); got != blobs || got == 0 {
		t.Errorf("Perfetto read %d blob slices, each on its own async track, want %d", got, blobs)
	}
}
