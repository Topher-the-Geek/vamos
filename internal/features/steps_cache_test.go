package features

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/cli"
	"github.com/Topher-the-Geek/vamos/internal/run"
	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/opencontainers/go-digest"
	imagemanifest "go.podman.io/image/v5/manifest"
	"go.podman.io/storage"
)

// Steps about the build cache and the container store as other programs see them: Given steps that damage them,
// as something outside vamos might, a disk that corrupts a file, a person who removes one, or another tool, or that
// hold them as another vamos does; and Then steps that see what they hold, as `podman images` or a look at the disk
// would. They reach into the cache's own files, as no user step can, and only stores of the scenario's own, so that
// they damage nothing another scenario uses.

// requireOwnStores fails the scenario unless its stores are its own, which a step that damages them needs.
func (w *world) requireOwnStores() {
	w.t.Helper()
	if !w.ownStores {
		w.t.Fatal("a step that damages the stores needs the scenario's own; give GivenStoresOfTheScenariosOwn")
	}
}

// cacheDir returns the scenario's build cache, failing the scenario unless its stores are its own.
func (w *world) cacheDir() string {
	w.t.Helper()
	w.requireOwnStores()
	return run.CacheDir(w.stores.Options)
}

// cached is what stores hold: the files and results in the build cache, by path, the images in the container store
// that the build cache names, by store ID, and every image's names, by store ID, for steps that compare what stores
// hold before and after a command.
type cached struct {
	blobs, records, images map[string]bool
	names                  map[string]string
}

// holds reports whether c holds everything that other does: its files, results and images, with the same names.
func (c cached) holds(other cached) bool {
	kinds := [][2]map[string]bool{{c.blobs, other.blobs}, {c.records, other.records}, {c.images, other.images}}
	for _, pair := range kinds {
		for key := range pair[1] {
			if !pair[0][key] {
				return false
			}
		}
	}
	for id, names := range other.names {
		if c.names[id] != names {
			return false
		}
	}
	return true
}

// same reports whether c and other hold the same files, results and images, every image with the same names.
func (c cached) same(other cached) bool {
	return maps.Equal(c.blobs, other.blobs) && maps.Equal(c.records, other.records) &&
		maps.Equal(c.images, other.images) && maps.Equal(c.names, other.names)
}

// String says how much c holds, for a failure's message.
func (c cached) String() string {
	return fmt.Sprintf("%d files, %d results and %d images, %d of them cached", len(c.blobs), len(c.records),
		len(c.names), len(c.images))
}

// cached returns what the scenario's stores hold.
func (w *world) cached() cached {
	w.t.Helper()
	return w.cachedIn(w.stores)
}

// cachedIn returns what stores hold.
func (w *world) cachedIn(stores *testenv.Stores) cached {
	w.t.Helper()
	held := cached{blobs: w.entriesOf(stores, "cas", "sha256"), records: w.entriesOf(stores, "ac"),
		images: map[string]bool{}, names: map[string]string{}}
	err := stores.WithStore(func(store storage.Store) error {
		all, err := store.Images()
		for _, img := range all {
			if _, named := cacheName(img); named {
				held.images[img.ID] = true
			}
			held.names[img.ID] = strings.Join(slices.Sorted(slices.Values(img.Names)), " ")
		}
		return err
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return held
}

// entriesOf returns the paths of the entries in the directory of stores' build cache that dirs name.
func (w *world) entriesOf(stores *testenv.Stores, dirs ...string) map[string]bool {
	w.t.Helper()
	dir := filepath.Join(append([]string{run.CacheDir(stores.Options)}, dirs...)...)
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		w.t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, e := range entries {
		paths[filepath.Join(dir, e.Name())] = true
	}
	return paths
}

// GivenTheCachedBlobOfIsCorrupt replaces the bytes of the blob in the build cache that holds content with as many
// other bytes, so that only reading it tells it is corrupt.
func (w *world) GivenTheCachedBlobOfIsCorrupt(content string) {
	w.t.Helper()
	w.corrupt(filepath.Join(w.cacheDir(), "cas", "sha256", digest.FromString(content).Encoded()))
}

// corrupt replaces the bytes of the file at path with as many other bytes.
func (w *world) corrupt(path string) {
	w.t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), int(info.Size())), 0o600); err != nil {
		w.t.Fatal(err)
	}
}

// GivenEveryCachedLogIsCorrupt corrupts, as GivenTheCachedBlobOfIsCorrupt does, the blob of every log that a result
// in the build cache names, which holds what its steps printed.
func (w *world) GivenEveryCachedLogIsCorrupt() {
	w.t.Helper()
	for _, path := range w.namedBlobs("log") {
		w.corrupt(path)
	}
}

// GivenEveryCachedLogIsMalformed makes every result in the build cache that names a log name one whose content
// matches its digest, but whose record promises more bytes than follow.
func (w *world) GivenEveryCachedLogIsMalformed() {
	w.t.Helper()
	w.pointRecordsAt("log", []byte{cache.StreamStdout, 10, 'h', 'i'})
}

// GivenEveryCachedTasksFilesAreAMalformedTree makes every result in the build cache that names a task's files name
// a tree whose content matches its digest, but which no walk could make: one with an entry outside the tree.
func (w *world) GivenEveryCachedTasksFilesAreAMalformedTree() {
	w.t.Helper()
	w.pointRecordsAt("output", []byte(`{"entries":[{"path":"../x","type":"dir"}]}`))
}

// pointRecordsAt stores content in the build cache, and makes every result that has the field, "log" or "output",
// name it.
func (w *world) pointRecordsAt(field string, content []byte) {
	w.t.Helper()
	store, err := cache.OpenDirectory(w.cacheDir())
	if err != nil {
		w.t.Fatal(err)
	}
	defer store.Close() // which holds the cache open, as a build does, and a prune would refuse
	b, err := store.PutBytes(w.t.Context(), content)
	if err != nil {
		w.t.Fatal(err)
	}
	for path, fields := range w.recordsNaming(field) {
		if fields[field], err = json.Marshal(b); err != nil {
			w.t.Fatal(err)
		}
		data, err := json.Marshal(fields)
		if err != nil {
			w.t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			w.t.Fatal(err)
		}
	}
}

// recordsNaming returns the fields of the results in the build cache that have the field, "log" or "output",
// by path.
func (w *world) recordsNaming(field string) map[string]map[string]json.RawMessage {
	w.t.Helper()
	dir := filepath.Join(w.cacheDir(), "ac")
	entries, err := os.ReadDir(dir)
	if err != nil {
		w.t.Fatal(err)
	}
	records := map[string]map[string]json.RawMessage{}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			w.t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			w.t.Fatal(err)
		}
		if _, ok := fields[field]; ok {
			records[path] = fields
		}
	}
	if len(records) == 0 {
		w.t.Fatalf("no result in the build cache names a %s", field)
	}
	return records
}

// namedBlobs returns the paths of the blobs that the results in the build cache name in field, "log" or "output".
func (w *world) namedBlobs(field string) []string {
	w.t.Helper()
	dir := filepath.Join(w.cacheDir(), "cas", "sha256")
	var paths []string
	for _, fields := range w.recordsNaming(field) {
		var b struct {
			Digest digest.Digest `json:"digest"`
		}
		if err := json.Unmarshal(fields[field], &b); err != nil {
			w.t.Fatal(err)
		}
		paths = append(paths, filepath.Join(dir, b.Digest.Encoded()))
	}
	return paths
}

// lastCached returns what after holds and before doesn't: what the last command cached, which Given steps since
// haven't. It stops the scenario if no command has run, or the last cached nothing.
func (w *world) lastCached(before, after map[string]bool) []string {
	w.t.Helper()
	if !w.ran {
		w.t.Fatal("no command has run to have cached anything")
	}
	var added []string
	for key := range after {
		if !before[key] {
			added = append(added, key)
		}
	}
	if len(added) == 0 {
		w.t.Fatal("the last command cached nothing of the kind to remove")
	}
	return added
}

// GivenTheBlobsTheLastCommandCachedAreRemoved removes from the build cache every blob that the last command added,
// as a person tidying the disk might, leaving the results that name them.
func (w *world) GivenTheBlobsTheLastCommandCachedAreRemoved() {
	w.t.Helper()
	w.requireOwnStores()
	paths := w.lastCached(w.cachedBefore.blobs, w.cachedAfter.blobs)
	for _, path := range paths {
		if err := os.Remove(path); err != nil {
			w.t.Fatal(err)
		}
	}
}

// GivenTheImagesTheLastCommandCachedAreRemoved removes from the container store every image that the build cache
// names and that the last command added, as `podman rmi` might, leaving the results that name them.
func (w *world) GivenTheImagesTheLastCommandCachedAreRemoved() {
	w.t.Helper()
	w.requireOwnStores()
	ids := w.lastCached(w.cachedBefore.images, w.cachedAfter.images)
	err := w.stores.WithStore(func(store storage.Store) error {
		for _, id := range ids {
			if _, err := store.DeleteImage(id, true); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		w.t.Fatal(err)
	}
}

// GivenTheFirstSavedImagesCacheNameNamesTheLast moves the name in the build cache's repository of the first image
// that the scenario saved to the last, as `podman tag` can.
func (w *world) GivenTheFirstSavedImagesCacheNameNamesTheLast() {
	w.t.Helper()
	w.requireOwnStores()
	if len(w.saved) < 2 {
		w.t.Fatal("the scenario has saved no second image to give the first's name")
	}
	first, last := w.imageID(w.saved[0]), w.imageID(w.saved[len(w.saved)-1])
	if first == last {
		w.t.Fatal("the last image saved is the first")
	}
	err := w.stores.WithStore(func(store storage.Store) error {
		img, err := store.Image(run.CacheRepository + ":" + last)
		if err != nil {
			return err
		}
		return store.AddNames(img.ID, []string{run.CacheRepository + ":" + first})
	})
	if err != nil {
		w.t.Fatal(err)
	}
}

// GivenTheLastSavedImageHasOtherLayersInTheStore gives the name that the build cache has for the image last saved to
// another image in the container store, with its config but other layers, holding app.sh, as a partial pull can leave
// with storage.conf's insecure_allow_unpredictable_image_contents, leaving the image it named in the store unnamed.
func (w *world) GivenTheLastSavedImageHasOtherLayersInTheStore() {
	w.t.Helper()
	w.requireOwnStores()
	if len(w.saved) == 0 {
		w.t.Fatal("the scenario has saved no image")
	}
	id := w.imageID(w.saved[len(w.saved)-1])
	err := w.stores.WithStore(func(store storage.Store) error {
		img, err := store.Image(run.CacheRepository + ":" + id)
		if err != nil {
			return err
		}
		var other bytes.Buffer
		tw := tar.NewWriter(&other)
		content := []byte("echo bye\n")
		if err := tw.WriteHeader(&tar.Header{Name: "app.sh", Mode: 0o755, Size: int64(len(content))}); err != nil {
			return err
		}
		if _, err := tw.Write(content); err != nil {
			return err
		}
		if err := tw.Close(); err != nil {
			return err
		}
		layer, _, err := store.PutLayer("", "", nil, "", false, nil, &other)
		if err != nil {
			return err
		}
		unverified, err := store.CreateImage("", nil, layer.ID, "",
			&storage.ImageOptions{CreationDate: img.Created, Digest: img.Digest})
		if err != nil {
			return err
		}
		if err := copyImageData(store, img, unverified.ID); err != nil {
			return err
		}
		return store.AddNames(unverified.ID, []string{run.CacheRepository + ":" + id})
	})
	if err != nil {
		w.t.Fatal(err)
	}
}

// GivenTheStoreHoldsTheCachedImagesUnderOtherIDs recreates each image that the build cache names in the container
// store under a random store ID, with its layers, data, digests and names, as a store can hold an image whose store
// ID isn't its config's digest.
func (w *world) GivenTheStoreHoldsTheCachedImagesUnderOtherIDs() {
	w.t.Helper()
	w.requireOwnStores()
	rekeyed := 0
	err := w.stores.WithStore(func(store storage.Store) error {
		images, err := store.Images()
		if err != nil {
			return err
		}
		for _, img := range images {
			id, named := cacheName(img)
			if !named {
				continue
			}
			rekeyed++
			copied, err := store.CreateImage("", nil, img.TopLayer, img.Metadata,
				&storage.ImageOptions{CreationDate: img.Created, Digest: img.Digest})
			if err != nil {
				return err
			}
			if copied.ID == id {
				return fmt.Errorf("image %s would be held under its image ID still", id)
			}
			if err := copyImageData(store, &img, copied.ID); err != nil {
				return err
			}
			// The copy keeps the layers in use as the original is deleted.
			if _, err := store.DeleteImage(img.ID, true); err != nil {
				return err
			}
			if err := store.AddNames(copied.ID, img.Names); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		w.t.Fatal(err)
	}
	if rekeyed == 0 {
		w.t.Fatal("the build cache names no image in the container store to hold under another ID")
	}
}

// cacheName returns the image ID that the build cache's repository names img by, if it names it.
func cacheName(img storage.Image) (string, bool) {
	for _, name := range img.Names {
		if id, ok := strings.CutPrefix(name, run.CacheRepository+":"); ok {
			return id, true
		}
	}
	return "", false
}

// copyImageData copies img's data, its manifests and config, to the image with store ID to in store.
func copyImageData(store storage.Store, img *storage.Image, to string) error {
	for _, key := range img.BigDataNames {
		data, err := store.ImageBigData(img.ID, key)
		if err != nil {
			return err
		}
		if err := store.SetImageBigData(to, key, data, imagemanifest.Digest); err != nil {
			return err
		}
	}
	return nil
}

// GivenAnotherVamosIsUsingTheBuildCache holds the scenario's build cache open, as another vamos that builds does,
// until the scenario ends or GivenTheOtherVamosIsDone.
func (w *world) GivenAnotherVamosIsUsingTheBuildCache() {
	w.t.Helper()
	w.noOther()
	d, err := cache.OpenDirectory(w.cacheDir())
	if err != nil {
		w.t.Fatal(err)
	}
	w.holdOther(func() { d.Close() })
}

// GivenAnotherVamosIsPruningTheBuildCache holds the scenario's build cache exclusively, as another vamos that prunes
// does, until the scenario ends or GivenTheOtherVamosIsDone.
func (w *world) GivenAnotherVamosIsPruningTheBuildCache() {
	w.t.Helper()
	w.noOther()
	d, err := cache.OpenDirectory(w.cacheDir())
	if err != nil {
		w.t.Fatal(err)
	}
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- cache.HoldExclusive(d, func() {
			close(held)
			<-release
		})
	}()
	select {
	case <-held:
	case err := <-done:
		d.Close()
		w.t.Fatalf("holding the build cache as a prune does: %v", err)
	}
	w.holdOther(func() {
		close(release)
		if err := <-done; err != nil {
			w.t.Errorf("the other vamos's prune: %v", err)
		}
		d.Close()
	})
}

// noOther fails the scenario if another vamos holds the build cache already, before a step takes it for another.
func (w *world) noOther() {
	w.t.Helper()
	if w.other != nil {
		w.t.Fatal("another vamos holds the build cache already")
	}
}

// holdOther records how to release what another vamos holds, once the scenario ends or GivenTheOtherVamosIsDone.
func (w *world) holdOther(release func()) {
	w.t.Helper()
	var once sync.Once
	w.other = func() { once.Do(release) }
	w.t.Cleanup(w.other)
}

// GivenTheOtherVamosIsDone releases what the other vamos held.
func (w *world) GivenTheOtherVamosIsDone() {
	w.t.Helper()
	if w.other == nil {
		w.t.Fatal("no other vamos holds the build cache")
	}
	w.other()
	w.other = nil
}

// GivenACachedLayerIsAlsoNamed gives an image that the build cache names, which no other name names, the name name
// too, as `buildah tag` can: a layer's, since a task keeps files rather than an image, and a pulled image is named by
// its digest too.
func (w *world) GivenACachedLayerIsAlsoNamed(name string) {
	w.t.Helper()
	w.requireOwnStores()
	err := w.stores.WithStore(func(store storage.Store) error {
		images, err := store.Images()
		if err != nil {
			return err
		}
		for _, img := range images {
			if _, named := cacheName(img); named && len(img.Names) == 1 {
				if w.alsoNamed == nil {
					w.alsoNamed = map[string]string{}
				}
				w.alsoNamed[name] = img.ID
				return store.AddNames(img.ID, []string{name})
			}
		}
		return fmt.Errorf("the build cache names no layer's image alone")
	})
	if err != nil {
		w.t.Fatal(err)
	}
}

// ThenTheContainerStoreNamesAnImageOnly checks that the container store has an image named name, and named nothing
// else, as `podman images` would show it: the image that GivenACachedLayerIsAlsoNamed gave the name, if it did.
func (w *world) ThenTheContainerStoreNamesAnImageOnly(name string) {
	w.t.Helper()
	err := w.stores.WithStore(func(store storage.Store) error {
		img, err := store.Image(name)
		if err != nil {
			return err
		}
		if id, ok := w.alsoNamed[name]; ok && img.ID != id {
			return fmt.Errorf("%s names image %s, want %s, which a Given step gave the name",
				name, img.ID[:12], id[:12])
		}
		if !slices.Equal(img.Names, []string{name}) {
			return fmt.Errorf("image %s is named %q, want only %s", img.ID[:12], img.Names, name)
		}
		return nil
	})
	w.check(err)
}

// ThenTheBuildCacheHoldsNothing checks that the build cache holds no file, and that the container store has no image
// that the build cache names: nothing that a prune would remove.
func (w *world) ThenTheBuildCacheHoldsNothing() {
	w.t.Helper()
	w.requireOwnStores()
	if held := w.cached(); len(held.blobs) > 0 || len(held.records) > 0 || len(held.images) > 0 {
		w.t.Errorf("the build cache holds %d files and %d results, and names %d images in the container store, "+
			"want none", len(held.blobs), len(held.records), len(held.images))
	}
}

// ThenTheStoresHoldWhatTheyDidBefore checks that the last command left the scenario's stores as it found them:
// every file and result of the build cache, and every image, with its names, and no more.
func (w *world) ThenTheStoresHoldWhatTheyDidBefore() {
	w.t.Helper()
	w.requireOwnStores()
	if !w.ran {
		w.t.Fatal("no command has run")
	}
	if before, after := w.cachedBefore, w.cachedAfter; !before.same(after) {
		w.t.Errorf("the stores held %v before the command, and %v after, want them as they were", before, after)
	}
}

// GivenTheSuitesStoresHoldAResult builds a layer of its own in the suite's stores, which other scenarios use,
// and notes what they hold, for ThenTheSuitesStoresHoldWhatTheyDid.
func (w *world) GivenTheSuitesStoresHoldAResult() {
	w.t.Helper()
	suite := testenv.SuiteStores(w.t)
	fsys := testenv.Project(w.t, files{
		"BUILD": dedent(`
			glob(name="marks", include=["mark.txt"])
			layer(name="mark", base="scratch", steps=[add("marks", "/")])
		`),
		"mark.txt": testenv.NewNonce(),
	})
	cmd := cli.NewRootCommand(cli.Config{Fs: fsys, Store: &suite.Options})
	cmd.SetArgs([]string{"-C", testenv.ProjectDir, "build", "mark"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.ExecuteContext(w.t.Context()); err != nil {
		w.t.Fatalf("building in the suite's stores: %v\n%s", err, out.String())
	}
	w.suiteBefore = w.cachedIn(suite)
	if len(w.suiteBefore.blobs) == 0 || len(w.suiteBefore.records) == 0 || len(w.suiteBefore.images) == 0 {
		w.t.Fatalf("the suite's stores hold %v after building there, want some of each", w.suiteBefore)
	}
}

// ThenTheSuitesStoresHoldWhatTheyDid checks that the suite's stores, which other scenarios use, still hold what they
// did when GivenTheSuitesStoresHoldAResult built there: every file and result of the build cache, and every image,
// with its names, whatever else they have gained since.
func (w *world) ThenTheSuitesStoresHoldWhatTheyDid() {
	w.t.Helper()
	if len(w.suiteBefore.records) == 0 {
		w.t.Fatal("no Given step built in the suite's stores")
	}
	if now := w.cachedIn(testenv.SuiteStores(w.t)); !now.holds(w.suiteBefore) {
		w.t.Errorf("the suite's stores held %v, and now %v, want them as they were", w.suiteBefore, now)
	}
}
