package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/spf13/afero"
)

// TestWorkspace builds a target whose BUILD file uses targets of another package's:
// its base, a glob's files, relative to that package, and a task's.
// Targets nothing needs are neither run nor checked.
func TestWorkspace(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	fsys := project(t, map[string]string{
		"WORKSPACE": "",
		"lib/BUILD": pullBusybox + `
glob(name="code", include=["lib.sh"])
glob(name="docs", include=["README"]) # missing, but nothing needs it

task(
    name="make",
    base="busybox",
    steps=[add("code", "/src"), run(["sh", "-c", "mkdir /out && cp /src/lib.sh /out/made.sh"])],
    source="/out",
    include=["made.sh"],
)
`,
		"lib/lib.sh": "echo " + nonce,
		"app/BUILD": `
glob(name="code", include=["app.sh"])

layer(
    name="app",
    base="//lib:busybox",
    steps=[add("code", "/app"), add("//lib:make", "/app"), add("//lib:code", "/lib")],
)

task(name="test", base="app", steps=[run(["false"])], source="/app", include=["app.sh"])
`,
		"app/app.sh": "echo app",
	})
	out := filepath.Join(t.TempDir(), "app.tar")

	r := vamos(t, fsys, "save", "app", out, "-C", filepath.Join(projectDir, "app")).succeeds(t)

	if !strings.Contains(r.stdout, `task "//lib:make": building`) {
		t.Errorf("stdout = %q, want another package's target named by its label", r.stdout)
	}
	if strings.Contains(r.stdout, `task "test"`) {
		t.Errorf("stdout = %q, want the task nothing needs left out", r.stdout)
	}
	img, err := tarball.ImageFromPath(out, nil)
	must(t, err)
	want := []string{"/app/app.sh", "/app/made.sh"}
	if got := imagePaths(t, img, "/app"); !slices.Equal(got, want) {
		t.Errorf("files in /app = %q, want %q", got, want)
	}
	if got := imageFile(t, img, "/lib/lib.sh"); got != "echo "+nonce {
		t.Errorf("/lib/lib.sh = %q, want the file from lib/", got)
	}

	// From the root, every target is named by its label.
	r = vamos(t, fsys, "build", "//app:app", "-C", projectDir).ran(t)
	if !strings.Contains(r.stdout, `layer "//app:app": cached`) {
		t.Errorf("stdout = %q, want the layer reused", r.stdout)
	}
}

// TestWorkspaceRoot checks which directory labels are relative to:
// the nearest one with a WORKSPACE file at or above -C, or else -C itself.
func TestWorkspaceRoot(t *testing.T) {
	lib := pullBusybox + `glob(name="code", include=["lib.sh"])`
	app := `layer(name="app", base="//lib:nope")`
	tests := []struct {
		name  string
		files map[string]string
		dir   string
		want  string
	}{
		{
			name:  "WORKSPACE above -C",
			files: map[string]string{"WORKSPACE": "", "lib/BUILD": lib, "app/BUILD": app},
			dir:   "app",
			want:  `base "//lib:nope": /project/lib/BUILD has no target "nope"`,
		},
		{
			name:  "WORKSPACE in -C",
			files: map[string]string{"app/WORKSPACE": "", "app/lib/BUILD": lib, "app/BUILD": app},
			dir:   "app",
			want:  `base "//lib:nope": /project/app/lib/BUILD has no target "nope"`,
		},
		{
			name:  "no WORKSPACE",
			files: map[string]string{"lib/BUILD": lib, "app/BUILD": app, "app/lib/BUILD": lib},
			dir:   "app",
			want:  `base "//lib:nope": /project/app/lib/BUILD has no target "nope"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vamos(t, project(t, tt.files), "build", "app", "-C", filepath.Join(projectDir, tt.dir)).
				failsWith(t, false, "/project/"+tt.dir+"/BUILD:1:6: layer: "+tt.want)
		})
	}
}

// TestWorkspaceMistakes checks the errors for labels that don't name a target the BUILD file can use.
func TestWorkspaceMistakes(t *testing.T) {
	lib := pullBusybox + `
glob(name="code", include=["lib.sh"])
task(name="make", base="busybox", source="/out", include=["made"])
`
	tests := []struct {
		name  string
		app   string
		files map[string]string
		want  []string
	}{
		{
			name: "no BUILD file in the package",
			app:  `layer(name="app", base="//nope:busybox")`,
			want: []string{
				`/project/app/BUILD:1:6: layer: base "//nope:busybox": no BUILD or BUILD.vms file in /project/nope`,
			},
		},
		{
			name: "misspelled target",
			app:  `layer(name="app", base="scratch", steps=[add("//lib:cdoe", "/app")])`,
			want: []string{
				`/project/app/BUILD:1:45: add: source "//lib:cdoe": /project/lib/BUILD has no target "cdoe"; ` +
					`did you mean "code"?`,
			},
		},
		{
			name: "base names a glob",
			app:  `layer(name="app", base="//lib:code")`,
			want: []string{`/project/app/BUILD:1:6: layer: base "//lib:code" is a glob(), not a pull() or layer()`},
		},
		{
			name: "source names a pull",
			app:  `layer(name="app", base="scratch", steps=[add("//lib:busybox", "/app")])`,
			want: []string{`add: source "//lib:busybox" is a pull(), not a glob() or task()`},
		},
		{
			name: "label without a name",
			app:  `layer(name="app", base="//lib")`,
			want: []string{`/project/app/BUILD:1:6: layer: base "//lib": label "//lib" must end with ":"`},
		},
		{
			name: "label outside the workspace",
			app:  `layer(name="app", base="//../lib:busybox")`,
			want: []string{
				`label "//../lib:busybox": "../lib" is not a clean path to a directory within the workspace`,
			},
		},
		{
			name: "label without its leading slashes",
			app:  `layer(name="app", base="scratch", steps=[add("lib:code", "/app")])`,
			want: []string{
				`add: source "lib:code" is not an earlier glob() or task(); a label starts with "//", ` +
					`as in "//lib:code"`,
			},
		},
		{
			name: "label naming the root as a dot",
			app:  `layer(name="app", base="//.:busybox")`,
			want: []string{`label "//.:busybox": "." is not a clean path`},
		},
		{
			name: "label with an unclean path",
			app:  `layer(name="app", base="//lib/:busybox")`,
			want: []string{`label "//lib/:busybox": "lib/" is not a clean path`},
		},
		{
			name: "name with a slash",
			app:  `layer(name="app/server", base="scratch")`,
			want: []string{`/project/app/BUILD:1:6: layer: name "app/server" must not contain ":" or "/"`},
		},
		{
			name: "packages in a cycle",
			app:  `layer(name="app", base="//lib:busybox")`,
			files: map[string]string{
				"lib/BUILD": `layer(name="base", base="//app:app")`,
			},
			want: []string{
				`/project/lib/BUILD:1:6: layer: base "//app:app": packages name each other's targets in a cycle: ` +
					"//app: -> //lib: -> //app:",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string]string{"WORKSPACE": "", "lib/BUILD": lib, "lib/lib.sh": "", "app/BUILD": tt.app}
			for name, content := range tt.files {
				files[name] = content
			}
			vamos(t, project(t, files), "build", "app", "-C", filepath.Join(projectDir, "app")).
				failsWith(t, false, tt.want...)
		})
	}
}

// TestWorkspaceErrorInOtherPackage checks that an error in a BUILD file loaded for another's label is reported
// where it is, as if that file were built itself.
func TestWorkspaceErrorInOtherPackage(t *testing.T) {
	fsys := project(t, map[string]string{
		"WORKSPACE": "",
		"lib/BUILD": pullBusybox + `layer(name="base", base="busybx")`,
		"app/BUILD": `layer(name="app", base="//lib:base")`,
	})
	r := vamos(t, fsys, "build", "app", "-C", filepath.Join(projectDir, "app")).
		failsWith(t, false, `/project/lib/BUILD:2:6: layer: base "busybx" is not "scratch" or an earlier pull()`)
	if strings.Contains(r.err.Error(), "app/BUILD") {
		t.Errorf("error = %q, want only where it is in lib/BUILD", r.err)
	}
}

// TestWorkspaceTargetMistakes checks the errors for targets named on the command line by label.
func TestWorkspaceTargetMistakes(t *testing.T) {
	fsys := project(t, map[string]string{
		"WORKSPACE": "",
		"lib/BUILD": pullBusybox + `glob(name="code", include=["lib.sh"])` + "\n" +
			`task(name="make", base="busybox", source="/out", include=["made"])`,
	})
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "no BUILD file in the package",
			args: []string{"build", "//nope:app"},
			want: "no BUILD or BUILD.vms file in /project/nope",
		},
		{
			name: "no BUILD file in -C",
			args: []string{"build", "app"},
			want: "no BUILD or BUILD.vms file in /project",
		},
		{
			name: "misspelled target",
			args: []string{"build", "//lib:mkae"},
			want: `/project/lib/BUILD has no target "mkae"; did you mean "make"?`,
		},
		{
			name: "label without a name",
			args: []string{"build", "//lib"},
			want: `label "//lib" must end with ":" and a target's name`,
		},
		{
			name: "send a glob",
			args: []string{"save", "//lib:code", "app.tar"},
			want: `"//lib:code" is a glob(), whose files are read by the targets that add them, not an image`,
		},
		{
			name: "send a task",
			args: []string{"load", "//lib:make", "app"},
			want: `"//lib:make" is a task(), whose image is discarded once it runs; ` +
				"name a pull(), layer() or index() to send",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append(tt.args, "-C", projectDir)
			vamos(t, fsys, args...).failsWith(t, false, tt.want)
		})
	}
}

// TestLabelsInOwnPackage checks that a BUILD file can name its own targets by label too,
// as long as they are declared earlier.
func TestLabelsInOwnPackage(t *testing.T) {
	for _, ref := range []string{":base", "//app:base"} {
		t.Run(ref, func(t *testing.T) {
			fsys := project(t, map[string]string{
				"WORKSPACE": "",
				"app/BUILD": fmt.Sprintf(`layer(name="app", base="%s")`+"\n"+`layer(name="base", base="scratch")`, ref),
			})
			vamos(t, fsys, "build", "app", "-C", filepath.Join(projectDir, "app")).
				failsWith(t, false, fmt.Sprintf(`layer: base %q is not "scratch" or an earlier pull() or layer()`, ref))
		})
	}
}

// TestWorkspaceSearchError checks that a WORKSPACE file that can't be checked stops the search
// for the workspace's root, rather than letting a directory further up, or -C, become the root.
func TestWorkspaceSearchError(t *testing.T) {
	fsys := &deniedFs{
		Fs:   project(t, map[string]string{"WORKSPACE": "", "app/BUILD": `layer(name="app", base="scratch")`}),
		name: filepath.Join(projectDir, "app", "WORKSPACE"),
	}
	vamos(t, fsys, "build", "app", "-C", filepath.Join(projectDir, "app")).
		failsWith(t, false, "looking for a WORKSPACE file: stat /project/app/WORKSPACE: permission denied")
}

// deniedFs is a filesystem in which the file name can't be checked, as when its directory isn't searchable.
// Tests run as root, which permissions on disk don't stop.
type deniedFs struct {
	afero.Fs
	name string
}

// Stat describes the file path, or fails for name.
func (f *deniedFs) Stat(path string) (fs.FileInfo, error) {
	if path == f.name {
		return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrPermission}
	}
	return f.Fs.Stat(path)
}

// TestPackageLinks checks that a label can't reach a package through a symbolic link,
// which could lead outside the workspace, just as a glob can't include a file through one.
func TestPackageLinks(t *testing.T) {
	fsys, root := diskProject(t, map[string]string{
		"WORKSPACE": "",
		"app/BUILD": `layer(name="app", base="scratch", steps=[add("//vendor/lib:code", "/lib")])`,
	}, map[string]string{"vendor": filepath.Join(t.TempDir(), "outside")})
	outside, err := os.Readlink(filepath.Join(root, projectDir, "vendor"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	build := `glob(name="code", include=["lib.sh"])`
	if err := os.WriteFile(filepath.Join(outside, "lib", "BUILD"), []byte(build), 0o644); err != nil {
		t.Fatal(err)
	}
	vamos(t, fsys, "build", "app", "-C", filepath.Join(projectDir, "app")).failsWith(t, false,
		`/project/app/BUILD:1:45: add: source "//vendor/lib:code": package "vendor/lib": /project/vendor `+
			"is a symbolic link; a package must be a directory within the workspace")
}

// TestBuildVms checks that a package's BUILD file may be named BUILD.vms, for a directory beside a build directory,
// which BUILD would clash with where names are compared without case: as the package of -C, and named by a label;
// that a directory by either name, as build is found as BUILD there, isn't a BUILD file; and that a package with both
// names is refused, naming its directory, rather than one being silently ignored.
func TestBuildVms(t *testing.T) {
	files := map[string]string{
		"WORKSPACE":       "",
		"BUILD.vms":       `layer(name="app", base="//react:site")`,
		"react/BUILD.vms": `layer(name="site", base="scratch", command=["serve"])`,
		"react/BUILD/out": "a build directory, as build is found as BUILD without case",
	}
	r := vamos(t, project(t, files), "graph", "app", "-C", projectDir).ran(t)
	for _, label := range []string{`"label": "app"`, `"label": "//react:site"`} {
		if !strings.Contains(r.stdout, label) {
			t.Errorf("the graph doesn't have %s:\n%s", label, r.stdout)
		}
	}
	vamos(t, project(t, files), "graph", "site", "-C", filepath.Join(projectDir, "react")).ran(t)

	files["react/BUILD.vms"], files["react/BUILD"] = files["react/BUILD.vms"], files["react/BUILD.vms"]
	delete(files, "react/BUILD/out")
	vamos(t, project(t, files), "graph", "app", "-C", projectDir).
		failsWith(t, false, `/project/BUILD.vms:1:6: layer: base "//react:site": /project/react has both BUILD and `+
			"BUILD.vms; a package has one BUILD file, so remove one of them")
}
