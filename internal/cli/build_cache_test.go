package cli

import (
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

// recordsNaming returns the paths of the test's action results that have the field, such as "epoch".
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
