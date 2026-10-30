package testenv

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.podman.io/buildah"
	"go.podman.io/storage"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	// RequireBuildah makes a container, as builds do.
	if buildah.InitReexec() {
		return
	}
	code := m.Run()
	if err := RemoveSuiteStores(); err != nil {
		fmt.Fprintf(os.Stderr, "removing the suite's stores: %v\n", err)
		code = 1
	}
	os.Exit(code)
}

// TestWithoutSharedStores checks that every way of naming an additional image or layer store is dropped,
// as containers/storage would parse it, and nothing else.
func TestWithoutSharedStores(t *testing.T) {
	got := withoutSharedStores([]string{
		"imagestore=/usr/lib/containers/storage",
		"overlay.imagestore=/a",
		"additionalimagestore=/b",
		"overlay.additionalimagestore=/c",
		"overlay.ImageStore=/var/lib/containers/storage",
		"imagestore =/e",
		" AdditionalImageStore = /f",
		"vfs.imagestore=/g",
		"additionallayerstore=/d",
		"overlay2.additionallayerstore=/h:ref",
		"overlay.mountopt=nodev",
		"mount_program=/usr/bin/fuse-overlayfs",
	})
	want := []string{"overlay.mountopt=nodev", "mount_program=/usr/bin/fuse-overlayfs"}
	if !slices.Equal(got, want) {
		t.Errorf("kept %q, want %q", got, want)
	}
}

// TestRemoveMounted checks that removing stores with a layer still mounted, as a test that failed might leave them,
// unmounts everything, the driver's own mounts too, before removing the directory.
func TestRemoveMounted(t *testing.T) {
	RequireBuildah(t)
	s, err := newStores("TestRemoveMounted")
	Must(t, err)
	store, err := storage.GetStore(s.Options)
	Must(t, err)
	layer, err := store.CreateLayer("", "", nil, "", true, nil)
	Must(t, err)
	_, err = store.Mount(layer.ID, "")
	Must(t, err)
	if _, err := store.Shutdown(false); !errors.Is(err, storage.ErrLayerUsedByContainer) {
		t.Fatalf("shutting down with a layer mounted: %v, want %v", err, storage.ErrLayerUsedByContainer)
	}

	if err := s.remove(); err != nil {
		t.Fatalf("removing the stores: %v", err)
	}
	if _, err := os.Stat(s.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s is left after removing the stores: %v", s.Dir, err)
	}
	mounts, err := mountsUnder(s.Dir)
	Must(t, err)
	if len(mounts) > 0 {
		t.Errorf("%v left mounted after removing the stores", mounts)
	}
}

// TestRemoveLeavesMounts checks that stores under which something the store doesn't know of is still mounted
// are left whole, rather than removed through the mount, saying so.
func TestRemoveLeavesMounts(t *testing.T) {
	RequireBuildah(t)
	s, err := newStores("TestRemoveLeavesMounts")
	Must(t, err)
	store, err := storage.GetStore(s.Options)
	Must(t, err)
	_, err = store.Shutdown(false)
	Must(t, err)
	outside := t.TempDir()
	Must(t, os.WriteFile(filepath.Join(outside, "sentinel"), []byte("kept"), 0o644))
	mountPoint := filepath.Join(s.Dir, "mounted")
	Must(t, os.Mkdir(mountPoint, 0o755))
	Must(t, unix.Mount(outside, mountPoint, "", unix.MS_BIND, ""))
	t.Cleanup(func() {
		Must(t, unix.Unmount(mountPoint, 0))
		Must(t, s.remove())
	})

	err = s.remove()
	if err == nil || !strings.Contains(err.Error(), "since these are still mounted: ") ||
		!strings.Contains(err.Error(), mountPoint) {
		t.Errorf("removing the stores: %v, want it to name %s, still mounted", err, mountPoint)
	}
	for _, path := range []string{s.Options.GraphRoot, filepath.Join(outside, "sentinel")} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s removed with something under the stores mounted: %v", path, err)
		}
	}
}

// isolationHelperEnv marks the test binary run by TestIsolation, in which TestIsolationHelper makes stores.
const isolationHelperEnv = "VAMOS_TEST_ISOLATION_HELPER"

// TestIsolationHelper makes private stores, opens them, and removes them, when TestIsolation runs it,
// printing their options; otherwise it does nothing.
func TestIsolationHelper(t *testing.T) {
	if os.Getenv(isolationHelperEnv) == "" {
		return
	}
	s, err := newStores("isolation")
	Must(t, err)
	store, err := storage.GetStore(s.Options)
	Must(t, err)
	_, err = store.Shutdown(false)
	Must(t, err)
	out, err := json.Marshal(s)
	Must(t, err)
	Must(t, s.remove())
	fmt.Printf("stores: %s\n", out)
}

// TestIsolation checks, against a default store of its own with additional image and layer stores,
// configured both in storage.conf and with STORAGE_OPTS, that stores for tests are made beside the default store,
// share none of its stores, and are removed without touching any of them.
// containers/storage reads its defaults once per process, so the stores are made in a new one.
func TestIsolation(t *testing.T) {
	if os.Geteuid() != 0 {
		unavailable(t, "configuring the default container store", errors.New("it needs root"))
	}
	for _, tc := range []struct {
		name        string
		storageOpts string // the STORAGE_OPTS that the helper is run with, if any
	}{
		{
			name: "storage.conf",
		},
		{
			name:        "STORAGE_OPTS image store",
			storageOpts: "vfs.ImageStore=%[1]s/images",
		},
		{
			name:        "STORAGE_OPTS layer store",
			storageOpts: " AdditionalLayerStore = %[1]s/layers",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			sentinels := map[string]string{}
			for _, sub := range []string{"default/storage", "default/run", "images", "layers"} {
				path := filepath.Join(dir, sub, "sentinel")
				Must(t, os.MkdirAll(filepath.Dir(path), 0o755))
				Must(t, os.WriteFile(path, []byte(sub), 0o644))
				sentinels[path] = sub
			}
			conf := filepath.Join(dir, "storage.conf")
			Must(t, os.WriteFile(conf, fmt.Appendf(nil, `[storage]
driver = "vfs"
graphroot = "%[1]s/default/storage"
runroot = "%[1]s/default/run"

[storage.options]
additionalimagestores = ["%[1]s/images"]
additionallayerstores = ["%[1]s/layers"]
`, dir), 0o644))

			cmd := exec.Command(os.Args[0], "-test.run", "^TestIsolationHelper$")
			// Even an empty STORAGE_OPTS replaces storage.conf's driver options, so it is unset unless the case
			// sets it.
			storageOpts := func(v string) bool { return strings.HasPrefix(v, "STORAGE_OPTS=") }
			cmd.Env = slices.DeleteFunc(os.Environ(), storageOpts)
			cmd.Env = append(cmd.Env, isolationHelperEnv+"=1", "CONTAINERS_STORAGE_CONF="+conf)
			if tc.storageOpts != "" {
				cmd.Env = append(cmd.Env, "STORAGE_OPTS="+fmt.Sprintf(tc.storageOpts, dir))
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("the helper failed: %v; output:\n%s", err, out)
			}
			_, found, ok := strings.Cut(string(out), "stores: ")
			if !ok {
				t.Fatalf("the helper printed no stores:\n%s", out)
			}
			var s Stores
			Must(t, json.Unmarshal([]byte(strings.SplitN(found, "\n", 2)[0]), &s))

			if got := filepath.Dir(s.Dir); got != filepath.Join(dir, "default") {
				t.Errorf("stores made in %s, want beside the default store, in %s", got, filepath.Join(dir, "default"))
			}
			if want := filepath.Join(s.Dir, "storage"); s.Options.GraphRoot != want {
				t.Errorf("graph root %s, want %s", s.Options.GraphRoot, want)
			}
			if want := filepath.Join(s.Dir, "run"); s.Options.RunRoot != want {
				t.Errorf("run root %s, want %s", s.Options.RunRoot, want)
			}
			for _, o := range s.Options.GraphDriverOptions {
				if strings.Contains(o, dir) {
					t.Errorf("driver option %q shares a store of the developer's", o)
				}
			}
			if _, err := os.Stat(s.Dir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s is left after removing the stores: %v", s.Dir, err)
			}
			for path, want := range sentinels {
				if got, err := os.ReadFile(path); err != nil || string(got) != want {
					t.Errorf("%s holds %q, %v after making and removing stores, want %q", path, got, err, want)
				}
			}
		})
	}
}

// gateHelperEnv marks the test binary run by TestGate, in which TestGateHelper reports an unavailable prerequisite.
const gateHelperEnv = "VAMOS_TEST_GATE_HELPER"

// TestGateHelper reports that a prerequisite is unavailable, when TestGate runs it; otherwise it does nothing.
func TestGateHelper(t *testing.T) {
	if os.Getenv(gateHelperEnv) == "" {
		return
	}
	unavailable(t, "a test prerequisite", errors.New("it is missing here"))
}

// TestGate checks that an unavailable prerequisite skips the test unless RequireIntegrationEnv is set,
// when it fails, saying so.
func TestGate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		required string
		fails    bool
		want     string
	}{
		{name: "skips", required: "", fails: false,
			want: "a test prerequisite isn't possible here: it is missing here"},
		{name: "fails when required", required: "1", fails: true,
			want: "a test prerequisite isn't possible here: it is missing here, but VAMOS_REQUIRE_INTEGRATION is set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run", "^TestGateHelper$", "-test.v")
			cmd.Env = append(os.Environ(), gateHelperEnv+"=1", RequireIntegrationEnv+"="+tc.required)
			out, err := cmd.CombinedOutput()
			if failed := err != nil; failed != tc.fails {
				t.Errorf("the helper failed = %v, want %v; output:\n%s", failed, tc.fails, out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("output lacks %q:\n%s", tc.want, out)
			}
			if verb := "--- SKIP"; !tc.fails && !strings.Contains(string(out), verb) {
				t.Errorf("output lacks %q:\n%s", verb, out)
			}
		})
	}
}

// TestStoresName checks that a test's name becomes a directory name that overlay mount options can hold.
func TestStoresName(t *testing.T) {
	got := storesName("TestPull_X/After a prune, builds run: a=b")
	if want := "TestPull_X-After_a_prune__builds_run__a_b"; got != want {
		t.Errorf("storesName = %q, want %q", got, want)
	}
	long := strings.Repeat("Scenario", 20)
	if got := storesName(long); len(got) != 57 || !strings.HasPrefix(got, long[:48]+"-") {
		t.Errorf("storesName of a long name = %q, want its first 48 characters and a hash", got)
	}
	if storesName(long+"a") == storesName(long+"b") {
		t.Error("storesName gives two long names that differ at their end the same name")
	}
}
