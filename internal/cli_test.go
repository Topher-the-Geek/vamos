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
	"BUILD": `build(base="` + busybox + `", command=["echo", "hello"])`,
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vamos(t, project(t, validBuild), tt.args...).failsWith(t, true, tt.want...)
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
	for _, want := range []string{"push", "load", "write"} {
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
	tests := []struct {
		name  string
		build string
		files map[string]string
		want  []string
	}{
		{
			name:  "syntax error",
			build: "build(\n    base=\"busybox\",\n    command=[\"echo\", \"hi\"\n)\n",
			want:  []string{"/project/BUILD:4:", "got ')', want ']'"},
		},
		{
			name:  "misspelled function",
			build: `bulid(base="busybox")`,
			want:  []string{"/project/BUILD:1:1:", "undefined: bulid"},
		},
		{
			name:  "misspelled argument",
			build: `build(base="busybox", comand=["echo"])`,
			want:  []string{"/project/BUILD:1:6:", `unexpected keyword argument "comand"`},
		},
		{
			name:  "missing base",
			build: `build(command=["echo"])`,
			want:  []string{"/project/BUILD:1:6:", "missing argument for base"},
		},
		{
			name:  "empty base",
			build: `build(base="")`,
			want:  []string{"/project/BUILD:1:6:", "base image must not be empty"},
		},
		{
			name:  "invalid base",
			build: `build(base="Busy Box")`,
			want:  []string{"/project/BUILD:1:6:", `invalid base image "Busy Box"`},
		},
		{
			name:  "sources not a list",
			build: `build(base="busybox", sources="app.sh", destination="/app")`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{"/project/BUILD:1:6:", "sources", "got string, want list"},
		},
		{
			name:  "command item not a string",
			build: `build(base="busybox", command=["sleep", 5])`,
			want:  []string{"/project/BUILD:1:6:", "command[1] is int, want string"},
		},
		{
			name:  "missing source",
			build: `build(base="busybox", sources=["app.sh"], destination="/app")`,
			want:  []string{"/project/BUILD:1:6:", `source "app.sh" not found in /project`},
		},
		{
			name:  "absolute source",
			build: `build(base="busybox", sources=["/etc/passwd"], destination="/app")`,
			want:  []string{`source "/etc/passwd" must be a path relative to the BUILD file`},
		},
		{
			name:  "source outside project",
			build: `build(base="busybox", sources=["../secret"], destination="/app")`,
			want:  []string{`source "../secret" is outside /project`},
		},
		{
			name:  "sources without destination",
			build: `build(base="busybox", sources=["app.sh"])`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{"destination is required when sources are given"},
		},
		{
			name:  "relative destination",
			build: `build(base="busybox", sources=["app.sh"], destination="app")`,
			files: map[string]string{"app.sh": "echo hi"},
			want:  []string{`destination "app" must be an absolute path`},
		},
		{
			name:  "build called twice",
			build: "build(base=\"busybox\")\nbuild(base=\"alpine\")\n",
			want:  []string{"/project/BUILD:2:6:", "may only be called once"},
		},
		{
			name:  "build never called",
			build: "base = \"busybox\"\n",
			want:  []string{"/project/BUILD: build() was never called"},
		},
		{
			name:  "empty BUILD",
			build: "",
			want:  []string{"build() was never called"},
		},
		{
			name:  "fail in BUILD",
			build: `fail("unsupported platform")`,
			want:  []string{"/project/BUILD:1:5:", "unsupported platform"},
		},
		{
			name: "error in a helper function",
			build: `
def app(src):
    build(base="busybox", sources=[src], destination="/app")

app("main.sh")
`,
			want: []string{"/project/BUILD:3:10:", `source "main.sh" not found`},
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

// TestSymlinkSource rejects a source that is itself a link.
func TestSymlinkSource(t *testing.T) {
	fsys, _ := diskProject(t, map[string]string{
		"BUILD": `build(base="busybox", sources=["lib"], destination="/app")`,
	}, map[string]string{"lib": "../shared"})
	out := t.TempDir()

	vamos(t, fsys, "write", filepath.Join(out, "app.tar"), "-C", projectDir).
		failsWith(t, false, "/project/BUILD:1:6:", `source "lib" is a symbolic link`)
	assertNoFiles(t, out)
}

// TestSymlinkSourceParent rejects links at every depth of a source path.
func TestSymlinkSourceParent(t *testing.T) {
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "secret"), []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, parent := range []string{"external", "nested/external"} {
		t.Run(parent, func(t *testing.T) {
			src := parent + "/secret"
			fsys, _ := diskProject(t, map[string]string{
				"BUILD": `build(base="busybox", sources=["` + src + `"], destination="/app")`,
			}, map[string]string{parent: external})
			out := t.TempDir()

			vamos(t, fsys, "write", filepath.Join(out, "app.tar"), "-C", projectDir).
				failsWith(t, false, "/project/BUILD:1:6:",
					`source "`+src+`" has symbolic link parent "`+filepath.Join(projectDir, parent)+`"`)
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
