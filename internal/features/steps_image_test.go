package features

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/containerd/platforms"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// Then steps about the image that the last command saved, its archive, and the store it was built in.

// ThenNoImageIsLeftBehind checks that the store has no unnamed image that it didn't have when the scenario began.
// It stops the scenario if its background doesn't build, and so noted no images.
func (w *world) ThenNoImageIsLeftBehind() {
	w.t.Helper()
	if w.before == nil {
		w.t.Fatal("the scenario's background doesn't build, so it noted no images to compare with")
	}
	after, err := w.stores.UnnamedImages()
	if err != nil {
		w.t.Fatal(err)
	}
	for id := range after {
		if !w.before[id] {
			w.t.Errorf("image %s left in the container store", id)
		}
	}
}

// scope is where in an image a listing looks: beneath a directory, or everywhere,
// since an image on a base such as busybox can't be listed whole, but a directory in it can.
type scope struct {
	dir   string
	whole bool // whether it is everywhere, rather than under the root by mistake
}

// under is the scope of the paths beneath dir, an absolute path in the image other than its root,
// which is everywhere.
func under(dir string) scope {
	return scope{dir: dir}
}

// everywhere is the scope of every path in the image, for an image on scratch.
var everywhere = scope{dir: "/", whole: true}

// scoped returns the image's entries within s.
// It stops the scenario if s isn't a clean absolute path short of the root, or any of paths, which a step expects,
// isn't within it, since either is a mistake in the scenario.
func (w *world) scoped(s scope, paths ...string) map[string]entry {
	w.t.Helper()
	if !s.whole && (!path.IsAbs(s.dir) || path.Clean(s.dir) != s.dir || s.dir == "/") {
		w.t.Fatalf("%q isn't a clean absolute path to look under, short of the root, which is everywhere", s.dir)
	}
	for _, p := range paths {
		if !isBeneath(p, s.dir) {
			w.t.Fatalf("%s isn't under %s", p, s.dir)
		}
	}
	return beneath(w.imageEntries(), s.dir)
}

// ThenTheImageHolds checks that the regular files within s are exactly want, by path, and that every directory
// within s holds one of them: a symbolic link, another file or an empty directory there fails it.
// A scope that holds links is listed with ThenTheImagePathsAre instead, its links checked by ThenTheImageLinks
// and its files by ThenTheImageHoldsAtLeast.
// Like every step about the image, it stops the scenario if no image was saved, or it can't be read,
// and like every step that takes a scope, if the scope or a path it expects is a mistake, as scoped says.
func (w *world) ThenTheImageHolds(s scope, want files) {
	w.t.Helper()
	w.check(matchFiles(w.scoped(s, slices.Collect(maps.Keys(want))...), want, true))
}

// ThenTheImageHoldsAtLeast checks that the image holds want, by path, and allows other files anywhere:
// for files where a scenario can't say all that their directory holds, such as one added to a base's /etc.
func (w *world) ThenTheImageHoldsAtLeast(want files) {
	w.t.Helper()
	w.check(matchFiles(w.scoped(everywhere, slices.Collect(maps.Keys(want))...), want, false))
}

// ThenTheImagePathsAre checks that the paths within s, directories and links among them, are exactly want.
func (w *world) ThenTheImagePathsAre(s scope, want ...string) {
	w.t.Helper()
	w.check(matchPaths(w.scoped(s, want...), want))
}

// ThenTheImageHoldsNothing checks that there is nothing within s: no file, directory or link,
// whether or not its directory is there, which ThenTheImageHasTheDirectory checks.
func (w *world) ThenTheImageHoldsNothing(s scope) {
	w.t.Helper()
	w.check(matchPaths(w.scoped(s), nil))
}

// ThenTheImageDoesNotContain checks that none of values is in the image's configuration or any of its layers,
// even in a file that a later layer removes.
func (w *world) ThenTheImageDoesNotContain(values ...string) {
	w.t.Helper()
	if w.image == nil {
		w.t.Fatal("no image was saved")
	}
	config, err := w.image.RawConfigFile()
	if err != nil {
		w.t.Fatal(err)
	}
	layers, err := w.image.Layers()
	if err != nil {
		w.t.Fatal(err)
	}
	for _, value := range values {
		if bytes.Contains(config, []byte(value)) {
			w.t.Errorf("the image's configuration holds %q", value)
		}
	}
	for i, l := range layers {
		rc, err := l.Uncompressed()
		if err != nil {
			w.t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			w.t.Fatal(err)
		}
		for _, value := range values {
			if bytes.Contains(b, []byte(value)) {
				w.t.Errorf("the image's layer %d holds %q", i, value)
			}
		}
	}
}

// ThenTheImageDoesNotHold checks that the image's filesystem has no file, directory or link at any of paths,
// and checks no others.
func (w *world) ThenTheImageDoesNotHold(paths ...string) {
	w.t.Helper()
	entries := w.imageEntries()
	for _, path := range paths {
		if _, ok := entries[path]; ok {
			w.t.Errorf("the image holds %s, want it not to", path)
		}
	}
}

// ThenTheImageHasTheDirectory checks that the image has a directory at path, and not what it holds.
func (w *world) ThenTheImageHasTheDirectory(path string) {
	w.t.Helper()
	if e, ok := w.imageEntries()[path]; !ok || !e.dir {
		w.t.Errorf("no directory %s in the image", path)
	}
}

// ThenTheImageIsTheOneSavedFirst checks that the last image saved is the scenario's first, by its ID.
// It stops the scenario if the last command saved no image.
func (w *world) ThenTheImageIsTheOneSavedFirst() {
	w.t.Helper()
	if first, last := w.imageID(w.firstSaved()), w.imageID(w.image); first != last {
		w.t.Errorf("saved image %s, want the first saved, %s", last, first)
	}
}

// ThenTheImageFileDiffersFromTheOneSavedFirst checks that the file at path differs from that in the first image saved,
// and that both have it, and checks no other file.
func (w *world) ThenTheImageFileDiffersFromTheOneSavedFirst(path string) {
	w.t.Helper()
	first, err := readImage(w.firstSaved())
	if err != nil {
		w.t.Fatalf("reading the first image saved: %v", err)
	}
	if was, ok := first[path]; !ok {
		w.t.Errorf("%s not found in the first image saved", path)
	} else if e, ok := w.imageEntries()[path]; !ok {
		w.t.Errorf("%s not found", path)
	} else if e.content == was.content {
		w.t.Errorf("%s = %q, as in the first image saved; want it to differ", path, e.content)
	}
}

// ThenTheImageWasCreatedWhenTheOneSavedFirstWas checks that the last image's creation time is the first's,
// and that the first's is a real time, not the zero of an unstamped image.
func (w *world) ThenTheImageWasCreatedWhenTheOneSavedFirstWas() {
	w.t.Helper()
	created := w.config(w.firstSaved()).Created.Time
	if created.Year() < 2000 {
		w.t.Errorf("the first image saved was created at %v, want a real time to compare with", created)
	}
	w.ThenTheImageWasCreatedAt(created)
}

// ThenTheImageWasCreatedAt checks the image's creation time.
func (w *world) ThenTheImageWasCreatedAt(want time.Time) {
	w.t.Helper()
	if got := w.imageConfig().Created.Time; !got.Equal(want) {
		w.t.Errorf("image created at %v, want %v", got, want)
	}
}

// ThenTheImageFilesWereModifiedAt checks the modification times of the image's files at paths, and checks no others.
func (w *world) ThenTheImageFilesWereModifiedAt(want time.Time, paths ...string) {
	w.t.Helper()
	w.check(matchModTimes(w.imageEntries(), want, paths))
}

// ThenTheImageFilesWereModifiedWhenItWasCreated checks that the image's files at paths have its creation time,
// and checks no others.
func (w *world) ThenTheImageFilesWereModifiedWhenItWasCreated(paths ...string) {
	w.t.Helper()
	w.ThenTheImageFilesWereModifiedAt(w.imageConfig().Created.Time, paths...)
}

// firstSaved returns the first image that the scenario saved.
func (w *world) firstSaved() v1.Image {
	w.t.Helper()
	if w.first == nil || w.image == nil {
		w.t.Fatal("the last command saved no image")
	}
	return w.first
}

func (w *world) imageID(img v1.Image) string {
	w.t.Helper()
	id, err := img.ConfigName()
	if err != nil {
		w.t.Fatal(err)
	}
	return id.Hex
}

// ThenTheImageLinks checks that the image holds these symbolic links, by path, to their targets,
// and checks no other paths: it is not a listing, so it allows other links.
func (w *world) ThenTheImageLinks(want links) {
	w.t.Helper()
	w.check(matchLinks(w.imageEntries(), want, false))
}

// ThenTheImageOwnersAre checks the owners of the image's files and directories at want's paths, and checks no others.
func (w *world) ThenTheImageOwnersAre(want owners) {
	w.t.Helper()
	w.check(matchOwners(w.imageEntries(), want))
}

// ThenTheImageModesAre checks the modes of the image's files and directories at want's paths, and checks no others.
func (w *world) ThenTheImageModesAre(want modes) {
	w.t.Helper()
	w.check(matchModes(w.imageEntries(), want))
}

// ThenTheImageCommandIs checks the image's command; with no arguments, that it has none.
func (w *world) ThenTheImageCommandIs(want ...string) {
	w.t.Helper()
	w.checkArgv("command", w.imageConfig().Config.Cmd, want)
}

// ThenTheImageEntrypointIs checks the image's entrypoint; with no arguments, that it has none.
func (w *world) ThenTheImageEntrypointIs(want ...string) {
	w.t.Helper()
	w.checkArgv("entrypoint", w.imageConfig().Config.Entrypoint, want)
}

func (w *world) checkArgv(field string, got, want []string) {
	w.t.Helper()
	if !slices.Equal(got, want) {
		w.t.Errorf("%s = %q, want %q", field, got, want)
	}
}

// ThenTheImageIsForThisMachine checks that the image's platform is the one that the tests run on, variant and all.
func (w *world) ThenTheImageIsForThisMachine() {
	w.t.Helper()
	w.ThenTheImageIsFor(thisPlatform())
}

// thisPlatform returns the platform that the tests run on, as vamos names it.
func thisPlatform() string {
	return platforms.Format(platforms.Normalize(platforms.DefaultSpec()))
}

// anotherPlatform returns a platform that isn't this machine's, which a target can be built for and saved
// without running its programs.
func anotherPlatform() string {
	if runtime.GOARCH == "amd64" {
		return "linux/arm64"
	}
	return "linux/amd64"
}

// ThenTheImageIsFor checks that the image's platform is want, such as linux/arm64.
func (w *world) ThenTheImageIsFor(want string) {
	w.t.Helper()
	config := w.imageConfig()
	if got := platformAsWanted(config.OS, config.Architecture, config.Variant, want); got != want {
		w.t.Errorf("the image is for %s, want %s", got, want)
	}
}

// environment is the values of environment variables, by name.
type environment map[string]string

// ThenTheImageEnvironmentSets checks that the image's configuration sets each variable of want to its value,
// and checks no other variable. Its failure lists every variable, with its value, that the image doesn't set so.
func (w *world) ThenTheImageEnvironmentSets(want environment) {
	w.t.Helper()
	if len(want) == 0 {
		w.t.Fatal("no variables to expect")
	}
	env := w.imageConfig().Config.Env
	var missing []string
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if v := name + "=" + want[name]; !slices.Contains(env, v) {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		w.t.Errorf("env = %q, want it to include %q", env, missing)
	}
}

// ThenTheImageEnvironmentDoesNotSet checks that the image's configuration doesn't set the variable name.
func (w *world) ThenTheImageEnvironmentDoesNotSet(name string) {
	w.t.Helper()
	env := w.imageConfig().Config.Env
	if slices.ContainsFunc(env, func(v string) bool { return strings.HasPrefix(v, name+"=") }) {
		w.t.Errorf("env = %q, want no %s", env, name)
	}
}

// ThenTheImageWorkingDirectoryIs checks the image's working directory.
func (w *world) ThenTheImageWorkingDirectoryIs(want string) {
	w.t.Helper()
	if got := w.imageConfig().Config.WorkingDir; got != want {
		w.t.Errorf("working directory = %q, want %q", got, want)
	}
}

// ThenTheArchiveNames checks that the archive saved names its image exactly as tags, and with no tags,
// that it names none. It stops the scenario if no archive was saved, or its manifest can't be read.
func (w *world) ThenTheArchiveNames(tags ...string) {
	w.t.Helper()
	if w.archive == "" {
		w.t.Fatal("no archive was saved")
	}
	manifest, err := tarball.LoadManifest(func() (io.ReadCloser, error) { return os.Open(w.archive) })
	if err != nil {
		w.t.Fatalf("reading the archive's manifest: %v", err)
	}
	var got []string
	for _, descriptor := range manifest {
		got = append(got, descriptor.RepoTags...)
	}
	if !slices.Equal(got, tags) {
		w.t.Errorf("the archive names %q, want %q", got, tags)
	}
}

// imageEntries returns the entries of the saved image's flattened filesystem, by absolute path.
// It reads the image once, for all the steps that check it.
func (w *world) imageEntries() map[string]entry {
	w.t.Helper()
	if w.image == nil {
		w.t.Fatal("no image was saved")
	}
	if w.entries != nil {
		return w.entries
	}
	entries, err := readImage(w.image)
	if err != nil {
		w.t.Fatalf("reading the image: %v", err)
	}
	w.entries = entries
	return entries
}

// readImage returns the entries of img's filesystem, by absolute path, without its root.
// It applies the layers in order, as a container's filesystem stacks them, so that a hard link,
// as busybox's applets are, takes its file as it is when the link's layer applies, whatever replaces it later.
func readImage(img v1.Image) (map[string]entry, error) {
	layers, err := img.Layers()
	if err != nil {
		return nil, err
	}
	entries := map[string]entry{}
	for _, l := range layers {
		if err := applyLayer(entries, l); err != nil {
			return nil, err
		}
	}
	delete(entries, "/")
	return entries, nil
}

// applyLayer applies a layer's entries and whiteouts to entries, those of the layers below it.
func applyLayer(entries map[string]entry, l v1.Layer) error {
	rc, err := l.Uncompressed()
	if err != nil {
		return err
	}
	defer rc.Close()
	added := map[string]bool{} // the paths this layer holds, which its whiteouts keep, hiding only those below
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		name := testenv.ImagePath(hdr.Name)
		dir, base := path.Split(name)
		if base == ".wh..wh..opq" {
			removeBeneath(entries, dir, func(p string) bool { return !added[p] })
			continue
		}
		if hidden, ok := strings.CutPrefix(base, ".wh."); ok {
			hidden = path.Join(dir, hidden)
			if !added[hidden] {
				delete(entries, hidden)
			}
			removeBeneath(entries, hidden, func(p string) bool { return !added[p] })
			continue
		}
		e := entry{
			dir:     hdr.Typeflag == tar.TypeDir,
			regular: hdr.Typeflag == tar.TypeReg,
			symlink: hdr.Typeflag == tar.TypeSymlink,
			mode:    hdr.Mode & 0o7777,
			modTime: hdr.ModTime,
			owner:   fmt.Sprintf("%d:%d", hdr.Uid, hdr.Gid),
		}
		switch hdr.Typeflag {
		case tar.TypeSymlink:
			e.target = hdr.Linkname
		case tar.TypeReg:
			b, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			e.content = string(b)
		case tar.TypeLink:
			file, ok := entries[testenv.ImagePath(hdr.Linkname)]
			if !ok || !file.regular {
				return fmt.Errorf("hard link %s: its target, %s, isn't a regular file before it", name, hdr.Linkname)
			}
			e = file
		}
		if !e.dir {
			removeBeneath(entries, name, func(string) bool { return true }) // a directory it replaces
		}
		entries[name] = e
		added[name] = true
	}
}

// removeBeneath removes from entries those beneath dir for which remove is true.
func removeBeneath(entries map[string]entry, dir string, remove func(string) bool) {
	for p := range beneath(entries, dir) {
		if remove(p) {
			delete(entries, p)
		}
	}
}

func (w *world) imageConfig() *v1.ConfigFile {
	w.t.Helper()
	if w.image == nil {
		w.t.Fatal("no image was saved")
	}
	return w.config(w.image)
}

func (w *world) config(img v1.Image) *v1.ConfigFile {
	w.t.Helper()
	config, err := img.ConfigFile()
	if err != nil {
		w.t.Fatal(err)
	}
	return config
}

// ThenNothingIsPushed checks that the registry has nothing at the image name that the last command pushed to.
func (w *world) ThenNothingIsPushed() {
	w.t.Helper()
	if w.pushedTo == "" {
		w.t.Fatal("no command has run that pushes to an image name")
	}
	ref, err := name.ParseReference(w.pushedTo, name.Insecure)
	if err != nil {
		w.t.Fatal(err)
	}
	desc, err := remote.Head(ref, w.readingTheRegistry()...)
	var terr *transport.Error
	switch {
	case err == nil:
		w.t.Errorf("the registry has %s at %s, want nothing", desc.Digest, w.pushedTo)
	case !errors.As(err, &terr) || terr.StatusCode != http.StatusNotFound:
		w.t.Fatalf("asking the registry for %s: %v, want it not found", w.pushedTo, err)
	}
}

// readingTheRegistry returns the options that read from the scenario's registry, logged in if it needs it.
func (w *world) readingTheRegistry() []remote.Option {
	if w.login == nil {
		return nil
	}
	return []remote.Option{remote.WithAuth(w.login)}
}

// platformName returns the platform of os, arch and variant as os/arch[/variant], normalized as this machine's is,
// so that linux/arm64/v8 is linux/arm64.
func platformName(os, arch, variant string) string {
	return platforms.Format(platforms.Normalize(imgspecv1.Platform{OS: os, Architecture: arch, Variant: variant}))
}

// platformAsWanted returns the platform of os, arch and variant as os/arch[/variant], to compare with want:
// as it is if want names a variant, so that a variant that is lost or wrong shows,
// and otherwise normalized, as this machine's is, so that linux/arm64/v8 is linux/arm64.
func platformAsWanted(os, arch, variant, want string) string {
	if strings.Count(want, "/") == 2 {
		return platforms.Format(imgspecv1.Platform{OS: os, Architecture: arch, Variant: variant})
	}
	return platformName(os, arch, variant)
}

// manifest is what to expect of one manifest of an index: the platform it is for, as os/arch[/variant],
// its image's command, and, if not nil, every regular file of its image, which is on scratch.
type manifest struct {
	platform string
	command  []string
	files    files
}

// ThenTheIndexManifestsAre checks that the index pushed lists exactly want, in order: each manifest for its
// platform, as both the index and the manifest's image say, with its command, and its files if want gives them.
// It stops the scenario if no index was pushed, or it can't be read.
func (w *world) ThenTheIndexManifestsAre(want ...manifest) {
	w.t.Helper()
	if w.index == nil {
		w.t.Fatal("no index was pushed")
	}
	listed, err := w.index.IndexManifest()
	if err != nil {
		w.t.Fatal(err)
	}
	var got, wantPlatforms []string
	for i, m := range listed.Manifests {
		if m.Platform == nil {
			w.t.Fatalf("the index lists %s for no platform", m.Digest)
		}
		var wanted string
		if i < len(want) {
			wanted = want[i].platform
		}
		got = append(got, platformAsWanted(m.Platform.OS, m.Platform.Architecture, m.Platform.Variant, wanted))
	}
	for _, m := range want {
		wantPlatforms = append(wantPlatforms, m.platform)
	}
	if !slices.Equal(got, wantPlatforms) {
		w.t.Errorf("the index lists manifests for %q, want %q, in order", got, wantPlatforms)
		return
	}
	var errs []error
	for i, m := range listed.Manifests {
		img, err := w.index.Image(m.Digest)
		if err != nil {
			w.t.Fatal(err)
		}
		config := w.config(img)
		got := platformAsWanted(config.OS, config.Architecture, config.Variant, want[i].platform)
		if got != want[i].platform {
			errs = append(errs, fmt.Errorf("%s: the image is for %s", want[i].platform, got))
		}
		if !slices.Equal(config.Config.Cmd, want[i].command) {
			errs = append(errs, fmt.Errorf("%s: command = %q, want %q", want[i].platform, config.Config.Cmd,
				want[i].command))
		}
		if want[i].files != nil {
			entries, err := readImage(img)
			if err != nil {
				w.t.Fatal(err)
			}
			if err := matchFiles(entries, want[i].files, true); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", want[i].platform, err))
			}
		}
	}
	w.check(errors.Join(errs...))
}
