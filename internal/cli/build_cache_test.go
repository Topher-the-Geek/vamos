package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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

// readImage reads the image in the tarball at path.
func readImage(t *testing.T, path string) v1.Image {
	t.Helper()
	img, err := tarball.ImageFromPath(path, nil)
	must(t, err)
	return img
}
