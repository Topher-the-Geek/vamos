package sched

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/fileset"
	"github.com/Topher-the-Geek/vamos/internal/graph"
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
			{Op: "run", Argv: []string{"make", "all"}, Secrets: []graph.Secret{
				{ID: "token", Env: "TOKEN"}, {ID: "key", File: "/run/secrets/key"},
			}},
			{Op: "env", Vars: []string{"A=1", "B=2"}},
			{Op: "workdir", Dir: "/app"},
		},
		Command:    []string{"/app/server"},
		Entrypoint: &[]string{"java", "-jar"},
		Source:     "/out",
		Output:     testSelection(t, []string{"dist"}, []string{"dist/*.map"}),
	}
	want := `{"version":10,"kind":"layer","base":"0123abcd","platform":"linux/arm64","steps":[` +
		`{"op":"add","files":{"digest":"` + files.Digest.String() + `","size":5},"epoch":7,"dest":"/app",` +
		`"owner":"app","group":"staff"},{"op":"run","argv":["make","all"],"secrets":[{"id":"token","env":"TOKEN"},` +
		`{"id":"key","file":"/run/secrets/key"}]},{"op":"env","vars":["A=1","B=2"]},` +
		`{"op":"workdir","dir":"/app"}],"command":["/app/server"],"entrypoint":["java","-jar"],"source":"/out",` +
		`"output":{"include":["dist"],` +
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
	b.Steps = []Step{a.Steps[0], {Op: "run", Argv: []string{"true"}, Secrets: []graph.Secret{{ID: "t", Env: "T"}}}}
	if b.Key() == key {
		t.Errorf("actions whose steps are given different secrets have the same key")
	}
}

// TestEntrypointKeys checks that a layer's key tells an entrypoint left out, which keeps the base's, from an empty one,
// which removes it, and from a set one; and that an action without one has the key it had before entrypoints were.
func TestEntrypointKeys(t *testing.T) {
	a := &Action{Version: ActionVersion, Kind: "layer", Base: "Scratch", Platform: "linux/amd64"}
	keys := map[digest.Digest]string{a.Key(): "left out"}
	for name, entrypoint := range map[string][]string{"empty": {}, "set": {"java"}} {
		b := *a
		b.Entrypoint = &entrypoint
		if other, ok := keys[b.Key()]; ok {
			t.Errorf("an entrypoint %s has the key of one %s", name, other)
		}
		keys[b.Key()] = name
	}
	if data, _ := json.Marshal(a); strings.Contains(string(data), "entrypoint") {
		t.Errorf("an action without an entrypoint encodes as %s, which names one", data)
	}
}

// TestRequestSecretsNotEncoded checks that a request's secret values are never encoded, as for a worker's attempt.
func TestRequestSecretsNotEncoded(t *testing.T) {
	req := Request{Action: &Action{Version: ActionVersion}, Secrets: map[string][]byte{"token": []byte("hunter2")}}
	data, err := json.Marshal(req)
	testenv.Must(t, err)
	if strings.Contains(string(data), "hunter2") || strings.Contains(string(data), "token") {
		t.Errorf("a request encodes as %s, which holds its secrets", data)
	}
}
