package sched

import (
	"encoding/json"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/fileset"
	"github.com/Topher-the-Geek/vamos/internal/testenv"

	"github.com/opencontainers/go-digest"
)

// testSelection returns a compiled selection of the files include and exclude select.
func testSelection(t *testing.T, include, exclude []string) *fileset.Selection {
	t.Helper()
	sel := fileset.DefaultSelection()
	sel.Include, sel.Exclude = include, exclude
	testenv.Must(t, sel.Compile(testenv.ProjectDir))
	return sel
}

// TestActionKeyStable pins how an Action is encoded, and so every target's key:
// a change that alters it would make every cached result unusable, so it must be deliberate, and raise ActionVersion.
func TestActionKeyStable(t *testing.T) {
	files := cache.Blob{Digest: digest.FromString("files"), Size: 5}
	a := &Action{
		Version:  ActionVersion,
		Kind:     "layer",
		Base:     "0123abcd",
		Platform: "linux/arm64",
		Steps: []Step{
			{Op: "add", Files: &files, Epoch: 7, Dest: "/app", Owner: "app", Group: "staff"},
			{Op: "run", Argv: []string{"make", "all"}},
			{Op: "env", Vars: []string{"A=1", "B=2"}},
			{Op: "workdir", Dir: "/app"},
		},
		Command: []string{"/app/server"},
		Source:  "/out",
		Output:  testSelection(t, []string{"dist"}, []string{"dist/*.map"}),
	}
	want := `{"version":8,"kind":"layer","base":"0123abcd","platform":"linux/arm64","steps":[` +
		`{"op":"add","files":{"digest":"` + files.Digest.String() + `","size":5},"epoch":7,"dest":"/app",` +
		`"owner":"app","group":"staff"},{"op":"run","argv":["make","all"]},{"op":"env","vars":["A=1","B=2"]},` +
		`{"op":"workdir","dir":"/app"}],"command":["/app/server"],"source":"/out","output":{"include":["dist"],` +
		`"exclude":["dist/*.map"],"mode_mask":493,"max_file_count":10000,"max_file_size":104857600,` +
		`"max_total_size":1073741824}}`
	if got := a.Key(); got != digest.FromString(want) {
		data, _ := json.Marshal(a)
		t.Errorf("Action encodes as\n%s\nwant\n%s", data, want)
	}
}

// TestActionKeys checks that an Action's key changes with its steps and their order.
func TestActionKeys(t *testing.T) {
	a := &Action{
		Version: ActionVersion,
		Kind:    "task",
		Base:    "Scratch",
		Steps: []Step{
			{Op: "env", Vars: []string{"LANG=C"}},
			{Op: "run", Argv: []string{"true"}},
		},
	}
	key := a.Key()
	b := *a
	b.Steps = []Step{a.Steps[0], {Op: "run", Argv: []string{"false"}}}
	if b.Key() == key {
		t.Errorf("actions with different commands have the same key")
	}
	b.Steps = []Step{a.Steps[1], a.Steps[0]}
	if b.Key() == key {
		t.Errorf("actions with steps in a different order have the same key")
	}
}
