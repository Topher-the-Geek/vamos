package cli

import (
	"archive/tar"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/run"
	"github.com/Topher-the-Geek/vamos/internal/testenv"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/spf13/afero"
	"go.podman.io/storage"
)

// cachedBuild is a BUILD file with a layer and a task whose results are cached, and a final layer that uses both.
// Its targets print a value unique to the test, so that results cached by other tests or earlier runs are never reused.
func cachedBuild(nonce string) string {
	return pullBusybox + fmt.Sprintf(`
glob(name="input", include=["input.txt"])
layer(
    name="deps",
    base="busybox",
    steps=[
        add("input", "/deps"),
        build_env(LANG="C", TZ="UTC"),
        env(GREETING="hello", NAME="vamos"),
        run(["sh", "-c", "echo deps %[1]s; echo deps warning %[1]s >&2; echo %[1]s > /deps/nonce.txt"]),
    ],
)
task(
    name="size",
    base="busybox",
    steps=[
        add("input", "/work"),
        run(["sh", "-c", "echo size %[1]s; wc -c < /work/input.txt > /work/size.txt; echo %[1]s > /work/nonce.txt"]),
    ],
    source="/work",
    include=["size.txt", "nonce.txt"],
)
layer(name="app", base="deps", steps=[add("size", "/app")])
`, nonce)
}

// newNonce returns a value unique to one test run.
func newNonce() string {
	return fmt.Sprintf("%016x", rand.Uint64())
}

// TestCacheReuse checks that a second build reuses the cached results of every target, the last layer included,
// says so, and prints what they printed when they ran, and that it sends the same image.
func TestCacheReuse(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	fsys := project(t, map[string]string{"BUILD": cachedBuild(nonce), "input.txt": "hello\n"})
	dir := t.TempDir()

	first := vamos(t, fsys, "save", "app", filepath.Join(dir, "1.tar"), "-C", projectDir).succeeds(t)
	assertTargets(t, first, map[string]string{`layer "deps"`: "building", `task "size"`: "building"})

	second := vamos(t, fsys, "save", "app", filepath.Join(dir, "2.tar"), "-C", projectDir).succeeds(t)
	assertTargets(t, second, map[string]string{
		`layer "deps"`: "cached",
		`task "size"`:  "cached",
		`layer "app"`:  "cached",
	})
	for _, want := range []string{"deps " + nonce, "size " + nonce} {
		if !strings.Contains(second.stdout, want) {
			t.Errorf("stdout = %q, want the cached target's output %q replayed", second.stdout, want)
		}
	}
	if want := "deps warning " + nonce; !strings.Contains(second.stderr, want) {
		t.Errorf("stderr = %q, want the cached target's errors %q replayed", second.stderr, want)
	}
	// TestRebuildReproducible checks that running the targets again makes the same image too.
	if a, b := builtID(t, first), builtID(t, second); a != b {
		t.Errorf("second build made image %s, want the first's %s", b, a)
	}
	// The ID reported is the one the image has where it was sent, as `docker images` shows it.
	config, err := readImage(t, filepath.Join(dir, "2.tar")).ConfigName()
	must(t, err)
	if got := builtID(t, second); got != config.Hex {
		t.Errorf("reported image %s, want the ID in the tarball, %s", got, config.Hex)
	}
}

// TestCacheMisses checks which changes to a target's inputs make it run again.
func TestCacheMisses(t *testing.T) {
	requireBuildah(t)
	tests := []struct {
		name   string
		change func(t *testing.T, fsys afero.Fs, build string) string // returns the new BUILD file
		want   map[string]string
	}{
		{
			name: "source content",
			change: func(t *testing.T, fsys afero.Fs, build string) string {
				must(t, afero.WriteFile(fsys, filepath.Join(projectDir, "input.txt"), []byte("HELLO\n"), 0o644))
				return build
			},
			want: map[string]string{`layer "deps"`: "building", `task "size"`: "building"},
		},
		{
			name: "source mode",
			change: func(t *testing.T, fsys afero.Fs, build string) string {
				must(t, fsys.Chmod(filepath.Join(projectDir, "input.txt"), 0o600))
				return build
			},
			want: map[string]string{`layer "deps"`: "building", `task "size"`: "building"},
		},
		{
			name: "run step",
			change: func(t *testing.T, fsys afero.Fs, build string) string {
				return strings.Replace(build, `"echo deps `, `"echo changed deps `, 1)
			},
			want: map[string]string{`layer "deps"`: "building", `task "size"`: "cached"},
		},
		{
			name: "step added",
			change: func(t *testing.T, fsys afero.Fs, build string) string {
				add := `add("input", "/deps"),`
				return strings.Replace(build, add, `build_env(LANG="C"), `+add, 1)
			},
			want: map[string]string{`layer "deps"`: "building", `task "size"`: "cached"},
		},
		{
			name: "variable's value",
			change: func(t *testing.T, fsys afero.Fs, build string) string {
				return strings.Replace(build, `GREETING="hello"`, `GREETING="hola"`, 1)
			},
			want: map[string]string{`layer "deps"`: "building", `task "size"`: "cached"},
		},
		{
			name: "add destination",
			change: func(t *testing.T, fsys afero.Fs, build string) string {
				return strings.Replace(build, `add("input", "/deps")`, `add("input", "/deps/input")`, 1)
			},
			want: map[string]string{`layer "deps"`: "building", `task "size"`: "cached"},
		},
		{
			name: "add owner",
			change: func(t *testing.T, fsys afero.Fs, build string) string {
				return strings.Replace(build, `add("input", "/deps")`, `add("input", "/deps", owner="nobody")`, 1)
			},
			want: map[string]string{`layer "deps"`: "building", `task "size"`: "cached"},
		},
		{
			name: "task's selection limit",
			change: func(t *testing.T, fsys afero.Fs, build string) string {
				return strings.Replace(build, `include=["size.txt", "nonce.txt"],`,
					`include=["size.txt", "nonce.txt"], max_file_count=2,`, 1)
			},
			want: map[string]string{`layer "deps"`: "cached", `task "size"`: "building"},
		},
		{
			// Neither changes which files the task collects.
			name: "task's patterns reordered and repeated",
			change: func(t *testing.T, fsys afero.Fs, build string) string {
				return strings.Replace(build, `include=["size.txt", "nonce.txt"]`,
					`include=["./nonce.txt", "size.txt", "nonce.txt"]`, 1)
			},
			want: map[string]string{`layer "deps"`: "cached", `task "size"`: "cached"},
		},
		{
			// Variables have different names, so their order makes no difference.
			// TestEnvOrder checks that it makes no difference to the image either.
			name: "variables reordered",
			change: func(t *testing.T, fsys afero.Fs, build string) string {
				build = strings.Replace(build, `build_env(LANG="C", TZ="UTC")`, `build_env(TZ="UTC", LANG="C")`, 1)
				env := `env(GREETING="hello", NAME="vamos")`
				return strings.Replace(build, env, `env(NAME="vamos", GREETING="hello")`, 1)
			},
			want: map[string]string{`layer "deps"`: "cached", `task "size"`: "cached"},
		},
		{
			// Target names aren't inputs, so renaming a target keeps its result.
			name: "target renamed",
			change: func(t *testing.T, fsys afero.Fs, build string) string {
				build = strings.Replace(build, `name="deps"`, `name="dependencies"`, 1)
				return strings.Replace(build, `base="deps"`, `base="dependencies"`, 1)
			},
			want: map[string]string{`layer "dependencies"`: "cached", `task "size"`: "cached"},
		},
		{
			name: "source modification time only",
			change: func(t *testing.T, fsys afero.Fs, build string) string {
				must(t, afero.WriteFile(fsys, filepath.Join(projectDir, "input.txt"), []byte("hello\n"), 0o644))
				return build
			},
			want: map[string]string{`layer "deps"`: "cached", `task "size"`: "cached"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			build := cachedBuild(newNonce())
			fsys := project(t, map[string]string{"BUILD": build, "input.txt": "hello\n"})
			dir := t.TempDir()
			vamos(t, fsys, "save", "app", filepath.Join(dir, "1.tar"), "-C", projectDir).succeeds(t)

			build = tt.change(t, fsys, build)
			must(t, afero.WriteFile(fsys, filepath.Join(projectDir, "BUILD"), []byte(build), 0o644))
			r := vamos(t, fsys, "save", "app", filepath.Join(dir, "2.tar"), "-C", projectDir).succeeds(t)
			assertTargets(t, r, tt.want)
		})
	}
}

// TestCacheUnchangedOutput checks that when a task runs again
// but makes the same files, the targets that use them are reused.
func TestCacheUnchangedOutput(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox + fmt.Sprintf(`
glob(name="input", include=["input.txt"])
task(
    name="size",
    base="busybox",
    steps=[
        add("input", "/work"),
        run(["sh", "-c", "echo %[1]s; wc -c < /work/input.txt > /work/size.txt"]),
    ],
    source="/work",
    include=["size.txt"],
)
layer(name="sized", base="busybox", steps=[add("size", "/app"), run(["echo", "%[1]s"])])
layer(name="app", base="sized")
`, nonce),
		"input.txt": "hello\n",
	})
	dir := t.TempDir()
	vamos(t, fsys, "save", "app", filepath.Join(dir, "1.tar"), "-C", projectDir).succeeds(t)

	// The same number of bytes, so the same size.txt.
	must(t, afero.WriteFile(fsys, filepath.Join(projectDir, "input.txt"), []byte("HELLO\n"), 0o644))
	r := vamos(t, fsys, "save", "app", filepath.Join(dir, "2.tar"), "-C", projectDir).succeeds(t)
	assertTargets(t, r, map[string]string{`task "size"`: "building", `layer "sized"`: "cached"})
}

// TestCacheEmptyOutput checks that a glob and a task
// that select no files, as allow_no_files allows, build and are reused.
func TestCacheEmptyOutput(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox + fmt.Sprintf(`
glob(name="none", include=["*.go"], allow_no_files=True)
task(
    name="empty",
    base="busybox",
    steps=[run(["sh", "-c", "echo %[1]s; mkdir /out"])],
    source="/out",
    include=["*"],
    allow_no_files=True,
)
layer(name="app", base="busybox", steps=[add("none", "/app"), add("empty", "/app"), env(NONCE="%[1]s")])
`, nonce),
	})
	dir := t.TempDir()
	first := vamos(t, fsys, "save", "app", filepath.Join(dir, "1.tar"), "-C", projectDir).succeeds(t)
	assertTargets(t, first, map[string]string{`task "empty"`: "building", `layer "app"`: "building"})
	second := vamos(t, fsys, "save", "app", filepath.Join(dir, "2.tar"), "-C", projectDir).succeeds(t)
	assertTargets(t, second, map[string]string{`task "empty"`: "cached", `layer "app"`: "cached"})
	if a, b := builtID(t, first), builtID(t, second); a != b {
		t.Errorf("second build made image %s, want the first's %s", b, a)
	}
}

// TestCacheSourceEditedDuringBuild checks
// that a source file edited while a build reads it is built as the cache stored it,
// and that the result is kept under the key of those files,
// so that a later build of the file as it was doesn't reuse it.
func TestCacheSourceEditedDuringBuild(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	before, edited := nonce+" before\n", nonce+" edited\n"
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox + `
glob(name="input", include=["input.txt"])
layer(name="app", base="busybox", steps=[add("input", "/app"), run(["cat", "/app/input.txt"])])
`,
		"input.txt": before,
	})
	input := filepath.Join(projectDir, "input.txt")
	dir := t.TempDir()
	r := vamos(t, &testenv.EditedFs{Fs: fsys, File: input, Edit: edited},
		"save", "app", filepath.Join(dir, "1.tar"), "-C", projectDir).succeeds(t)
	if !strings.Contains(r.stdout, edited) {
		t.Errorf("stdout = %q, want the layer built with the edited file %q", r.stdout, edited)
	}

	must(t, afero.WriteFile(fsys, input, []byte(before), 0o644))
	r = vamos(t, fsys, "save", "app", filepath.Join(dir, "2.tar"), "-C", projectDir).succeeds(t)
	assertTargets(t, r, map[string]string{`layer "app"`: "building"})
	if !strings.Contains(r.stdout, before) {
		t.Errorf("stdout = %q, want the layer built with the file as it was, %q", r.stdout, before)
	}
}

// TestCacheGlobReadOnce checks that a glob's files are read once in a build, however many targets add them,
// so that a file edited after they are read can't make the targets' files differ.
func TestCacheGlobReadOnce(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	before, edited := nonce+" before\n", nonce+" edited\n"
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox + `
glob(name="input", include=["input.txt"])
layer(name="first", base="busybox", steps=[add("input", "/first"), run(["cat", "/first/input.txt"])])
layer(name="app", base="first", steps=[add("input", "/app"), run(["cat", "/app/input.txt"])])
`,
		"input.txt": before,
	})
	// Storing a new file opens it twice, to hash it and then to copy it, so a third open is a second read.
	r := vamos(t, &testenv.EditedFs{Fs: fsys, File: filepath.Join(projectDir, "input.txt"), Edit: edited, At: 3},
		"save", "app", filepath.Join(t.TempDir(), "app.tar"), "-C", projectDir).succeeds(t)
	if got := strings.Count(r.stdout, before); got != 2 || strings.Contains(r.stdout, edited) {
		t.Errorf("stdout = %q, want both layers built with the file as first read, %q", r.stdout, before)
	}
}

// TestCacheEntryRemoved checks that a cached result whose image or files have been removed
// since is reported and built again, rather than failing the build.
func TestCacheEntryRemoved(t *testing.T) {
	requireBuildah(t)
	tests := []struct {
		name   string
		remove func(t *testing.T, images map[string]bool, blobs map[string]bool)
		want   []string
	}{
		{
			name: "image",
			remove: func(t *testing.T, images map[string]bool, blobs map[string]bool) {
				withStore(t, func(store storage.Store) {
					for id := range images {
						if _, err := store.DeleteImage(id, true); err != nil {
							t.Fatal(err)
						}
					}
				})
			},
			want: []string{
				`layer "deps": building, since the cached result can't be used: its image `,
				"is no longer in the container store",
				`task "size": cached`,
			},
		},
		{
			name: "files",
			remove: func(t *testing.T, images map[string]bool, blobs map[string]bool) {
				for path := range blobs {
					must(t, os.Remove(path))
				}
			},
			want: []string{
				`layer "deps": building, since the cached result can't be used: its output is missing from the cache`,
				`task "size": building, since the cached result can't be used: its files are missing from the cache`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := project(t, map[string]string{"BUILD": cachedBuild(newNonce()), "input.txt": "hello\n"})
			dir := t.TempDir()
			images, blobs := cachedImages(t), cachedBlobs(t)
			vamos(t, fsys, "save", "app", filepath.Join(dir, "1.tar"), "-C", projectDir).succeeds(t)
			tt.remove(t, newKeys(cachedImages(t), images), newKeys(cachedBlobs(t), blobs))

			r := vamos(t, fsys, "save", "app", filepath.Join(dir, "2.tar"), "-C", projectDir).succeeds(t)
			for _, want := range tt.want {
				if !strings.Contains(r.stdout, want) {
					t.Errorf("stdout = %q, want it to contain %q", r.stdout, want)
				}
			}
		})
	}
}

// TestFailedTargetNotCached checks that a target that fails is run again by the next build,
// while the targets before it are reused.
func TestFailedTargetNotCached(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox + fmt.Sprintf(`
layer(name="deps", base="busybox", steps=[run(["echo", %q])])
layer(name="broken", base="deps", steps=[run(["false"])])
layer(name="app", base="broken")
`, nonce),
	})
	out := filepath.Join(t.TempDir(), "app.tar")
	for range 2 {
		r := vamos(t, fsys, "save", "app", out, "-C", projectDir).
			failsWith(t, false, `layer "broken": run(["false"]) failed`)
		if !strings.Contains(r.stdout, `layer "broken": building`) {
			t.Errorf("stdout = %q, want the failed target built again", r.stdout)
		}
	}
	r := vamos(t, fsys, "save", "app", out, "-C", projectDir).
		failsWith(t, false, `layer "broken": run(["false"]) failed`)
	if !strings.Contains(r.stdout, `layer "deps": cached`) {
		t.Errorf("stdout = %q, want the target before the failure reused", r.stdout)
	}
}

// assertTargets fails the test unless r reports each target in want, such as `layer "app"`, as "building" or "cached".
func assertTargets(t *testing.T, r result, want map[string]string) {
	t.Helper()
	for target, how := range want {
		if line := target + ": " + how + "\n"; !strings.Contains(r.stdout, line) {
			t.Errorf("stdout = %q, want %q", r.stdout, line)
		}
	}
}

// builtID returns the image ID that r reports after "Image built!".
func builtID(t *testing.T, r result) string {
	t.Helper()
	_, line, ok := strings.Cut(r.stdout, "Image built! ")
	if !ok {
		t.Fatalf("stdout = %q, want it to report the image built", r.stdout)
	}
	fields := strings.Fields(line)
	return fields[len(fields)-1]
}

// cachedImages returns the IDs of the cached layers' images in the local container store.
func cachedImages(t *testing.T) map[string]bool {
	t.Helper()
	return imageIDs(t, func(img storage.Image) bool {
		return slices.ContainsFunc(img.Names, func(name string) bool {
			return strings.HasPrefix(name, run.CacheRepository+":")
		})
	})
}

// cachedBlobs returns the paths of the blobs in the build cache.
func cachedBlobs(t *testing.T) map[string]bool {
	t.Helper()
	cacheDir, err := run.CacheDir()
	must(t, err)
	dir := filepath.Join(cacheDir, "cas", "sha256")
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, e := range entries {
		paths[filepath.Join(dir, e.Name())] = true
	}
	return paths
}

// newKeys returns the keys of after that aren't in before.
func newKeys(after, before map[string]bool) map[string]bool {
	added := map[string]bool{}
	for k := range after {
		if !before[k] {
			added[k] = true
		}
	}
	return added
}

// TestBuildTargets checks that `vamos build` runs only the named targets
// and the targets they use, keeping the results in the cache for later builds.
func TestBuildTargets(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{"BUILD": cachedBuild(newNonce()), "input.txt": "hello\n"})

	r := vamos(t, fsys, "build", "size", "-C", projectDir).ran(t)
	assertTargets(t, r, map[string]string{`task "size"`: "building"})
	if strings.Contains(r.stdout, `layer "deps"`) {
		t.Errorf("stdout = %q, want only the named target and those it uses", r.stdout)
	}

	r = vamos(t, fsys, "build", "app", "size", "app", "-C", projectDir).ran(t)
	assertTargets(t, r, map[string]string{
		`layer "deps"`: "building",
		`task "size"`:  "cached",
		`layer "app"`:  "building",
	})
	if strings.Count(r.stdout, `layer "app"`) != 1 {
		t.Errorf("stdout = %q, want a target named twice built once", r.stdout)
	}
	r = vamos(t, fsys, "build", "app", "-C", projectDir).ran(t)
	assertTargets(t, r, map[string]string{`layer "app"`: "cached"})

	r = vamos(t, fsys, "save", "app", filepath.Join(t.TempDir(), "app.tar"), "-C", projectDir).succeeds(t)
	assertTargets(t, r, map[string]string{`layer "deps"`: "cached", `task "size"`: "cached"})
}

// TestRebuild checks that `vamos build --rebuild` runs the named targets again, reusing the targets they use,
// and that later builds use the new result.
func TestRebuild(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{
		"BUILD": pullBusybox + fmt.Sprintf(`
layer(name="base", base="busybox", steps=[run(["echo", %q])])
layer(name="deps", base="base", steps=[run(["sh", "-c", "cat /proc/sys/kernel/random/uuid > /id"])])
layer(name="app", base="deps")
`, newNonce()),
	})
	dir := t.TempDir()
	vamos(t, fsys, "save", "app", filepath.Join(dir, "1.tar"), "-C", projectDir).succeeds(t)

	r := vamos(t, fsys, "build", "deps", "--rebuild", "-C", projectDir).ran(t)
	assertTargets(t, r, map[string]string{`layer "base"`: "cached", `layer "deps"`: "rebuilding"})
	if strings.Contains(r.stdout, "pulling again") {
		t.Errorf("stdout = %q, want the targets the named target uses reused", r.stdout)
	}

	r = vamos(t, fsys, "save", "app", filepath.Join(dir, "2.tar"), "-C", projectDir).succeeds(t)
	assertTargets(t, r, map[string]string{`layer "deps"`: "cached"})
	first, second := readImage(t, filepath.Join(dir, "1.tar")), readImage(t, filepath.Join(dir, "2.tar"))
	if a, b := imageFile(t, first, "/id"), imageFile(t, second, "/id"); a == b {
		t.Errorf("/id = %q after rebuilding, want it to differ from the first build's", b)
	}
}

// TestRebuildReproducible checks that running every target again,
// rather than reusing their cached results, makes the same image:
// files are stamped with the base image's time, not the build's.
func TestRebuildReproducible(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{"BUILD": cachedBuild(newNonce()), "input.txt": "hello\n"})
	dir := t.TempDir()
	first := vamos(t, fsys, "save", "app", filepath.Join(dir, "1.tar"), "-C", projectDir).succeeds(t)

	r := vamos(t, fsys, "build", "deps", "size", "app", "--rebuild", "-C", projectDir).ran(t)
	assertTargets(t, r, map[string]string{
		`layer "deps"`: "rebuilding",
		`task "size"`:  "rebuilding",
		`layer "app"`:  "rebuilding",
	})
	second := vamos(t, fsys, "save", "app", filepath.Join(dir, "2.tar"), "-C", projectDir).succeeds(t)
	if a, b := builtID(t, first), builtID(t, second); a != b {
		t.Errorf("rebuilt image %s, want the first build's %s", b, a)
	}
}

// TestEnvOrder checks that the order of env()'s variables,
// which its target's key leaves out, makes no difference to the image either,
// so that a target's cached result is the one it would make.
func TestEnvOrder(t *testing.T) {
	requireBuildah(t)
	build := cachedBuild(newNonce())
	fsys := project(t, map[string]string{"BUILD": build, "input.txt": "hello\n"})
	dir := t.TempDir()
	first := vamos(t, fsys, "save", "app", filepath.Join(dir, "1.tar"), "-C", projectDir).succeeds(t)

	build = strings.Replace(build, `env(GREETING="hello", NAME="vamos")`, `env(NAME="vamos", GREETING="hello")`, 1)
	must(t, afero.WriteFile(fsys, filepath.Join(projectDir, "BUILD"), []byte(build), 0o644))
	vamos(t, fsys, "build", "deps", "app", "--rebuild", "-C", projectDir).ran(t)
	second := vamos(t, fsys, "save", "app", filepath.Join(dir, "2.tar"), "-C", projectDir).succeeds(t)
	if a, b := builtID(t, first), builtID(t, second); a != b {
		t.Errorf("image with variables reordered is %s, want the first build's %s", b, a)
	}
}

// TestRebuildPull checks that naming a pull() with --rebuild pulls it again.
func TestRebuildPull(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, validBuild)
	r := vamos(t, fsys, "build", "busybox", "--rebuild", "-C", projectDir).ran(t)
	if want := "pull \"busybox\": pulling again\n"; r.stdout != want {
		t.Errorf("stdout = %q, want %q", r.stdout, want)
	}
}

// TestTimestamp checks that --timestamp stamps the files of targets that run,
// and that it is left out of their keys: a later build reuses them, stamped as they were.
func TestTimestamp(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{"BUILD": cachedBuild(newNonce()), "input.txt": "hello\n"})
	dir := t.TempDir()
	stamp := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

	vamos(t, fsys, "save", "app", filepath.Join(dir, "1.tar"), "--timestamp", stamp.Format(time.RFC3339),
		"-C", projectDir).succeeds(t)
	r := vamos(t, fsys, "save", "app", filepath.Join(dir, "2.tar"), "--timestamp", "0", "-C", projectDir).succeeds(t)
	assertTargets(t, r, map[string]string{`layer "deps"`: "cached", `task "size"`: "cached"})

	for _, name := range []string{"1.tar", "2.tar"} {
		img := readImage(t, filepath.Join(dir, name))
		for _, path := range []string{"/deps/nonce.txt", "/app/size.txt"} {
			if got := imageModTime(t, img, path); !got.Equal(stamp) {
				t.Errorf("%s: %s modified at %v, want %v", name, path, got, stamp)
			}
		}
		config, err := img.ConfigFile()
		must(t, err)
		if got := config.Created.Time; !got.Equal(stamp) {
			t.Errorf("%s: image created at %v, want %v", name, got, stamp)
		}
	}

	// Without the flag, files are stamped with the base image's creation time.
	fsys = project(t, map[string]string{"BUILD": cachedBuild(newNonce()), "input.txt": "hello\n"})
	vamos(t, fsys, "save", "app", filepath.Join(dir, "3.tar"), "-C", projectDir).succeeds(t)
	img := readImage(t, filepath.Join(dir, "3.tar"))
	config, err := img.ConfigFile()
	must(t, err)
	if got := imageModTime(t, img, "/deps/nonce.txt"); !got.Equal(config.Created.Time) || got.Year() < 2020 {
		t.Errorf("/deps/nonce.txt modified at %v, want busybox's creation time, %v", got, config.Created.Time)
	}
}

// TestTaskEpochInKey checks that a target's key covers the epoch of the tasks whose files it adds,
// not only their files: a task run again with a later epoch makes the same files,
// but a layer adding them must then be stamped with that epoch, as a build from an empty cache would be.
func TestTaskEpochInKey(t *testing.T) {
	requireBuildah(t)
	fsys := project(t, map[string]string{"BUILD": cachedBuild(newNonce()), "input.txt": "hello\n"})
	dir := t.TempDir()
	first := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	later := first.Add(time.Hour)

	vamos(t, fsys, "save", "app", filepath.Join(dir, "1.tar"), "--timestamp", first.Format(time.RFC3339),
		"-C", projectDir).succeeds(t)
	vamos(t, fsys, "build", "size", "--rebuild", "--timestamp", later.Format(time.RFC3339),
		"-C", projectDir).ran(t)
	r := vamos(t, fsys, "save", "app", filepath.Join(dir, "2.tar"), "-C", projectDir).succeeds(t)
	assertTargets(t, r, map[string]string{
		`layer "deps"`: "cached",
		`task "size"`:  "cached",
		`layer "app"`:  "building",
	})

	img := readImage(t, filepath.Join(dir, "2.tar"))
	if got := imageModTime(t, img, "/app/size.txt"); !got.Equal(later) {
		t.Errorf("/app/size.txt modified at %v, want the task's epoch, %v", got, later)
	}
}

// TestPrune checks that `vamos prune` removes every cached result, so that targets run again,
// and leaves pulled images, and cached images that also have other names, in the container store.
// It empties the cache that every test, and every vamos on this machine, shares.
func TestPrune(t *testing.T) {
	requireBuildah(t)
	nonce := newNonce()
	fsys := project(t, map[string]string{"BUILD": cachedBuild(nonce), "input.txt": "hello\n"})
	vamos(t, fsys, "build", "app", "-C", projectDir).ran(t)

	// Name one cached image as a user might, with `buildah tag`.
	kept := "localhost/vamos-test-kept:" + nonce
	var keptID string
	for id := range cachedImages(t) {
		keptID = id
		break
	}
	withStore(t, func(store storage.Store) { must(t, store.AddNames(keptID, []string{kept})) })
	t.Cleanup(func() {
		withStore(t, func(store storage.Store) { _, _ = store.DeleteImage(keptID, true) })
	})

	// The pulled busybox is named by digest, or by its tag if an older vamos pulled it.
	var pulled []string
	withStore(t, func(store storage.Store) {
		images, err := store.Images()
		must(t, err)
		for _, img := range images {
			if slices.ContainsFunc(img.Names, func(n string) bool {
				return strings.HasPrefix(n, "docker.io/library/busybox")
			}) {
				pulled = append(pulled, img.ID)
			}
		}
	})
	if len(pulled) == 0 {
		t.Fatal("no busybox image in the container store after building")
	}

	r := vamos(t, fsys, "prune").ran(t)
	if !strings.HasPrefix(r.stdout, "Removed ") || strings.HasPrefix(r.stdout, "Removed 0 images") {
		t.Errorf("stdout = %q, want it to report the cached images removed", r.stdout)
	}
	if images := cachedImages(t); len(images) > 0 {
		t.Errorf("cached images %v left after pruning", images)
	}
	if blobs := cachedBlobs(t); len(blobs) > 0 {
		t.Errorf("%d files left in the build cache after pruning", len(blobs))
	}
	withStore(t, func(store storage.Store) {
		if img, err := store.Image(keptID); err != nil {
			t.Errorf("image %s, also named %s, removed by pruning: %v", keptID[:12], kept, err)
		} else if !slices.Equal(img.Names, []string{kept}) {
			t.Errorf("image %s has names %v after pruning, want only %s", keptID[:12], img.Names, kept)
		}
		for _, id := range pulled {
			if _, err := store.Image(id); err != nil {
				t.Errorf("pulled busybox image %s removed by pruning: %v", id[:12], err)
			}
		}
	})
	r = vamos(t, fsys, "prune").ran(t)
	if !strings.HasPrefix(r.stdout, "Removed 0 images from the container store, and 0 results and 0 files") {
		t.Errorf("stdout = %q, want nothing left to remove", r.stdout)
	}

	r = vamos(t, fsys, "build", "app", "-C", projectDir).ran(t)
	assertTargets(t, r, map[string]string{`layer "deps"`: "building", `task "size"`: "building"})
}

// readImage reads the image in the tarball at path.
func readImage(t *testing.T, path string) v1.Image {
	t.Helper()
	img, err := tarball.ImageFromPath(path, nil)
	must(t, err)
	return img
}

// imageModTime returns the modification time of the file at path in img's filesystem.
func imageModTime(t *testing.T, img v1.Image, path string) time.Time {
	t.Helper()
	var modTime time.Time
	found := walkImage(t, img, func(hdr *tar.Header, r io.Reader) bool {
		modTime = hdr.ModTime
		return imagePath(hdr) == path
	})
	if !found {
		t.Fatalf("%s not found in image", path)
	}
	return modTime
}
