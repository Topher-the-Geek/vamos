package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/run"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/opencontainers/go-digest"
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

// blobPath returns where the test's content store keeps the blob of content.
func blobPath(t *testing.T, content string) string {
	t.Helper()
	return filepath.Join(run.CacheDir(stores(t).Options), "cas", "sha256", digest.FromString(content).Encoded())
}

// corruptBlob replaces the content of the blob at path with as many other bytes,
// so that only reading it tells it is corrupt.
func corruptBlob(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	must(t, err)
	must(t, os.WriteFile(path, bytes.Repeat([]byte("x"), int(info.Size())), 0o600))
}

// TestCacheCorruption checks what a build does with content that is corrupt in the content store:
// a task's files are made again, and the target that needed them tried again;
// a glob's files fail the build, naming the blob, since only the client can capture them again;
// and a cached target whose output is corrupt is built again.
func TestCacheCorruption(t *testing.T) {
	requireBuildah(t)
	t.Run("task files", func(t *testing.T) {
		nonce := newNonce()
		fsys := project(t, map[string]string{"BUILD": cachedBuild(nonce), "input.txt": "hello\n"})
		vamos(t, fsys, "build", "app", "-C", projectDir).ran(t)
		corruptBlob(t, blobPath(t, nonce+"\n")) // the task's nonce.txt, which app adds

		r := vamos(t, fsys, "build", "app", "--rebuild", "-C", projectDir).ran(t)
		want := `task "size": building again, since its cached files are corrupt: `
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout = %q, want it to report the task built again", r.stdout)
		}
		if strings.Count(r.stdout, `layer "app": rebuilding`) != 2 {
			t.Errorf("stdout = %q, want the layer tried again after the task", r.stdout)
		}
		if data, err := os.ReadFile(blobPath(t, nonce+"\n")); err != nil || string(data) != nonce+"\n" {
			t.Errorf("the corrupt blob holds %q, %v after the build; want it made again", data, err)
		}
		r = vamos(t, fsys, "build", "app", "-C", projectDir).ran(t)
		assertTargets(t, r, map[string]string{
			`layer "deps"`: "cached",
			`task "size"`:  "cached",
			`layer "app"`:  "cached",
		})
	})
	t.Run("task tree", func(t *testing.T) {
		nonce := newNonce()
		fsys := project(t, map[string]string{"BUILD": cachedBuild(nonce), "input.txt": "hello\n"})
		vamos(t, fsys, "build", "size", "-C", projectDir).ran(t)
		// A tree whose content matches its digest, but which no walk could have made: an entry outside the tree.
		dir := run.CacheDir(stores(t).Options)
		store, err := cache.OpenDirectory(dir)
		must(t, err)
		malformed, err := store.PutBytes(t.Context(), []byte(`{"entries":[{"path":"../x","type":"dir"}]}`))
		must(t, err)
		replaceRecorded(t, "output", malformed)

		r := vamos(t, fsys, "build", "size", "-C", projectDir).ran(t)
		want := `task "size": building, since the cached result can't be used: ` +
			"its files can't be used from the cache: "
		if !strings.Contains(r.stdout, want) ||
			!strings.Contains(r.stdout, `is corrupt: it isn't a valid tree: "../x"`) {
			t.Errorf("stdout = %q, want it to report the malformed tree", r.stdout)
		}
	})
	t.Run("glob files", func(t *testing.T) {
		nonce := newNonce()
		input := "hello " + nonce + "\n"
		fsys := project(t, map[string]string{"BUILD": cachedBuild(nonce), "input.txt": input})
		vamos(t, fsys, "build", "deps", "-C", projectDir).ran(t)
		corruptBlob(t, blobPath(t, input))

		vamos(t, fsys, "build", "deps", "--rebuild", "-C", projectDir).failsWith(t, false,
			`layer "deps": add("input", "/deps") failed: copying sources from the build cache: input.txt: blob `+
				digest.FromString(input).String()+" is corrupt", "captured and submitted again")
		// The corrupt blob had the right size; building again captures the sources again and repairs it.
		vamos(t, fsys, "build", "deps", "--rebuild", "-C", projectDir).ran(t)
		if data, err := os.ReadFile(blobPath(t, input)); err != nil || string(data) != input {
			t.Errorf("the corrupt blob holds %q, %v after building again; want it captured again", data, err)
		}
	})
	t.Run("log", func(t *testing.T) {
		nonce := newNonce()
		fsys := project(t, map[string]string{"BUILD": cachedBuild(nonce), "input.txt": "hello\n"})
		vamos(t, fsys, "build", "deps", "-C", projectDir).ran(t)
		for _, log := range recordedBlobs(t, "log") {
			corruptBlob(t, log)
		}

		r := vamos(t, fsys, "build", "deps", "-C", projectDir).ran(t)
		want := `layer "deps": building, since the cached result can't be used: ` +
			"its output can't be used from the cache: "
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout = %q, want it to report the corrupt output", r.stdout)
		}
		if !strings.Contains(r.stdout, "is corrupt") {
			t.Errorf("stdout = %q, want the reason to say the log is corrupt", r.stdout)
		}
	})
	t.Run("malformed log", func(t *testing.T) {
		nonce := newNonce()
		fsys := project(t, map[string]string{"BUILD": cachedBuild(nonce), "input.txt": "hello\n"})
		vamos(t, fsys, "build", "deps", "-C", projectDir).ran(t)
		// A log whose content matches its digest, but whose record promises more bytes than follow.
		store, err := cache.OpenDirectory(run.CacheDir(stores(t).Options))
		must(t, err)
		malformed, err := store.PutBytes(t.Context(), []byte{cache.StreamStdout, 10, 'h', 'i'})
		must(t, err)
		replaceRecorded(t, "log", malformed)

		r := vamos(t, fsys, "build", "deps", "-C", projectDir).ran(t)
		want := `layer "deps": building, since the cached result can't be used: ` +
			"its output can't be used from the cache: "
		if !strings.Contains(r.stdout, want) || !strings.Contains(r.stdout, "it isn't a whole log") {
			t.Errorf("stdout = %q, want it to report the malformed log and build again", r.stdout)
		}
	})
}

// replaceRecorded makes every one of the test's action results that has the field, "log" or "output", name b.
func replaceRecorded(t *testing.T, field string, b cache.Blob) {
	t.Helper()
	for _, record := range recordsNaming(t, field) {
		data, err := os.ReadFile(record)
		must(t, err)
		var fields map[string]json.RawMessage
		must(t, json.Unmarshal(data, &fields))
		fields[field], err = json.Marshal(b)
		must(t, err)
		data, err = json.Marshal(fields)
		must(t, err)
		must(t, os.WriteFile(record, data, 0o600))
	}
}

// recordsNaming returns the paths of the test's action results that have the field, "log" or "output".
func recordsNaming(t *testing.T, field string) []string {
	t.Helper()
	dir := run.CacheDir(stores(t).Options)
	entries, err := os.ReadDir(filepath.Join(dir, "ac"))
	must(t, err)
	var paths []string
	for _, e := range entries {
		path := filepath.Join(dir, "ac", e.Name())
		data, err := os.ReadFile(path)
		must(t, err)
		var record map[string]json.RawMessage
		must(t, json.Unmarshal(data, &record))
		if _, ok := record[field]; ok {
			paths = append(paths, path)
		}
	}
	return paths
}

// recordedBlobs returns the paths of the blobs that the test's action results name in field, "log" or "output".
func recordedBlobs(t *testing.T, field string) []string {
	t.Helper()
	dir := run.CacheDir(stores(t).Options)
	var paths []string
	for _, record := range recordsNaming(t, field) {
		data, err := os.ReadFile(record)
		must(t, err)
		var fields map[string]json.RawMessage
		must(t, json.Unmarshal(data, &fields))
		var b struct {
			Digest digest.Digest `json:"digest"`
		}
		must(t, json.Unmarshal(fields[field], &b))
		paths = append(paths, filepath.Join(dir, "cas", "sha256", b.Digest.Encoded()))
	}
	return paths
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
				`layer "deps": building, since the cached result can't be used: ` +
					"its output can't be used from the cache: blob sha256:",
				`task "size": building, since the cached result can't be used: ` +
					"its files can't be used from the cache: tree sha256:",
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

// cachedBlobs returns the paths of the blobs in the test's build cache.
func cachedBlobs(t *testing.T) map[string]bool {
	t.Helper()
	dir := filepath.Join(run.CacheDir(stores(t).Options), "cas", "sha256")
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

// TestPrune checks that `vamos prune` removes every cached result, so that targets run again,
// and leaves pulled images, and cached images that also have other names, in the container store.
// It has stores of its own, since it empties them, and pulls from a registry of its own,
// a base that its targets only add files to, so that it needn't pull busybox from Docker Hub.
func TestPrune(t *testing.T) {
	requireBuildah(t)
	server, regHost := insecureRegistry(t, nil)
	pushIndex(t, regHost+"/base:v1", native)
	nonce := newNonce()
	fsys := project(t, map[string]string{
		"BUILD": fmt.Sprintf(`
pull(name="base", image="%s/base", tag="v1")
glob(name="input", include=["input.txt"])
layer(name="deps", base="base", steps=[add("input", "/deps")])
task(name="size", base="base", steps=[add("input", "/work")], source="/work", include=["input.txt"])
layer(name="app", base="deps", steps=[add("size", "/app")])
`, regHost),
		"input.txt": nonce + "\n",
	})
	build := func() result {
		t.Helper()
		return vamos(t, fsys, "build", "app", "--tls-verify=false", "-C", projectDir).ran(t)
	}
	// Build in the suite's stores first, to check that pruning the test's own leaves them alone.
	build()
	suiteImages, suiteBlobs := cachedImages(t), cachedBlobs(t)
	if len(suiteImages) == 0 || len(suiteBlobs) == 0 {
		t.Fatalf("building cached %d images and %d files in the suite's stores, want some of each",
			len(suiteImages), len(suiteBlobs))
	}
	usePrivateStores(t)
	build()

	// Name one cached layer's image as a user might, with `buildah tag`.
	// Pulled images carry the cache name too, beside their digest name, so pick an image with no other name.
	kept := "localhost/vamos-test-kept:" + nonce
	var keptID string
	withStore(t, func(store storage.Store) {
		for id := range cachedImages(t) {
			img, err := store.Image(id)
			must(t, err)
			if len(img.Names) == 1 {
				keptID = id
			}
		}
		if keptID == "" {
			t.Fatal("no cached layer image in the container store after building")
		}
		must(t, store.AddNames(keptID, []string{kept}))
	})

	// The pulled base is named by digest.
	var pulled []string
	withStore(t, func(store storage.Store) {
		images, err := store.Images()
		must(t, err)
		for _, img := range images {
			if slices.ContainsFunc(img.Names, func(n string) bool { return strings.HasPrefix(n, regHost+"/base") }) {
				pulled = append(pulled, img.ID)
			}
		}
	})
	if len(pulled) == 0 {
		t.Fatal("no pulled base image in the container store after building")
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
				t.Errorf("pulled base image %s removed by pruning: %v", id[:12], err)
			}
		}
	})
	r = vamos(t, fsys, "prune").ran(t)
	if !strings.HasPrefix(r.stdout, "Removed 0 images from the container store, and 0 results and 0 files") {
		t.Errorf("stdout = %q, want nothing left to remove", r.stdout)
	}
	inSuiteStores(t, func() {
		if gone := newKeys(suiteImages, cachedImages(t)); len(gone) > 0 {
			t.Errorf("cached images %v removed from the suite's store by pruning the test's own", gone)
		}
		if gone := newKeys(suiteBlobs, cachedBlobs(t)); len(gone) > 0 {
			t.Errorf("%d files removed from the suite's build cache by pruning the test's own", len(gone))
		}
	})

	// With the registry gone, the targets run again on the base pulled before.
	server.Close()
	assertTargets(t, build(), map[string]string{`layer "deps"`: "building", `task "size"`: "building"})
}

// TestExplicitStoreRoots checks that a container store given in part is refused, rather than opened,
// since containers/storage would fill in the developer's roots, and the build cache would be put in the working folder.
func TestExplicitStoreRoots(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options storage.StoreOptions
	}{
		{
			name:    "no graph root",
			options: storage.StoreOptions{RunRoot: "/run/vamos-test"},
		},
		{
			name:    "no run root",
			options: storage.StoreOptions{GraphRoot: "/var/lib/vamos-test/storage"},
		},
		{
			name:    "relative graph root",
			options: storage.StoreOptions{GraphRoot: "storage", RunRoot: "/run/vamos-test"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := NewRootCommand(Config{Fs: afero.NewMemMapFs(), Store: &tc.options})
			cmd.SetArgs([]string{"prune"})
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.Execute()
			want := fmt.Sprintf("the container store's graph root %q and run root %q must both be absolute paths",
				tc.options.GraphRoot, tc.options.RunRoot)
			if err == nil || err.Error() != want {
				t.Errorf("prune: %v, want %q", err, want)
			}
		})
	}
}

// defaultStoreHelperEnv marks the test binary run by TestDefaultStore,
// in which TestDefaultStoreHelper prunes the default store.
const defaultStoreHelperEnv = "VAMOS_TEST_DEFAULT_STORE_HELPER"

// TestDefaultStoreHelper prunes, without a store in its configuration, when TestDefaultStore runs it;
// otherwise it does nothing.
func TestDefaultStoreHelper(t *testing.T) {
	if os.Getenv(defaultStoreHelperEnv) == "" {
		return
	}
	cmd := NewRootCommand(Config{Fs: afero.NewMemMapFs()})
	cmd.SetArgs([]string{"prune"})
	must(t, cmd.Execute())
}

// TestDefaultStore checks that vamos, given no store, works on the one that containers/storage finds,
// with the build cache beside it, against a default store of its own.
// containers/storage reads its defaults once per process, so vamos runs in a new one.
func TestDefaultStore(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("configures the default container store, which needs root")
	}
	dir := t.TempDir()
	conf := filepath.Join(dir, "storage.conf")
	must(t, os.WriteFile(conf, fmt.Appendf(nil, `[storage]
driver = "vfs"
graphroot = "%[1]s/default/storage"
runroot = "%[1]s/default/run"
`, dir), 0o644))
	cache := filepath.Join(dir, "default", "vamos")
	must(t, os.MkdirAll(filepath.Join(cache, "cas", "sha256"), 0o755))
	must(t, os.WriteFile(filepath.Join(cache, "cas", "sha256", "stale"), nil, 0o644))

	cmd := exec.Command(os.Args[0], "-test.run", "^TestDefaultStoreHelper$")
	cmd.Env = slices.DeleteFunc(os.Environ(), func(v string) bool { return strings.HasPrefix(v, "STORAGE_OPTS=") })
	cmd.Env = append(cmd.Env, defaultStoreHelperEnv+"=1", "CONTAINERS_STORAGE_CONF="+conf)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pruning the default store failed: %v; output:\n%s", err, out)
	}
	if want := "and 0 results and 1 files"; !strings.Contains(string(out), want) {
		t.Errorf("output lacks %q, for the file in the default store's build cache:\n%s", want, out)
	}
	for _, path := range []string{filepath.Join(dir, "default", "storage"), filepath.Join(dir, "default", "run")} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("pruning didn't open the default store: %v", err)
		}
	}
}

// TestPruneRefusedWhileInUse checks that `vamos prune` is refused while another process holds the build cache open,
// naming it, and removes nothing then: no image, name, result or file; and that a build is refused while another
// process prunes.
func TestPruneRefusedWhileInUse(t *testing.T) {
	requireBuildah(t)
	usePrivateStores(t)
	fsys := project(t, map[string]string{
		"BUILD": `
glob(name="input", include=["input.txt"])
layer(name="app", base="scratch", steps=[add("input", "/app")])
`,
		"input.txt": newNonce() + "\n",
	})
	vamos(t, fsys, "build", "app", "-C", projectDir).ran(t)
	dir := run.CacheDir(stores(t).Options)
	names := func() map[string][]string {
		t.Helper()
		names := map[string][]string{}
		withStore(t, func(store storage.Store) {
			images, err := store.Images()
			must(t, err)
			for _, img := range images {
				names[img.ID] = img.Names
			}
		})
		return names
	}
	results := func() []string {
		t.Helper()
		entries, err := os.ReadDir(filepath.Join(dir, "ac"))
		must(t, err)
		var keys []string
		for _, e := range entries {
			keys = append(keys, e.Name())
		}
		return keys
	}
	images, blobs, records := names(), cachedBlobs(t), results()
	if len(cachedImages(t)) == 0 || len(blobs) == 0 || len(records) == 0 {
		t.Fatalf("building cached %d images, %d files and %d results, want some of each",
			len(cachedImages(t)), len(blobs), len(records))
	}

	stop := holdDirectory(t, dir, false)
	vamos(t, project(t, nil), "prune").failsWith(t, false, "another vamos is using the build cache")
	stop()
	if got := names(); !maps.EqualFunc(got, images, slices.Equal) {
		t.Errorf("images after a refused prune = %v, want them as they were, %v", got, images)
	}
	if got := cachedBlobs(t); !maps.Equal(got, blobs) {
		t.Errorf("a refused prune left %d of %d files", len(got), len(blobs))
	}
	if got := results(); !slices.Equal(got, records) {
		t.Errorf("results after a refused prune = %v, want %v", got, records)
	}

	stop = holdDirectory(t, dir, true)
	vamos(t, fsys, "build", "app", "-C", projectDir).failsWith(t, false, "another vamos is pruning the build cache")
	stop()
	vamos(t, project(t, nil), "prune").ran(t)
}

// holdDirectory runs the test binary's helper, which holds the build cache at dir open, or pruning it if prune,
// until the returned function is called, and waits for it to hold before returning.
func holdDirectory(t *testing.T, dir string, prune bool) func() {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", "^TestHoldDirectoryHelper$")
	cmd.Env = append(os.Environ(), "VAMOS_TEST_HOLD_DIR="+dir)
	if prune {
		cmd.Env = append(cmd.Env, "VAMOS_TEST_HOLD_PRUNE=1")
	}
	out := &strings.Builder{}
	cmd.Stdout, cmd.Stderr = out, out
	must(t, cmd.Start())
	ready := filepath.Join(dir, "ready")
	for start := time.Now(); ; {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Since(start) > 10*time.Second {
			cmd.Process.Kill()
			t.Fatalf("the helper didn't take the lock in time:\n%s", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return func() {
		os.Remove(ready)
		if err := cmd.Wait(); err != nil {
			t.Errorf("the helper failed: %v\n%s", err, out)
		}
	}
}

// TestHoldDirectoryHelper holds the build cache that VAMOS_TEST_HOLD_DIR names open, or exclusively, as a prune,
// if VAMOS_TEST_HOLD_PRUNE is set, until its ready file is removed; otherwise it does nothing.
func TestHoldDirectoryHelper(t *testing.T) {
	dir := os.Getenv("VAMOS_TEST_HOLD_DIR")
	if dir == "" {
		return
	}
	d, err := cache.OpenDirectory(dir)
	must(t, err)
	defer d.Close()
	ready := filepath.Join(dir, "ready")
	hold := func() {
		must(t, os.WriteFile(ready, nil, 0o600))
		for {
			if _, err := os.Stat(ready); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if os.Getenv("VAMOS_TEST_HOLD_PRUNE") == "" {
		hold()
		return
	}
	must(t, cache.HoldExclusive(d, hold))
}

// readImage reads the image in the tarball at path.
func readImage(t *testing.T, path string) v1.Image {
	t.Helper()
	img, err := tarball.ImageFromPath(path, nil)
	must(t, err)
	return img
}
