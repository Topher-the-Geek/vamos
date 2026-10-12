package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// exportBuild declares a task with output, a task without, and a layer, after pullBusybox.
const exportBuild = pullBusybox + `
glob(name="code", include=["app.sh"])
task(name="make", base="busybox", steps=[run(["mkdir", "/out"])], source="/out", include=["."])
task(name="test", base="busybox", steps=[run(["true"])])
layer(name="app", base="busybox")
index(name="all", layer="app", platforms=["linux/amd64", "linux/arm64"])
`

// TestExportMistakes checks the mistakes in an export that are found before anything is built.
func TestExportMistakes(t *testing.T) {
	out := t.TempDir()
	full := filepath.Join(out, "full")
	must(t, os.MkdirAll(filepath.Join(full, "old"), 0o755))
	file := filepath.Join(out, "file")
	must(t, os.WriteFile(file, nil, 0o644))
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "a layer",
			args: []string{"app", filepath.Join(out, "new")},
			want: `"app" is a layer(), not a task(); export writes a task's files, and "vamos save" writes an image`,
		},
		{
			name: "a pull",
			args: []string{"busybox", filepath.Join(out, "new")},
			want: `"busybox" is a pull(), not a task(); export writes a task's files`,
		},
		{
			name: "an index",
			args: []string{"all", filepath.Join(out, "new")},
			want: `"all" is an index(), not a task(); export writes a task's files`,
		},
		{
			name: "a glob",
			args: []string{"code", filepath.Join(out, "new")},
			want: `"code" is a glob(), whose files are already in the project; export writes a task's files`,
		},
		{
			name: "a task without source",
			args: []string{"test", filepath.Join(out, "new")},
			want: `"test" is a task() without source, so it has no files to export; "vamos build test" runs it`,
		},
		{
			name: "a folder that holds files",
			args: []string{"make", full},
			want: "exporting to " + full + ": it isn't empty; pass --replace to replace it",
		},
		{
			name: "a file",
			args: []string{"make", file, "--replace"},
			want: "exporting to " + file + ": it isn't a directory",
		},
		{
			name: "a missing parent",
			args: []string{"make", filepath.Join(out, "nope", "new")},
			want: "exporting to " + filepath.Join(out, "nope", "new") + ": directory " + filepath.Join(out, "nope") +
				" does not exist",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := project(t, map[string]string{"BUILD": exportBuild, "app.sh": ""})
			args := append([]string{"export"}, tt.args...)
			vamos(t, fsys, append(args, "-C", projectDir)...).failsWith(t, false, tt.want)
			if _, err := os.Stat(filepath.Join(full, "old")); err != nil {
				t.Errorf("the folder's files are gone: %v", err)
			}
		})
	}
}

// TestSendTaskWithOutput checks that sending a task's image, which it has none of, suggests exporting its files
// if it has some.
func TestSendTaskWithOutput(t *testing.T) {
	fsys := project(t, map[string]string{"BUILD": exportBuild, "app.sh": ""})
	vamos(t, fsys, "save", "make", filepath.Join(t.TempDir(), "app.tar"), "-C", projectDir).failsWith(t, false,
		`"make" is a task(), whose image is discarded once it runs; name a pull(), layer() or index() to send, `+
			`or export the task's files with "vamos export"`)
}

// TestExport exports a task's files, with their modes, links and times, and replaces a folder that holds files
// only with --replace, removing those the task doesn't make.
func TestExport(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	fsys := project(t, map[string]string{"BUILD": pullBusybox + fmt.Sprintf(`
task(
    name="make",
    base="busybox",
    steps=[run(["sh", "-c", "mkdir -p /out/sub && echo %s > /out/a.txt && echo b > /out/sub/b.txt && `+
		`chmod 750 /out/sub && chmod 640 /out/a.txt && ln -s a.txt /out/link"])],
    source="/out",
    include=["."],
)
`, nonce)})
	// A time the task's files are stamped with, after the base image's.
	epoch := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := filepath.Join(t.TempDir(), "made")

	r := vamos(t, fsys, "export", "make", dir, "--timestamp", epoch.Format(time.RFC3339), "-C", projectDir).ran(t)
	if !strings.HasPrefix(lastLine(r.stdout), "Files exported! "+dir+" sha256:") {
		t.Errorf("stdout = %q, want it to end with the files' tree", r.stdout)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "a.txt")); err != nil || string(got) != nonce+"\n" {
		t.Errorf("a.txt = %q, %v; want the task's", got, err)
	}
	for path, want := range map[string]os.FileMode{"a.txt": 0o640, "sub": 0o750 | os.ModeDir} {
		if info, err := os.Lstat(filepath.Join(dir, path)); err != nil || info.Mode() != want {
			t.Errorf("%s's mode = %v, %v; want %v", path, info.Mode(), err, want)
		}
	}
	if target, err := os.Readlink(filepath.Join(dir, "link")); err != nil || target != "a.txt" {
		t.Errorf("link = %q, %v; want a link to a.txt", target, err)
	}
	for _, path := range []string{".", "a.txt", "sub", "sub/b.txt", "link"} {
		if info, err := os.Lstat(filepath.Join(dir, path)); err != nil || !info.ModTime().Equal(epoch) {
			t.Errorf("%s's time = %v, %v; want the task's, %v", path, info.ModTime(), err, epoch)
		}
	}

	must(t, os.WriteFile(filepath.Join(dir, "stale.txt"), nil, 0o644))
	vamos(t, fsys, "export", "make", dir, "-C", projectDir).
		failsWith(t, false, "exporting to "+dir+": it isn't empty; pass --replace to replace it")
	vamos(t, fsys, "export", "make", dir, "--replace", "-C", projectDir).ran(t)
	entries, err := os.ReadDir(dir)
	must(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if want := []string{"a.txt", "link", "sub"}; !slices.Equal(names, want) {
		t.Errorf("the folder holds %q, want %q, the task's files alone", names, want)
	}
	if leftover, _ := filepath.Glob(filepath.Join(filepath.Dir(dir), ".made.vamos-*")); len(leftover) > 0 {
		t.Errorf("export left %q behind", leftover)
	}
}

// TestExportFailingBuild checks that a task that fails leaves the folder as it was.
func TestExportFailingBuild(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{"BUILD": pullBusybox +
		`task(name="make", base="busybox", steps=[run(["false"])], source="/out", include=["."])`})
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "old.txt"), nil, 0o644))
	vamos(t, fsys, "export", "make", dir, "--replace", "-C", projectDir).
		failsWith(t, false, `task "make": run(["false"]) failed`)
	if _, err := os.Stat(filepath.Join(dir, "old.txt")); err != nil {
		t.Errorf("the folder's files are gone: %v", err)
	}
	staged := filepath.Join(filepath.Dir(dir), "."+filepath.Base(dir)+".vamos-*")
	if leftover, _ := filepath.Glob(staged); len(leftover) > 0 {
		t.Errorf("export left %q behind", leftover)
	}
}

// TestWriteDeprecated checks that write, save's earlier name, still saves, and says it is deprecated.
func TestWriteDeprecated(t *testing.T) {
	requireBuildah(t)
	path := filepath.Join(t.TempDir(), "app.tar")
	r := vamos(t, project(t, helloBuild), "write", "app", path, "-C", projectDir).succeeds(t)
	if !strings.Contains(r.stdout+r.stderr, `Command "write" is deprecated, use "vamos save"`) {
		t.Errorf("output = %q, want it to say write is deprecated", r.stdout+r.stderr)
	}
	readImage(t, path)
}

// lastLine returns the last line of s, without its newline.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	return lines[len(lines)-1]
}
