package graph

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/fileset"
	"github.com/Topher-the-Geek/vamos/internal/testenv"

	"github.com/opencontainers/go-digest"
)

// testSelection returns a compiled selection of the files include and exclude select, the defaults changed by change.
func testSelection(t *testing.T, include, exclude []string, change func(sel *fileset.Selection)) *fileset.Selection {
	t.Helper()
	sel := fileset.DefaultSelection()
	sel.Include, sel.Exclude = include, exclude
	if change != nil {
		change(sel)
	}
	testenv.Must(t, sel.Compile(testenv.ProjectDir))
	return sel
}

// testGraph returns a valid graph that uses every kind of node and step: a glob's files, built by a task on a
// pulled image, are added to a layer, which an index lists.
func testGraph(t *testing.T) *Graph {
	t.Helper()
	from := func(ref Ref) *Ref { return &ref }
	return &Graph{
		Version: Version,
		Nodes: []Node{
			{Label: "src", Desc: `glob "src"`, Source: &Source{
				Tree: cache.Blob{Digest: digest.FromString("tree"), Size: 4},
			}},
			{Label: "busybox", Desc: `pull "busybox"`, Pull: &Pull{
				Image:    "docker.io/library/busybox:1.36",
				Platform: "linux/amd64",
			}},
			{Label: "build", Desc: `task "build"`, Task: &Task{
				Base: 1,
				Steps: []Step{
					{Op: OpAdd, Display: `add("src", "/src")`, From: from(0), Dest: "/src"},
					{Op: OpRun, Display: `run(["make"])`, Argv: []string{"make"}},
				},
				Output: &Output{Source: "/out", Selection: testSelection(t, []string{"."}, nil, nil)},
			}},
			{Label: "app", Desc: `layer "app"`, Layer: &Layer{
				Base:     1,
				Platform: "linux/amd64",
				Steps: []Step{
					{Op: OpAdd, Display: `add("build", "$APP")`, From: from(2), Dest: "$APP", Owner: "app"},
					{Op: OpEnv, Display: `env(A="1")`, Vars: []string{"A=1"}},
					{Op: OpBuildEnv, Display: `build_env(B="$A")`, Vars: []string{"B=$A"}},
					{Op: OpWorkdir, Display: `workdir("/app")`, Dir: "/app"},
				},
				Command: []string{"/app/server"},
			}},
			{Label: "all", Desc: `index "all"`, Index: &Index{Manifests: []Ref{3}}},
		},
		Roots: []Ref{4},
	}
}

// TestGraphValidate checks each rule of validate, on graphs that a BUILD file can't make.
func TestGraphValidate(t *testing.T) {
	tests := []struct {
		name   string
		change func(g *Graph)
		want   string // in the error; "" for none
	}{
		{
			name:   "valid",
			change: func(g *Graph) {},
		},
		{
			name:   "another version",
			change: func(g *Graph) { g.Version = 0 },
			want:   "invalid build graph: version 0, want 1",
		},
		{
			name:   "no kind",
			change: func(g *Graph) { g.Nodes[0].Source = nil },
			want:   `node 0 (glob "src"): it sets 0 kinds, want 1`,
		},
		{
			name:   "two kinds",
			change: func(g *Graph) { g.Nodes[1].Source = &Source{} },
			want:   `node 1 (pull "busybox"): it sets 2 kinds, want 1`,
		},
		{
			name:   "a ref to a later node",
			change: func(g *Graph) { g.Nodes[3].Layer.Base = 3 },
			want:   `node 3 (layer "app"): base is 3, which isn't an earlier node`,
		},
		{
			name:   "a base of the wrong kind",
			change: func(g *Graph) { g.Nodes[3].Layer.Base = 0 },
			want:   `node 3 (layer "app"): base is a source, want pull or layer`,
		},
		{
			name:   "a base for another platform",
			change: func(g *Graph) { g.Nodes[3].Layer.Platform = "linux/arm64" },
			want:   `node 3 (layer "app"): its base is for linux/amd64, not linux/arm64`,
		},
		{
			name:   "a platform not normalized",
			change: func(g *Graph) { g.Nodes[1].Pull.Platform = "linux/arm64/v8" },
			want:   `platform "linux/arm64/v8" isn't normalized; want "linux/arm64"`,
		},
		{
			name:   "an image without a tag",
			change: func(g *Graph) { g.Nodes[1].Pull.Image = "docker.io/library/busybox" },
			want:   `image "docker.io/library/busybox" has no tag`,
		},
		{
			name:   "a source tree that isn't a digest",
			change: func(g *Graph) { g.Nodes[0].Source.Tree = cache.Blob{} },
			want:   `node 0 (glob "src"):`,
		},
		{
			name:   "a relative source",
			change: func(g *Graph) { g.Nodes[2].Task.Output.Source = "out" },
			want:   `node 2 (task "build"): source "out" isn't absolute`,
		},
		{
			name: "a selection that doesn't compile",
			change: func(g *Graph) {
				g.Nodes[2].Task.Output.Selection = &fileset.Selection{Include: []string{"../x"}, ModeMask: 0o755,
					MaxFileCount: 1, MaxFileSize: 1, MaxTotalSize: 1}
			},
			want: `include "../x" is outside /out`,
		},
		{
			name:   "an add from scratch",
			change: func(g *Graph) { *g.Nodes[3].Layer.Steps[0].From = Scratch },
			want:   "steps[0] (add(\"build\", \"$APP\")): from is -1, which isn't an earlier node",
		},
		{
			name:   "an add from a task without output",
			change: func(g *Graph) { g.Nodes[2].Task.Output = nil },
			want:   "from is a task without output",
		},
		{
			name:   "an unknown op",
			change: func(g *Graph) { g.Nodes[2].Task.Steps[1].Op = "copy" },
			want:   `unknown op "copy"`,
		},
		{
			name:   "a field of another op",
			change: func(g *Graph) { g.Nodes[2].Task.Steps[1].Dest = "/x" },
			want:   "it sets fields that run doesn't use",
		},
		{
			name:   "an empty argv",
			change: func(g *Graph) { g.Nodes[2].Task.Steps[1].Argv = nil },
			want:   "argv is empty",
		},
		{
			name:   "a variable given twice",
			change: func(g *Graph) { g.Nodes[3].Layer.Steps[1].Vars = []string{"A=1", "A=2"} },
			want:   `variable "A" is given twice`,
		},
		{
			name:   "a variable that refers badly",
			change: func(g *Graph) { g.Nodes[3].Layer.Steps[1].Vars = []string{"A=${"} },
			want:   `A: "${" must be followed`,
		},
		{
			name:   "a relative workdir",
			change: func(g *Graph) { g.Nodes[3].Layer.Steps[3].Dir = "app" },
			want:   `path "app" must be an absolute path`,
		},
		{
			name:   "a NUL byte",
			change: func(g *Graph) { g.Nodes[3].Layer.Command = []string{"a\x00"} },
			want:   `node 3 (layer "app"): "a\x00" holds a NUL byte`,
		},
		{
			name:   "an index without manifests",
			change: func(g *Graph) { g.Nodes[4].Index.Manifests = nil },
			want:   "an index has no manifests",
		},
		{
			name:   "an index with two manifests for a platform",
			change: func(g *Graph) { g.Nodes[4].Index.Manifests = []Ref{1, 3} },
			want:   "two manifests are for linux/amd64",
		},
		{
			name:   "no roots",
			change: func(g *Graph) { g.Roots = nil },
			want:   "invalid build graph: it has no roots",
		},
		{
			name:   "a root twice",
			change: func(g *Graph) { g.Roots = []Ref{4, 4} },
			want:   "invalid build graph: node 4 is a root twice",
		},
		{
			name:   "a root that names no node",
			change: func(g *Graph) { g.Roots = []Ref{5} },
			want:   "invalid build graph: root 0 is 5, which names no node",
		},
		{
			name:   "a node no root uses",
			change: func(g *Graph) { g.Roots = []Ref{3} },
			want:   `invalid build graph: node 4 (index "all") is used by no root`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := testGraph(t)
			tt.change(g)
			err := g.Validate()
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("validate() = %v, want no error", err)
			case tt.want != "" && err == nil:
				t.Fatalf("validate() = nil, want error containing %q", tt.want)
			case tt.want != "" && !strings.Contains(err.Error(), tt.want):
				t.Fatalf("validate() = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

// TestGraphRoundTrip checks that a graph decodes as it was encoded, ready to run: its selections compiled.
func TestGraphRoundTrip(t *testing.T) {
	g := testGraph(t)
	got, err := Decode(Encode(g))
	if err != nil {
		t.Fatal(err)
	}
	// DeepEqual compares the selections' compiled patterns too, which decoding leaves out.
	if !reflect.DeepEqual(got, g) {
		t.Errorf("decoded graph =\n%s\nwant\n%s", Encode(got), Encode(g))
	}
}
