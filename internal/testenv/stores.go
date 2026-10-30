package testenv

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/moby/sys/mountinfo"
	"go.podman.io/buildah"
	"go.podman.io/storage"
)

// RequireIntegrationEnv is the environment variable that, when set, makes tests which need buildah,
// root or ownership changes fail, rather than skip, where those are unavailable:
// for a host that is meant to run the whole suite.
const RequireIntegrationEnv = "VAMOS_REQUIRE_INTEGRATION"

// unavailable reports that what the test needs, what, isn't available here, as err says:
// the test fails if RequireIntegrationEnv is set, and is skipped otherwise.
func unavailable(t *testing.T, what string, err error) {
	t.Helper()
	msg := fmt.Sprintf("%s isn't possible here: %v", what, err)
	if os.Getenv(RequireIntegrationEnv) != "" {
		t.Fatalf("%s, but %s is set", msg, RequireIntegrationEnv)
	}
	t.Skip(msg)
}

// RequireBuildah skips tests that build images with -short,
// and otherwise checks that they can be built here: as root, in a container store that opens,
// with a container that buildah can make and mount.
// Where they can't, the test is skipped, or fails if RequireIntegrationEnv is set.
// The test binary's TestMain must be Main, as for any test that builds.
func RequireBuildah(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("builds an image; skipped with -short")
	}
	if err := buildahAvailable(); err != nil {
		unavailable(t, "building an image", err)
	}
}

// buildahAvailable probes, once, whether images can be built here: it needs root,
// and the suite's container store must open, which fails on a filesystem its driver can't use.
// It then makes and mounts a container from scratch, as every build does first, which needs nothing from a registry:
// that also needs buildah's network backend, whose lock is on /run, and the driver's mounts.
// Steps that run commands need more, such as an isolation that works here; their tests fail where it doesn't.
var buildahAvailable = sync.OnceValue(func() error {
	if os.Geteuid() != 0 {
		return errors.New("it needs root")
	}
	s, err := suiteStores()
	if err != nil {
		return err
	}
	store, err := storage.GetStore(s.Options)
	if err != nil {
		return fmt.Errorf("opening a container store at %s: %w", s.Options.GraphRoot, err)
	}
	err = probeBuilder(store)
	if _, shutdownErr := store.Shutdown(false); err == nil {
		err = shutdownErr
	}
	return err
})

// probeBuilder makes a container from scratch in store, mounts it, and deletes it.
func probeBuilder(store storage.Store) error {
	builder, err := buildah.NewBuilder(context.Background(), store, buildah.BuilderOptions{
		FromImage:  "scratch",
		PullPolicy: buildah.PullNever,
	})
	if err != nil {
		return fmt.Errorf("making a container from scratch: %w", err)
	}
	_, err = builder.Mount("")
	if err != nil {
		err = fmt.Errorf("mounting a container: %w", err)
	}
	return errors.Join(err, builder.Delete())
}

// RequireOwnership checks that tests which give files other owners can do so here,
// by trying it on a temporary file: the test is skipped if they can't, or fails if RequireIntegrationEnv is set.
func RequireOwnership(t *testing.T) {
	t.Helper()
	if err := ownershipAvailable(); err != nil {
		unavailable(t, "changing a file's owner", err)
	}
}

// ownershipAvailable probes, once, whether files can be given other owners.
// Exports give files owners only as root, so it needs root, as well as a filesystem on which that works.
var ownershipAvailable = sync.OnceValue(func() error {
	if os.Geteuid() != 0 {
		return errors.New("it needs root")
	}
	f, err := os.CreateTemp("", "vamos-owner-")
	if err != nil {
		return err
	}
	f.Close()
	defer os.Remove(f.Name())
	return os.Lchown(f.Name(), 1234, 1235)
})

// Stores is a container store and the build cache beside it, made for tests, in a directory of their own,
// so that tests never touch the developer's.
type Stores struct {
	Dir     string               // holds storage/, the store, run/, its run root, and vamos/, the build cache
	Options storage.StoreOptions // the store's, which a local executor opens it with
}

// UnnamedImages returns the IDs of the images in the container store that have no name,
// such as an image a failed target might leave behind; cached layers' images are named.
func (s *Stores) UnnamedImages() (map[string]bool, error) {
	return s.ImageIDs(func(img storage.Image) bool { return len(img.Names) == 0 })
}

// ImageIDs returns the IDs of the images in the container store for which keep is true.
func (s *Stores) ImageIDs(keep func(storage.Image) bool) (map[string]bool, error) {
	ids := map[string]bool{}
	err := s.WithStore(func(store storage.Store) error {
		images, err := store.Images()
		if err != nil {
			return err
		}
		for _, img := range images {
			if keep(img) {
				ids[img.ID] = true
			}
		}
		return nil
	})
	return ids, err
}

// ContainerIDs returns the IDs of the containers in the container store, such as one that a step runs in.
func (s *Stores) ContainerIDs() (map[string]bool, error) {
	ids := map[string]bool{}
	err := s.WithStore(func(store storage.Store) error {
		containers, err := store.Containers()
		for _, c := range containers {
			ids[c.ID] = true
		}
		return err
	})
	return ids, err
}

// WithStore calls use with the container store, open only until use returns,
// since each vamos run opens and shuts it too.
func (s *Stores) WithStore(use func(storage.Store) error) error {
	store, err := storage.GetStore(s.Options)
	if err != nil {
		return err
	}
	defer store.Shutdown(false)
	return use(store)
}

// suiteStores makes the stores that a test binary's tests share, once.
var suiteStores = sync.OnceValues(func() (*Stores, error) {
	suiteMade.Store(true)
	return newStores("suite")
})

// SuiteStores returns the stores that the test binary's tests share.
// Tests that destroy what a store holds, such as by pruning, must use PrivateStores instead.
func SuiteStores(t *testing.T) *Stores {
	t.Helper()
	s, err := suiteStores()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// PrivateStores returns stores for the test alone, removed once it ends.
func PrivateStores(t *testing.T) *Stores {
	t.Helper()
	s, err := newStores(storesName(t.Name()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.remove(); err != nil {
			t.Errorf("removing the test's stores: %v", err)
		}
	})
	return s
}

// storesName returns the test name as part of the name of its stores' directory: "/" as "-", and any other character
// but a letter, a digit, "_", "." and "-" as "_", since overlay mount options, which name the directory, are separated
// by commas and can't hold "=" or ":" in a path. A long name is cut short, and a hash of it kept, since every layer
// of a mount names the directory, and mount options are limited to a page.
func storesName(test string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r == '/':
			return '-'
		case strings.ContainsRune("_.-", r), 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9':
			return r
		}
		return '_'
	}, test)
	if len(name) > 64 {
		h := fnv.New32a()
		h.Write([]byte(test))
		name = fmt.Sprintf("%s-%08x", name[:48], h.Sum32())
	}
	return name
}

// RemoveSuiteStores removes the shared stores, if they were made.
// A TestMain that may have used them calls it after running the tests.
func RemoveSuiteStores() error {
	if !suiteMade.Load() {
		return nil
	}
	s, err := suiteStores()
	if err != nil {
		return nil // nothing was made
	}
	return s.remove()
}

// suiteMade reports whether suiteStores has been called.
var suiteMade atomic.Bool

// newStores makes stores in a new directory named after name.
// The directory is beside the default container store, whose filesystem its driver is known to work on:
// a container's own overlay filesystem, as /tmp often is, can't hold an overlay store.
// Where that can't be written, as without root, it is in the temporary directory, where non-building tests
// can still use it.
// The store takes the default driver and its options, so that it works as the developer's does.
func newStores(name string) (*Stores, error) {
	options, err := storage.DefaultStoreOptions()
	if err != nil {
		return nil, fmt.Errorf("reading the default container storage options: %w", err)
	}
	pattern := "vamos-test-" + name + "-"
	dir, err := os.MkdirTemp(filepath.Dir(options.GraphRoot), pattern)
	if err != nil {
		if dir, err = os.MkdirTemp("", pattern); err != nil {
			return nil, err
		}
	}
	options.GraphRoot = filepath.Join(dir, "storage")
	options.RunRoot = filepath.Join(dir, "run")
	options.ImageStore = ""
	options.TransientStore = false
	options.GraphDriverOptions = withoutSharedStores(options.GraphDriverOptions)
	return &Stores{Dir: dir, Options: options}, nil
}

// withoutSharedStores returns the driver options without stores that the store would share with others:
// additional image stores, which would show the developer's images to tests, hiding redundant pulls,
// and additional layer stores, which would serve layers to tests and count their use.
// containers/storage emits them as imagestore=<dir> and additionallayerstore=<dir>,
// and the overlay driver also takes additionalimagestore=<dir>.
// Keys are matched as the driver parses them: trimmed, in any case, and with or without a driver's prefix,
// as in overlay.imagestore=<dir>.
func withoutSharedStores(options []string) []string {
	var kept []string
	for _, o := range options {
		key, _, _ := strings.Cut(o, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		if _, name, ok := strings.Cut(key, "."); ok {
			key = name
		}
		switch key {
		case "imagestore", "additionalimagestore", "additionallayerstore":
		default:
			kept = append(kept, o)
		}
	}
	return kept
}

// remove unmounts and removes the stores.
// A store that never opened, as when the probe found its driver can't use the filesystem, has nothing mounted,
// so its directory is removed all the same.
// Where something under the directory is still mounted, it is left as it is, since removing it would remove
// the store's records while the mounts that they describe remain.
func (s *Stores) remove() error {
	if _, err := os.Stat(s.Options.GraphRoot); err == nil {
		if store, err := storage.GetStore(s.Options); err == nil {
			if err := shutdown(store); err != nil {
				return fmt.Errorf("shutting down the container store at %s, so leaving %s: %w",
					s.Options.GraphRoot, s.Dir, err)
			}
		}
	}
	mounts, err := mountsUnder(s.Dir)
	if err != nil {
		return fmt.Errorf("listing mounts, so leaving %s: %w", s.Dir, err)
	}
	if len(mounts) > 0 {
		return fmt.Errorf("leaving %s, since these are still mounted: %s", s.Dir, strings.Join(mounts, ", "))
	}
	return os.RemoveAll(s.Dir)
}

// shutdown unmounts every layer of store, and then the driver's own mounts.
// Shutdown reports that a layer is used if any was mounted when it started, even once it has unmounted them,
// and then leaves the driver mounted; called again, with none mounted, it unmounts the driver too.
func shutdown(store storage.Store) error {
	_, err := store.Shutdown(true)
	if errors.Is(err, storage.ErrLayerUsedByContainer) {
		_, err = store.Shutdown(true)
	}
	return err
}

// mountsUnder returns the mount points at dir or under it.
func mountsUnder(dir string) ([]string, error) {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved // mount points are listed by their real paths
	}
	infos, err := mountinfo.GetMounts(mountinfo.PrefixFilter(dir))
	if err != nil {
		return nil, err
	}
	var mounts []string
	for _, info := range infos {
		mounts = append(mounts, info.Mountpoint)
	}
	return mounts, nil
}
