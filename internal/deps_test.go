package internal_test

import (
	"os/exec"
	"strings"
	"testing"
)

// module is the path of vamos's module, which its packages' import paths start with.
const module = "github.com/Topher-the-Geek/vamos/internal/"

// TestImportRules checks which packages each of vamos's may depend on, directly or not: load must not reach buildah,
// so that it could run where buildah can't, run must not reach Starlark, so that a graph is all an executor needs,
// and api must reach neither buildah nor the container store, so that a client of a remote executor needs neither.
func TestImportRules(t *testing.T) {
	starlark := "go.starlark.net/..."
	buildah := "go.podman.io/buildah/..."
	// The container store itself; image references use a regexp package from the same module.
	storage := "go.podman.io/storage"
	rules := []struct {
		pkg      string
		internal []string // the packages of vamos it may use; nil for any
		not      []string // packages it must not use, or trees of them, ending in "/..."
	}{
		{
			pkg:      "cache",
			internal: []string{},
		},
		{
			pkg:      "cache/cachetest",
			internal: []string{"cache"},
			not:      []string{starlark, buildah, storage},
		},
		{
			pkg:      "tracing",
			internal: []string{},
		},
		{
			pkg:      "fileset",
			internal: []string{"cache"},
			not:      []string{starlark, buildah, storage},
		},
		{
			pkg:      "graph",
			internal: []string{"fileset", "cache"},
			not:      []string{starlark, buildah, storage},
		},
		{
			pkg:      "api",
			internal: []string{"cache", "fileset", "graph", "tracing"},
			not:      []string{starlark, buildah, storage},
		},
		{
			pkg:      "api/apitest",
			internal: []string{"api", "cache", "fileset", "graph", "tracing"},
			not:      []string{starlark, buildah, storage},
		},
		{
			pkg:      "sched",
			internal: []string{"api", "cache", "fileset", "graph", "tracing"},
			not:      []string{starlark, buildah, storage},
		},
		{
			pkg:      "remote",
			internal: []string{"api", "cache", "fileset", "graph", "sched", "tracing"},
			not:      []string{starlark, buildah, storage},
		},
		{
			pkg:      "coord",
			internal: []string{"api", "cache", "fileset", "graph", "remote", "sched", "tracing"},
			not:      []string{starlark, buildah, storage},
		},
		{
			pkg: "load",
			not: []string{buildah, storage, "go.podman.io/image/v5/copy", module + "run", module + "finish"},
		},
		{
			pkg:      "transfer",
			internal: []string{"cache", "tracing"},
			not:      []string{starlark, buildah, storage},
		},
		{
			pkg: "run",
			not: []string{starlark, module + "load", module + "finish"},
		},
		{
			pkg: "worker",
			not: []string{starlark, module + "load", module + "finish", module + "cli"},
		},
		{
			pkg: "finish",
			not: []string{starlark, buildah, storage, module + "load", module + "run"},
		},
	}
	for _, rule := range rules {
		t.Run(rule.pkg, func(t *testing.T) {
			out, err := exec.Command("go", "list", "-deps", module+rule.pkg).Output()
			if err != nil {
				t.Fatalf("go list: %v", err)
			}
			for _, dep := range strings.Fields(string(out)) {
				if dep == module+rule.pkg {
					continue
				}
				if name, ok := strings.CutPrefix(dep, module); ok && rule.internal != nil {
					allowed := false
					for _, p := range rule.internal {
						allowed = allowed || name == p
					}
					if !allowed {
						t.Errorf("%s uses %s; it may use only %q of vamos's packages", rule.pkg, dep, rule.internal)
					}
				}
				for _, not := range rule.not {
					tree, ok := strings.CutSuffix(not, "/...")
					if dep == not || ok && (dep == tree || strings.HasPrefix(dep, tree+"/")) {
						t.Errorf("%s uses %s, which it must not", rule.pkg, dep)
					}
				}
			}
		})
	}
}
