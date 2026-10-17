package cli

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/graph"

	"github.com/spf13/afero"
)

// TestGraphExamples checks that the examples' graphs are valid, and round-trip through their encoding.
func TestGraphExamples(t *testing.T) {
	tests := []struct {
		dir     string
		targets []string
	}{
		{
			dir:     "../../examples/typescript-mcp",
			targets: []string{"server"},
		},
		{
			dir:     "../../examples/fastmcp",
			targets: []string{"server"},
		},
		{
			dir:     "../../examples/strawberry-react",
			targets: []string{"//backend:layer", "//backend:test", "//ui:layer", "//ui:lint"},
		},
		{
			dir:     "../../examples/golang-cli",
			targets: []string{"echo"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.dir, func(t *testing.T) {
			args := append([]string{"graph"}, tt.targets...)
			r := vamos(t, afero.NewOsFs(), append(args, "-C", tt.dir)...).ran(t)
			g, err := graph.Decode([]byte(r.stdout))
			if err != nil {
				t.Fatal(err)
			}
			var compact bytes.Buffer
			must(t, json.Compact(&compact, []byte(r.stdout)))
			if got := graph.Encode(g); !bytes.Equal(got, compact.Bytes()) {
				t.Errorf("graph encodes as\n%s\nwant\n%s", got, compact.Bytes())
			}
		})
	}
}

// TestGraphConfigurations checks the graph of an index of a layer for two platforms,
// which adds a task in two configurations, each for the exec platform, so that both platforms' layers share them.
func TestGraphConfigurations(t *testing.T) {
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox + `
glob(name="code", include=["app.sh"])
task(
    name="build",
    params={"mode": "release"},
    base="busybox",
    steps=[add("code", "/src"), run(["sh", "-c", "mkdir /out"])],
    source="/out",
    include=["."],
)
layer(name="app", base="busybox", steps=[
    add(dep("build", platform=EXEC_PLATFORM, mode="debug"), "/debug"),
    add(dep("build", platform=EXEC_PLATFORM), "/app"),
])
index(name="all", layer="app", platforms=["linux/amd64", "linux/arm64"])
`,
		"app.sh": "echo hi",
	})
	r := vamos(t, fsys, "graph", "all", "-C", projectDir).ran(t)
	g, err := graph.Decode([]byte(r.stdout))
	if err != nil {
		t.Fatal(err)
	}

	var sources, tasks, layers []string
	for _, n := range g.Nodes {
		switch {
		case n.Source != nil:
			sources = append(sources, n.Desc)
		case n.Task != nil:
			tasks = append(tasks, n.Desc)
		case n.Layer != nil:
			layers = append(layers, n.Desc)
		}
	}
	if want := []string{`glob "code"`}; !slices.Equal(sources, want) {
		t.Errorf("globs = %q, want %q, read once for every configuration", sources, want)
	}
	// The command's platform is the exec platform, so the tasks are described without one.
	if want := []string{`task "build" (mode="debug")`, `task "build"`}; !slices.Equal(tasks, want) {
		t.Errorf("tasks = %q, want %q, each built once, on the exec platform, for both layers", tasks, want)
	}
	slices.Sort(layers)
	var want []string
	for _, p := range []string{"linux/amd64", "linux/arm64"} {
		if p == native {
			want = append(want, `layer "app"`)
		} else {
			want = append(want, `layer "app" (platform=`+p+`)`)
		}
	}
	slices.Sort(want)
	if !slices.Equal(layers, want) {
		t.Errorf("layers = %q, want %q", layers, want)
	}

	if len(g.Roots) != 1 || g.Node(g.Roots[0]).Index == nil {
		t.Fatalf("roots = %v, want the index alone", g.Roots)
	}
	var platforms []string
	for _, m := range g.Node(g.Roots[0]).Index.Manifests {
		platforms = append(platforms, g.Node(m).Platform())
	}
	if want := []string{"linux/amd64", "linux/arm64"}; !slices.Equal(platforms, want) {
		t.Errorf("index's platforms = %q, want %q, in order", platforms, want)
	}
}
