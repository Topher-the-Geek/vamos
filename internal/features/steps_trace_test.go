package features

import (
	"cmp"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// Steps about the trace that --trace writes, which Perfetto shows: its spans, as slices on tracks, nested as they
// are within each other, their arguments, and the arrows from what one made to what another used.
// A span is named by its process, "vamos" for the client or "executor", and its name, which may start or end in "*",
// as matches says, and in which placeholders, such as <destination> and <registry>, stand for what atPlaceholders
// says.

// traced writes the command's trace to a file of the scenario's own, with --trace, which Then steps read.
// Every command takes it.
func traced() option {
	return option{"traced", everyCommand, func(w *world, c *commandLine) {
		c.traceWith(w, "traced")
		w.traceTo = filepath.Join(w.t.TempDir(), "trace.json")
		c.flags = append(c.flags, "--trace", w.traceTo)
	}}
}

// tracedTo writes the command's trace to path, with --trace, in which <destination> stands for the folder that the
// command sends to, and <outside> for the folder of GivenAFileOutsideTheProjectHolds: for a path that is a mistake.
// Every command takes it. The path is given once the When step has made the destination, as the command runs.
func tracedTo(path string) option {
	return option{"tracedTo", everyCommand, func(w *world, c *commandLine) {
		c.traceWith(w, "tracedTo")
		w.traceAt = path
	}}
}

// traceWith records that the option by gives --trace, failing the scenario if another option has already.
func (c *commandLine) traceWith(w *world, by string) {
	w.t.Helper()
	if c.traceBy != "" {
		w.t.Fatalf("both %s() and %s() give --trace", c.traceBy, by)
	}
	c.traceBy = by
}

// theTrace returns the trace that the last command wrote, failing the scenario if it was given no traced() option,
// or wrote none.
func (w *world) theTrace() *trace {
	w.t.Helper()
	if w.trace == nil {
		w.trace = readTrace(w.t, w.theTracePath())
	}
	return w.trace
}

// theTracePath returns where the last command wrote its trace, failing the scenario if it was given no traced()
// option.
func (w *world) theTracePath() string {
	w.t.Helper()
	if w.tracePath == "" {
		w.t.Fatal("the last command was given no traced() option")
	}
	return w.tracePath
}

// span returns the only span of process that name names, in which placeholders stand for what atPlaceholders says,
// failing the scenario unless there is exactly one.
func (w *world) span(process, name string) *traceSlice {
	w.t.Helper()
	name = w.atPlaceholders(name)
	tr := w.theTrace()
	found := tr.all(process, name)
	if len(found) != 1 {
		w.t.Fatalf("the trace has %d spans of %s named %q, want 1; it has:\n%s", len(found), process, name, tr)
	}
	return found[0]
}

// ThenTheTraceHasNoSpan checks that the trace has no span of process that name names.
func (w *world) ThenTheTraceHasNoSpan(process, name string) {
	w.t.Helper()
	if found := w.theTrace().all(process, w.atPlaceholders(name)); len(found) > 0 {
		w.t.Errorf("the trace has %d spans of %s named %q, want none", len(found), process, name)
	}
}

// ThenTheTraceDoesNotContain checks that none of values is anywhere in the trace file.
func (w *world) ThenTheTraceDoesNotContain(values ...string) {
	w.t.Helper()
	data, err := os.ReadFile(w.theTracePath())
	if err != nil {
		w.t.Fatal(err)
	}
	for _, value := range values {
		if strings.Contains(string(data), value) {
			w.t.Errorf("the trace holds %q", value)
		}
	}
}

// ThenTheTraceCopiesNoBlobWithin checks that the trace has no span of process for a blob within the span that name
// names: that it copied nothing.
func (w *world) ThenTheTraceCopiesNoBlobWithin(process, name string) {
	w.t.Helper()
	if blobs := w.blobsWithin(process, w.span(process, name)); len(blobs) > 0 {
		w.t.Errorf("%d blobs were copied within %q, want none", len(blobs), name)
	}
}

// ThenTheTraceSpanHolds checks that the spans directly within the span of process that name names are exactly
// children, in order, each of which may start or end in "*", as matches says.
func (w *world) ThenTheTraceSpanHolds(process, name string, children ...string) {
	w.t.Helper()
	s := w.span(process, name)
	got := s.childNames()
	same := len(got) == len(children)
	for i := 0; same && i < len(children); i++ {
		same = matches(got[i], children[i])
	}
	if !same {
		w.t.Errorf("the spans within %q are %q, want %q", s.Name, got, children)
	}
}

// matches reports whether s is what want names: want itself, or, if want starts or ends in "*", or both, a string
// that ends with, starts with, or holds what is between.
func matches(s, want string) bool {
	inner, leading := strings.CutPrefix(want, "*")
	inner, trailing := strings.CutSuffix(inner, "*")
	switch {
	case leading && trailing:
		return strings.Contains(s, inner)
	case leading:
		return strings.HasSuffix(s, inner)
	case trailing:
		return strings.HasPrefix(s, inner)
	}
	return s == want
}

// ThenTheTraceSpanIsWithin checks that the span of process that name names takes place within the span of
// outerProcess that outer names, by the trace's clock, and, on the same track, is nested within it.
func (w *world) ThenTheTraceSpanIsWithin(process, name, outerProcess, outer string) {
	w.t.Helper()
	s, o := w.span(process, name), w.span(outerProcess, outer)
	if !s.within(o) {
		w.t.Errorf("%q isn't within %q", s.Name, o.Name)
	}
	if s.process == o.process && s.track == o.track {
		for p := s.parent; ; p = p.parent {
			if p == nil {
				w.t.Errorf("%q isn't nested within %q on their track", s.Name, o.Name)
				break
			}
			if p == o {
				break
			}
		}
	}
}

// ThenTheTraceSpanIsOnTrack checks that the span of process that name names is on the track named track.
func (w *world) ThenTheTraceSpanIsOnTrack(process, name, track string) {
	w.t.Helper()
	if s := w.span(process, name); s.track != track {
		w.t.Errorf("%q is on the track %q, want %q", s.Name, s.track, track)
	}
}

// traceArgs are the arguments of a span, by key, as JSON decodes them: a number is a float64.
type traceArgs map[string]any

// ThenTheTraceSpanSaysAtLeast checks the span's arguments that want names, and not its others, in which a string
// value may start with "*" or end in "*", or both, to say only how it ends or starts, or what it holds, "<image>"
// stands for the full ID of the image that the command sent, and other placeholders for what atPlaceholders says.
// A number is compared as a number.
func (w *world) ThenTheTraceSpanSaysAtLeast(process, name string, want traceArgs) {
	w.t.Helper()
	s := w.span(process, name)
	for _, key := range slices.Sorted(maps.Keys(want)) {
		got, ok := s.Args[key]
		if !ok {
			w.t.Errorf("%q has no argument %q; it has %v", s.Name, key, s.Args)
			continue
		}
		switch v := want[key].(type) {
		case int:
			if got != float64(v) { // as JSON decodes it
				w.t.Errorf("%q says %s = %#v, want %d", s.Name, key, got, v)
			}
		case string:
			if v == "<image>" {
				if w.image == nil {
					w.t.Fatal("the last command sent no image for <image> to stand for")
				}
				v = w.imageID(w.image)
			}
			v = w.atPlaceholders(v)
			if str, isText := got.(string); !isText || !matches(str, v) {
				w.t.Errorf("%q says %s = %#v, want %q", s.Name, key, got, v)
			}
		default:
			if !reflect.DeepEqual(got, v) {
				w.t.Errorf("%q says %s = %#v, want %#v", s.Name, key, got, v)
			}
		}
	}
}

// ThenTheTraceGlobHasTheTreeTheGraphShows checks that the span of the glob named glob says the tree of files it
// captured is the one that the graph WhenIShowTheGraph showed has for it.
func (w *world) ThenTheTraceGlobHasTheTreeTheGraphShows(glob string) {
	w.t.Helper()
	if w.graph == nil {
		w.t.Fatal(w.noGraph())
	}
	var tree string
	for _, n := range w.graph.Nodes {
		if n.Source != nil && n.Label == glob {
			tree = n.Source.Tree.Digest.String()
		}
	}
	if tree == "" {
		w.t.Fatalf("the graph has no glob %q", glob)
	}
	s := w.span("vamos", fmt.Sprintf("glob %q", glob))
	if got := s.Args["tree"]; got != tree {
		w.t.Errorf("%q says its tree is %v, want the graph's, %s", s.Name, got, tree)
	}
}

// ThenTheTraceArrowsAre checks that the trace's arrows, from what one span made to the span that used it, are
// exactly want, each "from -> to", in any order.
func (w *world) ThenTheTraceArrowsAre(want ...string) {
	w.t.Helper()
	var got []string
	for _, f := range w.theTrace().flows {
		got = append(got, f[0].Name+" -> "+f[1].Name)
	}
	slices.Sort(got)
	want = slices.Sorted(slices.Values(want))
	if !slices.Equal(got, want) {
		w.t.Errorf("the arrows are %q, want %q", got, want)
	}
}

// ThenTheTraceCopiesTheImagePushed checks that, within the span of process that copies to the registry, the trace
// has an async span for each blob of the image pushed, its config and every layer, each saying the bytes it sent,
// as many as the registry's manifest lists for some blob, and the manifest's digest.
func (w *world) ThenTheTraceCopiesTheImagePushed(process string) {
	w.t.Helper()
	copied, m, digest := w.copyToRegistry(process)
	if got := copied.Args["manifest"]; got != digest {
		w.t.Errorf("the copy says its manifest is %v, want the registry's, %s", got, digest)
	}
	sizes := map[int64]bool{m.Config.Size: true}
	for _, l := range m.Layers {
		sizes[l.Size] = true
	}
	blobs := w.blobsWithin(process, copied)
	if len(blobs) != len(m.Layers)+1 {
		w.t.Errorf("%d blob spans, want one for the config and each of %d layers", len(blobs), len(m.Layers))
	}
	for _, b := range blobs {
		if b.Phase != "b" {
			w.t.Errorf("blob %q isn't an async span", b.Name)
		}
		if sent, _ := b.Args["bytes sent"].(float64); !sizes[int64(sent)] {
			w.t.Errorf("blob %q sent %v bytes, which no blob in the registry's manifest has", b.Name,
				b.Args["bytes sent"])
		}
	}
}

// ThenTheTraceSkipsTheLayersPushed checks that, within the span of process that copies to the registry, the trace
// copies only the config of the image pushed, which containers/image sends again whatever the registry has, and has
// an instant within the copy for each layer it skips, as the registry has it.
func (w *world) ThenTheTraceSkipsTheLayersPushed(process string) {
	w.t.Helper()
	copied, m, _ := w.copyToRegistry(process)
	blobs := w.blobsWithin(process, copied)
	if len(blobs) != 1 || blobs[0].Args["source digest"] != m.Config.Digest.String() {
		w.t.Errorf("%d blobs were copied, want only the config, %s", len(blobs), m.Config.Digest)
	}
	skipped := w.theTrace().all(process, "skipped blob *")
	if len(skipped) != len(m.Layers) {
		w.t.Errorf("%d blobs were skipped, want each of %d layers", len(skipped), len(m.Layers))
	}
	for _, s := range skipped {
		if s.Phase != "i" || s.parent == nil || !strings.HasPrefix(s.parent.Name, "copy to ") {
			w.t.Errorf("skipped blob %q isn't an instant within the copy", s.Name)
		}
	}
}

// copyToRegistry returns the span of process that copies to the registry, and the manifest of the image pushed,
// with its digest.
func (w *world) copyToRegistry(process string) (*traceSlice, *v1.Manifest, string) {
	w.t.Helper()
	copied := w.span(process, "copy to docker://*")
	ref, err := name.ParseReference(w.sentAs, name.Insecure)
	if err != nil {
		w.t.Fatal(err)
	}
	desc, err := remote.Get(ref, w.readingTheRegistry()...)
	if err != nil {
		w.t.Fatal(err)
	}
	img, err := desc.Image()
	if err != nil {
		w.t.Fatal(err)
	}
	m, err := img.Manifest()
	if err != nil {
		w.t.Fatal(err)
	}
	return copied, m, desc.Digest.String()
}

// blobsWithin returns the spans of process for blobs that take place within the span outer.
func (w *world) blobsWithin(process string, outer *traceSlice) []*traceSlice {
	var within []*traceSlice
	for _, b := range w.theTrace().all(process, "blob *") {
		if b.within(outer) {
			within = append(within, b)
		}
	}
	return within
}

// ThenTheTracePullsTheImage checks that the span of the pull that name names resolves its tag and then copies the
// image, which it says is the image that label names for platform, which a Given step put in the registry,
// with an async span within it for each of that image's blobs.
func (w *world) ThenTheTracePullsTheImage(pull, label, platform string) {
	w.t.Helper()
	s := w.span("executor", pull)
	if got := s.childNames(); len(got) < 2 || !strings.HasPrefix(got[0], "resolve ") ||
		!strings.HasPrefix(got[1], "copy to ") {
		w.t.Errorf("the pull's spans are %q, want it to resolve its tag, then copy the image", got)
	}
	img, ok := w.held[w.heldKey(label, platform)]
	if !ok {
		w.t.Fatalf("no Given step put %s for %s in the registry", label, platform)
	}
	digest, err := img.Digest()
	if err != nil {
		w.t.Fatal(err)
	}
	if got := s.Args["instance"]; got != digest.String() {
		w.t.Errorf("the pull says its instance is %v, want %s", got, digest)
	}
	m, err := img.Manifest()
	if err != nil {
		w.t.Fatal(err)
	}
	blobs := w.theTrace().all("executor", "blob *")
	if len(blobs) != len(m.Layers)+1 {
		w.t.Errorf("%d blob spans, want one for the config and each of %d layers", len(blobs), len(m.Layers))
	}
	for _, b := range blobs {
		if !b.within(s) {
			w.t.Errorf("blob %q isn't within the pull", b.Name)
		}
	}
}

// ThenPerfettoReadsTheTraceAsVamosMeansIt checks that Perfetto's own importer, trace_processor_shell, reads the
// trace as these steps do: the same spans, nested the same way, the same arrows, and an async track for each blob.
// It skips the scenario without trace_processor_shell, which the devcontainer has.
func (w *world) ThenPerfettoReadsTheTraceAsVamosMeansIt() {
	w.t.Helper()
	shell, err := exec.LookPath("trace_processor_shell")
	if err != nil {
		w.t.Skip("trace_processor_shell isn't installed")
	}
	tr := w.theTrace()
	query := func(sql string) [][]string {
		w.t.Helper()
		cmd := exec.Command(shell, "-q", "/dev/stdin", w.theTracePath())
		cmd.Stdin = strings.NewReader(sql)
		out, err := cmd.Output()
		if err != nil {
			w.t.Fatalf("trace_processor_shell: %v", err)
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
		w.t.Helper()
		n, err := strconv.Atoi(query(sql)[0][0])
		if err != nil {
			w.t.Fatal(err)
		}
		return n
	}
	if got := count("select count(*) from slice"); got != len(tr.slices) {
		w.t.Errorf("Perfetto read %d slices, want %d:\n%s", got, len(tr.slices), tr)
	}
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
		if err != nil {
			w.t.Fatal(err)
		}
		got[string(name)]++
	}
	for k, n := range want {
		if got[k] != n {
			w.t.Errorf("Perfetto nests %q %d times, want %d", k, got[k], n)
		}
	}
	for k := range got {
		if want[k] == 0 {
			w.t.Errorf("Perfetto nests %q, which vamos doesn't", k)
		}
	}
	if got := count("select count(*) from flow"); got != len(tr.flows) || got == 0 {
		w.t.Errorf("Perfetto read %d arrows, want %d", got, len(tr.flows))
	}
	blobs := len(tr.all("executor", "blob *"))
	if got := count("select count(*) from slice where name like 'blob %' and depth = 0"); got != blobs || got == 0 {
		w.t.Errorf("Perfetto read %d blob slices, each on its own async track, want %d", got, blobs)
	}
}

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
}

// readTrace reads the trace file at path, as Perfetto would.
func readTrace(t *testing.T, path string) *trace {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
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
		if s.process == process && matches(s.Name, name) {
			found = append(found, s)
		}
	}
	return found
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
