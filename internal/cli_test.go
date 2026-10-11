package internal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/afero"
)

// validBuild is a BUILD file that would build, were the command line right.
var validBuild = map[string]string{
	"BUILD": pullBusybox + `layer(name="app", base="busybox", command=["echo", "hello"])`,
}

func TestCommandLineMistakes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "push without image",
			args: []string{"push"},
			want: []string{"missing image name", "vamos push ghcr.io/me/app:1.0"},
		},
		{
			name: "load without image",
			args: []string{"load"},
			want: []string{"missing image name", "vamos load app:dev"},
		},
		{
			name: "write without file",
			args: []string{"write"},
			want: []string{"missing output file", "vamos write app.tar"},
		},
		{
			name: "push with two images",
			args: []string{"push", "app:1", "app:2"},
			want: []string{"expected one image name, got 2 arguments: app:1 app:2"},
		},
		{
			name: "write with two files",
			args: []string{"write", "a.tar", "b.tar"},
			want: []string{"expected one output file, got 2 arguments"},
		},
		{
			name: "unknown flag",
			args: []string{"load", "app", "--bogus"},
			want: []string{"unknown flag: --bogus"},
		},
		{
			name: "unknown shorthand",
			args: []string{"push", "-x", "app"},
			want: []string{"unknown shorthand flag: 'x'"},
		},
		{
			name: "tag without value",
			args: []string{"write", "app.tar", "--tag"},
			want: []string{"flag needs an argument: --tag"},
		},
		{
			name: "tag on push",
			args: []string{"push", "app", "--tag", "app:1"},
			want: []string{"unknown flag: --tag"},
		},
		{
			name: "dir without value",
			args: []string{"load", "app", "-C"},
			want: []string{"flag needs an argument: 'C'"},
		},
		{
			name: "tls-verify not a bool",
			args: []string{"push", "app", "--tls-verify=maybe"},
			want: []string{`invalid argument "maybe"`, "--tls-verify"},
		},
		{
			name: "build without steps",
			args: []string{"build"},
			want: []string{"missing step name", "vamos build packages"},
		},
		{
			name: "rebuild on push",
			args: []string{"push", "app", "--rebuild"},
			want: []string{"unknown flag: --rebuild"},
		},
		{
			name: "prune with argument",
			args: []string{"prune", "all"},
			want: []string{"vamos prune takes no arguments, got: all"},
		},
		{
			name: "timestamp not a time",
			args: []string{"write", "app.tar", "--timestamp", "yesterday"},
			want: []string{`invalid argument "yesterday"`, "--timestamp", "want Unix seconds"},
		},
		{
			name: "timestamp before 1970",
			args: []string{"build", "app", "--timestamp=-1"},
			want: []string{`invalid argument "-1"`, "must not be before 1970"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vamos(t, project(t, validBuild), tt.args...).failsWith(t, true, tt.want...)
		})
	}
}

// TestBuildStepMistakes checks that naming steps `vamos build` can't build is reported before
// anything is built.
func TestBuildStepMistakes(t *testing.T) {
	build := pullBusybox + `
glob(name="code", include=["app.sh"])
layer(name="packages", base="busybox", sources=["code"], destination="/app")
`
	tests := []struct {
		name  string
		steps []string
		want  []string
	}{
		{
			name:  "misspelled step",
			steps: []string{"pakcages"},
			want:  []string{`/project/BUILD has no step "pakcages"; did you mean "packages"?`},
		},
		{
			name:  "unknown step",
			steps: []string{"packages", "server"},
			want:  []string{`/project/BUILD has no step "server"; its steps are "busybox", "code", "packages"`},
		},
		{
			name:  "glob",
			steps: []string{"code"},
			want:  []string{`"code" is a glob(), whose files are read by every build`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := project(t, map[string]string{"BUILD": build, "app.sh": ""})
			args := append([]string{"build", "-C", projectDir}, tt.steps...)
			vamos(t, fsys, args...).failsWith(t, false, tt.want...)
		})
	}
}

func TestUnknownCommands(t *testing.T) {
	vamos(t, project(t, validBuild), "bulid").
		failsWith(t, true, `unknown command "bulid"`)
	vamos(t, project(t, validBuild), "pusj", "app").
		failsWith(t, true, `unknown command "pusj"; did you mean "push"?`)
}

func TestNoCommandShowsHelp(t *testing.T) {
	r := vamos(t, project(t, validBuild))
	if r.err != nil {
		t.Fatal(r.err)
	}
	for _, want := range []string{"build", "push", "load", "write", "prune"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("help = %q, want it to list %q", r.stdout, want)
		}
	}
}

func TestInvalidImageNames(t *testing.T) {
	out := filepath.Join(t.TempDir(), "app.tar")
	digest := "@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "uppercase push",
			args: []string{"push", "MyApp"},
			want: []string{`invalid image name "MyApp"`, "must be lowercase"},
		},
		{
			name: "uppercase load",
			args: []string{"load", "MyApp"},
			want: []string{`invalid image name "MyApp"`, "must be lowercase"},
		},
		{
			name: "space",
			args: []string{"load", "my app"},
			want: []string{`invalid image name "my app"`},
		},
		{
			name: "empty tag",
			args: []string{"load", "app:"},
			want: []string{`invalid image name "app:"`},
		},
		{
			name: "digest",
			args: []string{"push", "app" + digest},
			want: []string{"must not include a digest"},
		},
		{
			name: "invalid write tag",
			args: []string{"write", out, "-t", "Bad Tag"},
			want: []string{`invalid image name "Bad Tag"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append(tt.args, "-C", projectDir)
			vamos(t, project(t, validBuild), args...).failsWith(t, false, tt.want...)
		})
	}
	assertNoFiles(t, filepath.Dir(out))
}

func TestProjectDirectoryMistakes(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/docker.sock")
	t.Run("missing directory", func(t *testing.T) {
		vamos(t, project(t, validBuild), "load", "app", "-C", "/nope").
			failsWith(t, false, "directory /nope does not exist")
	})
	t.Run("directory is a file", func(t *testing.T) {
		vamos(t, project(t, validBuild), "load", "app", "-C", "/project/BUILD").
			failsWith(t, false, "/project/BUILD is not a directory")
	})
	t.Run("no BUILD file", func(t *testing.T) {
		vamos(t, project(t, map[string]string{"script.sh": "echo hi"}), "load", "app", "-C", projectDir).
			failsWith(t, false, "no BUILD file in /project")
	})
	t.Run("relative directory", func(t *testing.T) {
		// -C resolves against the working directory, as other tools do.
		wd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		vamos(t, afero.NewMemMapFs(), "load", "app", "-C", "sub").
			failsWith(t, false, "directory "+filepath.Join(wd, "sub")+" does not exist")
	})
}

func TestBuildFileMistakes(t *testing.T) {
	// Steps follow pullBusybox, so they start on line 2, or line 3 after withGlob.
	const withGlob = `glob(name="code", include=["app.sh"])` + "\n"
	tests := []struct {
		name  string
		build string
		files map[string]string
		want  []string
	}{
		{
			name:  "syntax error",
			build: pullBusybox + "layer(\n    name=\"app\",\n    base=\"busybox\",\n    command=[\"echo\", \"hi\"\n)\n",
			want:  []string{"/project/BUILD:6:", "got ')', want ']'"},
		},
		{
			name:  "misspelled function",
			build: pullBusybox + `bulid(name="app", base="busybox")`,
			want:  []string{"/project/BUILD:2:1:", "undefined: bulid"},
		},
		{
			name:  "misspelled argument",
			build: pullBusybox + `layer(name="app", base="busybox", comand=["echo"])`,
			want:  []string{"/project/BUILD:2:6:", `unexpected keyword argument "comand"`},
		},
		{
			name:  "missing name",
			build: pullBusybox + `layer(base="busybox")`,
			want:  []string{"/project/BUILD:2:6:", "missing argument for name"},
		},
		{
			name:  "empty name",
			build: pullBusybox + `layer(name="", base="busybox")`,
			want:  []string{"/project/BUILD:2:6:", "layer: name must not be empty"},
		},
		{
			name:  "missing base",
			build: pullBusybox + `layer(name="app", command=["echo"])`,
			want:  []string{"/project/BUILD:2:6:", "missing argument for base"},
		},
		{
			name:  "unknown base",
			build: pullBusybox + `layer(name="app", base="busybx")`,
			want: []string{
				"/project/BUILD:2:6:",
				`layer: base "busybx" is not "scratch" or an earlier pull() or layer(); earlier steps are "busybox"`,
			},
		},
		{
			name:  "base names an image instead of a step",
			build: `layer(name="app", base="docker.io/library/busybox:1.36")`,
			want: []string{
				"/project/BUILD:1:6:",
				`base "docker.io/library/busybox:1.36" is not "scratch" or an earlier pull() or layer(); ` +
					"there are none before this",
			},
		},
		{
			name:  "base declared later",
			build: pullBusybox + "layer(name=\"app\", base=\"deps\")\nlayer(name=\"deps\", base=\"busybox\")\n",
			want:  []string{"/project/BUILD:2:6:", `base "deps" is not "scratch" or an earlier pull() or layer()`},
		},
		{
			name:  "name used twice",
			build: pullBusybox + `layer(name="busybox", base="busybox")`,
			want:  []string{"/project/BUILD:2:6:", `layer: name "busybox" is already used at /project/BUILD:1:5`},
		},
		{
			name:  "step named scratch",
			build: `pull(name="scratch", image="busybox")`,
			want:  []string{"/project/BUILD:1:5:", `pull: name "scratch" is reserved for the empty base image`},
		},
		{
			name:  "pull without image",
			build: `pull(name="busybox")`,
			want:  []string{"/project/BUILD:1:5:", "missing argument for image"},
		},
		{
			name:  "empty image",
			build: `pull(name="busybox", image="")`,
			want:  []string{"/project/BUILD:1:5:", "pull: image must not be empty"},
		},
		{
			name:  "invalid image",
			build: `pull(name="busybox", image="Busy Box")`,
			want:  []string{"/project/BUILD:1:5:", `pull: invalid image "Busy Box"`},
		},
		{
			name:  "tag in image",
			build: `pull(name="busybox", image="busybox:1.36")`,
			want:  []string{"/project/BUILD:1:5:", `image "busybox:1.36" must not include a tag; use tag="1.36"`},
		},
		{
			name:  "digest in image",
			build: `pull(name="busybox", image="busybox@sha256:` + strings.Repeat("0", 64) + `")`,
			want:  []string{"/project/BUILD:1:5:", "must not include a digest"},
		},
		{
			name:  "invalid tag",
			build: `pull(name="busybox", image="busybox", tag="1 36")`,
			want:  []string{"/project/BUILD:1:5:", `pull: invalid tag "1 36"`},
		},
		{
			name:  "empty pull name",
			build: `pull(name="", image="busybox")`,
			want:  []string{"/project/BUILD:1:5:", "pull: name must not be empty"},
		},
		{
			name:  "unused pull",
			build: pullBusybox + "pull(name=\"alpine\", image=\"alpine\")\nlayer(name=\"app\", base=\"busybox\")\n",
			want:  []string{"/project/BUILD:2:5: pull: \"alpine\" is not the base of any layer()"},
		},
		{
			name:  "unused layer",
			build: pullBusybox + "layer(name=\"test\", base=\"busybox\")\nlayer(name=\"app\", base=\"busybox\")\n",
			want: []string{
				`/project/BUILD:2:6: layer: "test" is not the base of any later layer() or task(); ` +
					"only the last layer() makes the image",
			},
		},
		{
			name:  "sources not a list",
			build: pullBusybox + withGlob + `layer(name="app", base="busybox", sources="code", destination="/app")`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{"/project/BUILD:3:6:", "sources", "got string, want list"},
		},
		{
			name:  "source names a file",
			build: pullBusybox + `layer(name="app", base="busybox", sources=["app.sh"], destination="/app")`,
			files: map[string]string{"app.sh": "echo hi"},
			want: []string{
				"/project/BUILD:2:6:",
				`layer: source "app.sh" is not an earlier glob() or task(); there are none before this`,
			},
		},
		{
			name:  "unknown source",
			build: pullBusybox + withGlob + `layer(name="app", base="busybox", sources=["cod"], destination="/app")`,
			files: map[string]string{"app.sh": "echo hi"},
			want: []string{
				"/project/BUILD:3:6:",
				`source "cod" is not an earlier glob() or task(); earlier globs and tasks are "code"`,
			},
		},
		{
			name:  "source names a pull",
			build: pullBusybox + `layer(name="app", base="busybox", sources=["busybox"], destination="/app")`,
			want:  []string{"/project/BUILD:2:6:", `layer: source "busybox" is a pull(), not a glob() or task()`},
		},
		{
			name:  "base names a glob",
			build: pullBusybox + withGlob + `layer(name="app", base="code")`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{"/project/BUILD:3:6:", `layer: base "code" is a glob(), not a pull() or layer()`},
		},
		{
			name:  "destination not a string",
			build: pullBusybox + withGlob + `layer(name="app", base="busybox", sources=["code"], destination=["/app"])`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{"/project/BUILD:3:6:", "destination", "got list, want string"},
		},
		{
			name:  "command item not a string",
			build: pullBusybox + `layer(name="app", base="busybox", command=["sleep", 5])`,
			want:  []string{"/project/BUILD:2:6:", "command[1] is int, want string"},
		},
		{
			name:  "sources without destination",
			build: pullBusybox + withGlob + `layer(name="app", base="busybox", sources=["code"])`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{"destination is required when sources are given"},
		},
		{
			name:  "relative destination",
			build: pullBusybox + withGlob + `layer(name="app", base="busybox", sources=["code"], destination="app")`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{`destination "app" must be an absolute path`},
		},
		{
			name:  "glob without include",
			build: `glob(name="code")`,
			want:  []string{"/project/BUILD:1:5:", "missing argument for include"},
		},
		{
			name:  "empty include",
			build: `glob(name="code", include=[])`,
			want:  []string{"/project/BUILD:1:5:", "glob: include must not be empty"},
		},
		{
			name:  "empty include path",
			build: `glob(name="code", include=["app.sh", ""])`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{"/project/BUILD:1:5:", `glob: include[1] is empty; use "." for the whole directory`},
		},
		{
			name:  "empty task include path",
			build: pullBusybox + `task(name="make", base="busybox", source="/out", include=[""])`,
			want:  []string{"/project/BUILD:2:5:", `task: include[0] is empty; use "." for the whole directory`},
		},
		{
			name:  "include item not a string",
			build: `glob(name="code", include=[1])`,
			want:  []string{"/project/BUILD:1:5:", "glob: include[0] is int, want string"},
		},
		{
			name:  "include pattern",
			build: `glob(name="code", include=["*.sh"])`,
			files: map[string]string{"app.sh": "echo hi"},
			want: []string{
				"/project/BUILD:1:5:",
				`glob: include "*.sh": patterns are not supported yet; list each file`,
			},
		},
		{
			name:  "missing file",
			build: `glob(name="code", include=["app.sh"])`,
			want:  []string{"/project/BUILD:1:5:", `glob: include "app.sh" not found in /project`},
		},
		{
			name:  "absolute include",
			build: `glob(name="code", include=["/etc/passwd"])`,
			want:  []string{`include "/etc/passwd" must be a path relative to /project`},
		},
		{
			name:  "include outside project",
			build: `glob(name="code", include=["../secret"])`,
			want:  []string{`include "../secret" is outside /project`},
		},
		{
			name:  "empty glob name",
			build: `glob(name="", include=["app.sh"])`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{"/project/BUILD:1:5:", "glob: name must not be empty"},
		},
		{
			name:  "glob name used twice",
			build: pullBusybox + `glob(name="busybox", include=["app.sh"])`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{"/project/BUILD:2:5:", `glob: name "busybox" is already used at /project/BUILD:1:5`},
		},
		{
			name:  "unused glob",
			build: pullBusybox + withGlob + `layer(name="app", base="busybox")`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{`/project/BUILD:2:5: glob: "code" is not in the sources of any later layer() or task()`},
		},
		{
			name:  "task without source",
			build: pullBusybox + `task(name="make", base="busybox", include=["app"])`,
			want:  []string{"/project/BUILD:2:5:", "missing argument for source"},
		},
		{
			name:  "task without include",
			build: pullBusybox + `task(name="make", base="busybox", source="/out")`,
			want:  []string{"/project/BUILD:2:5:", "missing argument for include"},
		},
		{
			name:  "relative task source",
			build: pullBusybox + `task(name="make", base="busybox", source="out", include=["app"])`,
			want:  []string{"/project/BUILD:2:5:", `task: source "out" must be an absolute path`},
		},
		{
			name:  "task include outside source",
			build: pullBusybox + `task(name="make", base="busybox", source="/out", include=["../etc"])`,
			want:  []string{"/project/BUILD:2:5:", `task: include "../etc" is outside /out`},
		},
		{
			name:  "task include pattern",
			build: pullBusybox + `task(name="make", base="busybox", source="/out", include=["*.js"])`,
			want:  []string{"/project/BUILD:2:5:", `task: include "*.js": patterns are not supported yet`},
		},
		{
			name:  "task command not a list",
			build: pullBusybox + `task(name="make", base="busybox", command="make", source="/out", include=["app"])`,
			want:  []string{"/project/BUILD:2:5:", "command", "got string, want list"},
		},
		{
			name: "base names a task",
			build: pullBusybox + `task(name="make", base="busybox", source="/out", include=["app"])` + "\n" +
				`layer(name="app", base="make")`,
			want: []string{"/project/BUILD:3:6:", `layer: base "make" is a task(), not a pull() or layer()`},
		},
		{
			name: "unused task",
			build: pullBusybox + `task(name="make", base="busybox", source="/out", include=["app"])` + "\n" +
				`layer(name="app", base="busybox")`,
			want: []string{`/project/BUILD:2:5: task: "make" is not in the sources of any later layer() or task()`},
		},
		{
			name: "task after the last layer",
			build: pullBusybox + `glob(name="code", include=["app.sh"])` + "\n" +
				`layer(name="app", base="busybox", sources=["code"], destination="/app")` + "\n" +
				`task(name="make", base="app", source="/app", include=["app.sh"])`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{`/project/BUILD:4:5: task: "make" is not in the sources of any later layer() or task()`},
		},
		{
			name: "glob and task include the same file",
			build: pullBusybox + withGlob +
				`task(name="make", base="busybox", source="/out", include=["app.sh"])` + "\n" +
				`layer(name="app", base="busybox", sources=["code", "make"], destination="/app")`,
			files: map[string]string{"app.sh": "echo hi"},
			want: []string{
				"/project/BUILD:4:6:",
				`layer: sources "code" and "make" both include "app.sh"`,
			},
		},
		{
			name: "task output within a glob's directory",
			build: pullBusybox + `glob(name="code", include=["."])` + "\n" +
				`task(name="make", base="busybox", source="/out", include=["gen/app.sh"])` + "\n" +
				`task(name="use", base="busybox", sources=["make", "code"], destination="/app", ` +
				`source="/app", include=["."])` + "\n" +
				`layer(name="app", base="busybox", sources=["use"], destination="/app")`,
			want: []string{
				"/project/BUILD:4:5:",
				`task: sources "make" and "code" both include "gen/app.sh"`,
			},
		},
		{
			name:  "only tasks",
			build: pullBusybox + `task(name="make", base="busybox", source="/out", include=["app"])`,
			want:  []string{"/project/BUILD: layer() was never called"},
		},
		{
			name:  "layer never called",
			build: pullBusybox,
			want:  []string{"/project/BUILD: layer() was never called"},
		},
		{
			name:  "empty BUILD",
			build: "",
			want:  []string{"layer() was never called"},
		},
		{
			name:  "fail in BUILD",
			build: `fail("unsupported platform")`,
			want:  []string{"/project/BUILD:1:5:", "unsupported platform"},
		},
		{
			name: "error in a helper function",
			build: pullBusybox + `
def app(src):
    glob(name="code", include=[src])

app("main.sh")
`,
			want: []string{"/project/BUILD:4:9:", `include "main.sh" not found`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string]string{"BUILD": tt.build}
			for name, content := range tt.files {
				files[name] = content
			}
			out := t.TempDir()
			vamos(t, project(t, files), "write", filepath.Join(out, "app.tar"), "-C", projectDir).
				failsWith(t, false, tt.want...)
			assertNoFiles(t, out)
		})
	}
}

func TestWriteDestinationMistakes(t *testing.T) {
	dir := t.TempDir()
	t.Run("missing directory", func(t *testing.T) {
		path := filepath.Join(dir, "nope", "app.tar")
		vamos(t, project(t, validBuild), "write", path, "-C", projectDir).
			failsWith(t, false, "cannot write "+path+": directory "+filepath.Join(dir, "nope")+" does not exist")
	})
	t.Run("path is a directory", func(t *testing.T) {
		vamos(t, project(t, validBuild), "write", dir, "-C", projectDir).
			failsWith(t, false, "cannot write "+dir+": it is a directory")
	})
	t.Run("parent is a file", func(t *testing.T) {
		file := filepath.Join(dir, "file")
		if err := os.WriteFile(file, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		vamos(t, project(t, validBuild), "write", filepath.Join(file, "app.tar"), "-C", projectDir).
			failsWith(t, false, file+" is not a directory")
	})
}

func TestMissingAuthFile(t *testing.T) {
	t.Setenv("REGISTRY_AUTH_FILE", "/nonexistent/auth.json")

	vamos(t, project(t, validBuild), "push", "app", "-C", projectDir).
		failsWith(t, false, "REGISTRY_AUTH_FILE", "/nonexistent/auth.json", "no such file")
}

// TestSymlinkSource rejects an include that is itself a link.
func TestSymlinkSource(t *testing.T) {
	fsys, _ := diskProject(t, map[string]string{
		"BUILD": `glob(name="code", include=["lib"])`,
	}, map[string]string{"lib": "../shared"})
	out := t.TempDir()

	vamos(t, fsys, "write", filepath.Join(out, "app.tar"), "-C", projectDir).
		failsWith(t, false, "/project/BUILD:1:5:", `include "lib" is a symbolic link`)
	assertNoFiles(t, out)
}

// TestSymlinkSourceParent rejects links at every depth of an included path.
func TestSymlinkSourceParent(t *testing.T) {
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "secret"), []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, parent := range []string{"external", "nested/external"} {
		t.Run(parent, func(t *testing.T) {
			src := parent + "/secret"
			fsys, _ := diskProject(t, map[string]string{
				"BUILD": `glob(name="code", include=["` + src + `"])`,
			}, map[string]string{parent: external})
			out := t.TempDir()

			vamos(t, fsys, "write", filepath.Join(out, "app.tar"), "-C", projectDir).
				failsWith(t, false, "/project/BUILD:1:5:",
					`include "`+src+`" has symbolic link parent "`+filepath.Join(projectDir, parent)+`"`)
			assertNoFiles(t, out)
		})
	}
}

// assertNoFiles fails the test if dir holds anything, such as a partly written image.
func assertNoFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("unexpected file left in %s: %s", dir, e.Name())
	}
}
