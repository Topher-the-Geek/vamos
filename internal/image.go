package internal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/docker/distribution/registry/api/errcode"
	v2 "github.com/docker/distribution/registry/api/v2"
	"github.com/opencontainers/go-digest"
	"github.com/spf13/afero"
	"go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/pkg/parse"
	"go.podman.io/common/pkg/config"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/signature"
	is "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// buildOptions describes one build: of a target's image, sent to dest, or else of targets, kept in the cache.
type buildOptions struct {
	workspace *workspace
	targets   []string                             // labels of the targets to build: if dest is set, the one to send
	dest      func() (types.ImageReference, error) // called once the image is ready to commit
	action    string                               // describes sending the image, e.g. "pushing to app:latest"

	rebuild   bool      // run targets again, rather than reuse their cached results
	timestamp time.Time // if set, the earliest time that targets which run stamp their files with

	systemContext  *types.SystemContext
	stdout, stderr io.Writer // receive the output of run() steps
}

// buildImage runs o.targets, and the targets they use, with buildah. If o.dest is set, it then copies
// the image of the one target to o.dest, returning its image ID there; otherwise it returns "". Each
// target's result is kept in the build cache, and reused instead of running the target again whenever
// a later build gives it the same inputs, unless it is a target to rebuild.
func buildImage(ctx context.Context, o buildOptions) (string, error) {
	needed := o.workspace.needs(o.targets)
	if err := o.workspace.checkSources(needed); err != nil {
		return "", err
	}
	rebuild := map[string]bool{}
	for _, l := range o.targets {
		rebuild[l] = o.rebuild
	}

	buildStoreOptions, err := storage.DefaultStoreOptions()
	if err != nil {
		return "", err
	}
	buildStore, err := storage.GetStore(buildStoreOptions)
	if err != nil {
		return "", err
	}
	defer buildStore.Shutdown(false)
	buildCache, err := openCache(cacheDir(buildStoreOptions))
	if err != nil {
		return "", err
	}

	// Pull everything first, so a bad image name is reported before any slow target runs.
	images := map[string]builtImage{
		scratch: {id: scratch, epoch: time.Unix(0, 0)}, // buildah starts an empty image from this name
	}
	for _, p := range o.workspace.pulls {
		if !needed[p.label] {
			continue
		}
		name := o.workspace.display(p.label)
		policy := buildah.PullIfMissing
		if rebuild[p.label] {
			// The tag may name a newer image now.
			fmt.Fprintf(o.stdout, "pull %q: pulling again\n", name)
			policy = buildah.PullAlways
		}
		if images[p.label], err = pullImage(ctx, buildStore, p, policy, o.systemContext); err != nil {
			return "", fmt.Errorf("pull %q: %w", name, err)
		}
	}

	conf, err := config.Default()
	if err != nil {
		return "", err
	}
	capabilities, err := conf.Capabilities("root", nil, nil)
	if err != nil {
		return "", err
	}
	isolation, err := parse.IsolationOption("")
	if err != nil {
		return "", err
	}

	r := targetRunner{
		buildOptions: o,
		store:        buildStore,
		cache:        buildCache,
		capabilities: capabilities,
		isolation:    isolation,
		images:       images,
		trees:        map[*fileSet]setTree{},
	}
	for _, s := range o.workspace.targets {
		if !needed[s.label] {
			continue
		}
		if err := r.runCached(ctx, s, rebuild[s.label]); err != nil {
			return "", err
		}
	}
	if o.dest == nil {
		return "", nil
	}
	return r.send(ctx, r.images[o.targets[0]].id)
}

// cacheDir returns where the build cache is kept: beside the container store, so that a volume
// holding /var/lib/containers keeps both.
func cacheDir(options storage.StoreOptions) string {
	return filepath.Join(filepath.Dir(options.GraphRoot), "vamos")
}

// cacheRepository names the images of cached layers in the container store, as
// cacheRepository:<image ID>, which keeps tools that remove unnamed images from removing them.
const cacheRepository = "localhost/vamos-cache"

// builtImage is the image of a pull or a layer, once it is in the container store.
type builtImage struct {
	id    string    // its ID in the container store, or scratch
	epoch time.Time // when it was made; files that targets add to it are stamped with this, or a later epoch
}

// setTree is the files of a glob or task, once they are in the build cache.
type setTree struct {
	tree  blob      // the files, as a tree
	epoch time.Time // as for builtImage; zero for a glob
}

// pullImage fetches p's image into store, as policy says: unless it is already there, or always.
func pullImage(
	ctx context.Context, store storage.Store, p *pullSpec, policy define.PullPolicy,
	systemContext *types.SystemContext,
) (builtImage, error) {
	image := p.image.String()
	id, err := buildah.Pull(ctx, image, buildah.PullOptions{
		Store:         store,
		SystemContext: systemContext,
		PullPolicy:    policy,
	})
	if err != nil {
		if imageUnavailable(err) {
			return builtImage{}, fmt.Errorf("image %q: %w (check the name, and log in if the image is private)",
				image, err)
		}
		return builtImage{}, fmt.Errorf("image %q: %w", image, err)
	}
	// buildah.Pull can't be cancelled, so check for an interrupt after it.
	if err := ctx.Err(); err != nil {
		return builtImage{}, err
	}
	img, err := store.Image(id)
	if err != nil {
		return builtImage{}, fmt.Errorf("image %q: %w", image, err)
	}
	// Files built on the image are stamped with its creation time: a time that is the same on
	// every machine, and that moves forward only when the image does.
	epoch := img.Created
	if epoch.IsZero() {
		epoch = time.Unix(0, 0)
	}
	return builtImage{id: id, epoch: epoch}, nil
}

// targetRunner runs the layers and tasks of a build.
type targetRunner struct {
	buildOptions
	store        storage.Store
	cache        *cache
	capabilities []string // given to run() steps, as root
	isolation    define.Isolation
	images       map[string]builtImage // the images of pulls and layers, by label, once they are ready
	trees        map[*fileSet]setTree  // the files of globs and tasks, once they are ready
}

// describe names target s for messages, e.g. `layer "app"` or `task "//ui:build"`.
func (r *targetRunner) describe(s *targetSpec) string {
	if s.task {
		return fmt.Sprintf("task %q", r.workspace.display(s.label))
	}
	return fmt.Sprintf("layer %q", r.workspace.display(s.label))
}

// runCached makes the result of target s ready for later targets, reusing the result the cache
// holds for its inputs or else running it and saving the result to the cache. It reports which
// it did, and replays a reused target's output, so a build prints what the target printed. With
// rebuild, it runs s even if its result is cached, and replaces that result.
func (r *targetRunner) runCached(ctx context.Context, s *targetSpec, rebuild bool) error {
	target := r.describe(s)
	a, err := r.inputs(s)
	if err != nil {
		return fmt.Errorf("%s: %w", target, err)
	}
	key := a.key()
	if rebuild {
		fmt.Fprintf(r.stdout, "%s: rebuilding\n", target)
	} else {
		result, err := r.cache.result(key)
		switch {
		case err != nil:
			return fmt.Errorf("%s: reading the build cache: %w", target, err)
		case result == nil:
			fmt.Fprintf(r.stdout, "%s: building\n", target)
		default:
			problem := r.checkResult(s, result)
			if problem == nil {
				fmt.Fprintf(r.stdout, "%s: cached\n", target)
				if err := r.replay(result); err != nil {
					return fmt.Errorf("%s: replaying its output from the build cache: %w", target, err)
				}
				r.record(s, result)
				return nil
			}
			fmt.Fprintf(r.stdout, "%s: building, since the cached result can't be used: %v\n", target, problem)
		}
	}

	var result *actionResult

	if s.task {
		result, err = r.runTarget(ctx, s, a, func(st *targetState, result *actionResult) error {
			if s.output == nil {
				return nil
			}
			output, err := collectOutput(st.mountPoint, s, r.cache)
			result.Output = &output
			return err
		})
	} else {
		result, err = r.runTarget(ctx, s, a, func(st *targetState, result *actionResult) error {
			return r.commitLayer(ctx, st.builder, result)
		})
	}
	if err != nil {
		return err
	}
	if err := r.cache.setResult(key, result); err != nil {
		return fmt.Errorf("%s: saving to the build cache: %w", target, err)
	}
	r.record(s, result)
	return nil
}

// inputs returns the action of target s: its inputs, which must be ready. The files it adds are stored
// in the cache, so that the target is built from the files its key describes.
func (r *targetRunner) inputs(s *targetSpec) (*action, error) {
	a := &action{
		Version: actionVersion,
		Kind:    "layer",
		Base:    r.images[s.base].id,
		Command: s.command,
	}
	if s.task {
		a.Kind = "task"
	}
	if s.output != nil {
		a.Source = s.source
		a.Include = sourcePaths([]*fileSet{s.output})
	}
	for _, in := range s.steps {
		entry := in.entry()
		if set := in.source(); set != nil {
			b, err := r.setTree(set)
			if err != nil {
				return nil, err
			}
			entry.Files = &b
		}
		a.Steps = append(a.Steps, entry)
	}
	return a, nil
}

// setTree returns the tree of set's files, stored in the cache. A task's files are ready once it has run.
// A glob's are stored as they are first read, so that a file changed later, even during the build,
// can't make a target's files differ from what its key says, and each target that adds them gets the same.
func (r *targetRunner) setTree(set *fileSet) (blob, error) {
	if t, ok := r.trees[set]; ok {
		return t.tree, nil
	}
	b, err := r.cache.putTree(r.workspace.fs, set.dir, sourcePaths([]*fileSet{set}))
	if err != nil {
		return blob{}, fmt.Errorf("reading %s: %w", r.workspace.display(set.label), err)
	}
	r.trees[set] = setTree{tree: b}
	return b, nil
}

// checkResult reports why a cached result for s can't be used, if it can't: something it
// names may have been removed since, from the cache or the container store.
func (r *targetRunner) checkResult(s *targetSpec, result *actionResult) error {
	if s.output != nil {
		if result.Output == nil {
			return errors.New("it has no files")
		}
		if err := r.cache.checkTree(*result.Output); err != nil {
			return fmt.Errorf("its files are missing from the cache: %w", err)
		}
	} else if !s.task {
		if result.Image == "" {
			return errors.New("it has no image")
		}
		if _, err := r.store.Image(result.Image.Encoded()); err != nil {
			return fmt.Errorf("its image %s is no longer in the container store", result.Image.Encoded()[:12])
		}
	}
	if result.Log != nil {
		if _, err := os.Stat(r.cache.blobPath(result.Log.Digest)); err != nil {
			return fmt.Errorf("its output is missing from the cache: %w", err)
		}
	}
	return nil
}

// replay writes what a cached target printed to stdout and stderr.
func (r *targetRunner) replay(result *actionResult) error {
	if result.Log == nil {
		return nil
	}
	log, err := r.cache.open(*result.Log)
	if err != nil {
		return err
	}
	defer log.Close()
	return replayLog(log, r.stdout, r.stderr)
}

// record makes the result of target s available to later targets.
func (r *targetRunner) record(s *targetSpec, result *actionResult) {
	epoch := time.Unix(result.Epoch, 0)
	if s.task {
		if s.output != nil {
			r.trees[s.output] = setTree{tree: *result.Output, epoch: epoch}
		}
	} else {
		r.images[s.label] = builtImage{id: result.Image.Encoded(), epoch: epoch}
	}
}

// epoch returns the time to stamp the files of s's result with when it runs: the latest of the
// epochs of its base and of the tasks whose files it adds, and of r.timestamp.
func (r *targetRunner) epoch(s *targetSpec) time.Time {
	epoch := r.images[s.base].epoch
	if r.timestamp.After(epoch) {
		epoch = r.timestamp
	}
	for _, set := range s.sets() {
		if set.task != nil {
			if e := r.trees[set].epoch; e.After(epoch) {
				epoch = e
			}
		}
	}
	return epoch
}

// runTarget runs target s, whose action is a, and saves what it printed to the cache. It then calls
// finish with the target's container, still mounted, to complete its result: to commit a layer's
// image, or collect a task's files.
func (r *targetRunner) runTarget(
	ctx context.Context, s *targetSpec, a *action, finish func(*targetState, *actionResult) error,
) (*actionResult, error) {
	target := r.describe(s)
	log, err := r.cache.newLog()
	if err != nil {
		return nil, fmt.Errorf("%s: saving to the build cache: %w", target, err)
	}
	defer log.discard()
	st, err := r.start(ctx, s, a, log)
	if err != nil {
		return nil, err
	}
	defer st.delete()

	result := &actionResult{Epoch: r.epoch(s).Unix()}
	// Save the log first, so that a failure saving it can't leave an image no result names.
	if result.Log, err = log.save(r.cache); err != nil {
		return nil, fmt.Errorf("%s: saving to the build cache: %w", target, err)
	}
	if err := finish(st, result); err != nil {
		return nil, fmt.Errorf("%s: %w", target, err)
	}
	return result, nil
}

// commitLayer commits the image of a layer from builder to the container store.
func (r *targetRunner) commitLayer(ctx context.Context, builder *buildah.Builder, result *actionResult) error {
	epoch := time.Unix(result.Epoch, 0)
	// A nil reference commits to the store under a temporary name, which buildah then removes.
	id, _, _, err := builder.Commit(ctx, nil, buildah.CommitOptions{
		SystemContext: r.systemContext,
		// Stamp every file the layer adds, its creation and its history with the epoch, so that the
		// same files make the same layer whenever they are built and whatever times they had on disk.
		HistoryTimestamp: &epoch,
	})
	if err != nil {
		return fmt.Errorf("saving image: %w", err)
	}
	// The same files can make an image cached before, which already has its name and may be in use.
	img, err := r.store.Image(id)
	if err != nil {
		return fmt.Errorf("saving image: %w", err)
	}
	if err := r.store.AddNames(id, []string{cacheRepository + ":" + id}); err != nil {
		if len(img.Names) == 0 {
			r.store.DeleteImage(id, true)
		}
		return fmt.Errorf("naming image %s: %w", id, err)
	}
	result.Image = digest.NewDigestFromEncoded(digest.SHA256, id)
	return nil
}

// send copies the image with ID id from the container store to r.dest, returning the ID it has
// there: the digest of its config, as written there, which may differ from the store's if its
// manifest is converted, as docker-archive does.
func (r *targetRunner) send(ctx context.Context, id string) (string, error) {
	dest, err := r.dest()
	if err != nil {
		return "", fmt.Errorf("%s: %w", r.action, err)
	}
	src, err := is.Transport.NewStoreReference(r.store, nil, id)
	if err != nil {
		return "", fmt.Errorf("%s: %w", r.action, err)
	}
	// Follow the system's signature policy, as buildah does when committing, but trust local
	// storage, where the image was just built.
	policy, err := signature.DefaultPolicy(r.systemContext)
	if err != nil {
		return "", fmt.Errorf("%s: %w", r.action, err)
	}
	policy.Transports[is.Transport.Name()] = signature.PolicyTransportScopes{
		"": {signature.NewPRInsecureAcceptAnything()},
	}
	policyContext, err := signature.NewPolicyContext(policy)
	if err != nil {
		return "", fmt.Errorf("%s: %w", r.action, err)
	}
	defer policyContext.Destroy()

	written, err := imagecopy.Image(ctx, policyContext, dest, src, &imagecopy.Options{DestinationCtx: r.systemContext})
	if err != nil {
		return "", fmt.Errorf("%s: %w", r.action, err)
	}
	m, err := manifest.FromBlob(written, manifest.GuessMIMEType(written))
	if err != nil {
		return "", fmt.Errorf("%s: reading the manifest written: %w", r.action, err)
	}
	return m.ConfigInfo().Digest.Encoded(), nil
}

// start makes a container for target s from its base image, mounts it, and applies its steps,
// whose files are stored as a says, recording what they print in log. A layer's default command is
// set too. The caller must delete the returned target's container.
func (r *targetRunner) start(
	ctx context.Context, s *targetSpec, a *action, log *logRecorder,
) (_ *targetState, err error) {
	target := r.describe(s)
	builder, err := buildah.NewBuilder(ctx, r.store, buildah.BuilderOptions{
		FromImage:     r.images[s.base].id,
		PullPolicy:    buildah.PullNever, // only pull() reaches registries
		Capabilities:  r.capabilities,
		SystemContext: r.systemContext,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", target, err)
	}
	st := &targetState{targetRunner: r, builder: builder, log: log, buildEnv: map[string]string{}}
	defer func() {
		if err != nil {
			st.delete()
		}
	}()
	// Mount the container once for the whole target, rather than for each step that needs its
	// files. buildah's own Run and Add then only count another use of the mount.
	if st.mountPoint, err = builder.Mount(""); err != nil {
		return nil, fmt.Errorf("%s: %w", target, err)
	}

	for i, in := range s.steps {
		// builder.Run can't be cancelled, so check for an interrupt between steps.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := in.apply(st, a.Steps[i].Files); err != nil {
			return nil, fmt.Errorf("%s: %s failed: %w", target, in, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if !s.task && len(s.command) > 0 {
		builder.SetCmd(s.command)
	}
	return st, nil
}

// delete unmounts and removes the target's container.
func (st *targetState) delete() {
	if st.mountPoint != "" {
		st.builder.Unmount()
	}
	st.builder.Delete()
}

// pruneCache removes every cached result: the images of cached layers from the container store,
// other than names that other tools gave them, and the build cache. Pulled images are kept.
func pruneCache(w io.Writer) error {
	options, err := storage.DefaultStoreOptions()
	if err != nil {
		return err
	}
	store, err := storage.GetStore(options)
	if err != nil {
		return err
	}
	defer store.Shutdown(false)
	images, err := store.Images()
	if err != nil {
		return err
	}
	var errs []error
	removed := 0
	for _, img := range images {
		var cached, other []string
		for _, name := range img.Names {
			if strings.HasPrefix(name, cacheRepository+":") {
				cached = append(cached, name)
			} else {
				other = append(other, name)
			}
		}
		if len(cached) == 0 {
			continue
		}
		if len(other) > 0 {
			err = store.RemoveNames(img.ID, cached)
		} else {
			_, err = store.DeleteImage(img.ID, true)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("removing image %s: %w", img.ID[:12], err))
			continue
		}
		removed++
	}

	dir := cacheDir(options)
	results, _ := os.ReadDir(filepath.Join(dir, "ac"))
	blobs, _ := os.ReadDir(filepath.Join(dir, "cas", "sha256"))
	if err := os.RemoveAll(dir); err != nil {
		errs = append(errs, err)
	}
	fmt.Fprintf(w, "Removed %d images from the container store, and %d results and %d files from %s\n",
		removed, len(results), len(blobs), dir)
	return errors.Join(errs...)
}

// addSource copies the files of set, the tree stored as blob files, into the target's container,
// beneath dest.
func (st *targetState) addSource(set *fileSet, files blob, dest string) error {
	// buildah reads sources from disk, so check them out into a temporary folder: from the cache,
	// even the project's, since the project's files may have changed since they were stored.
	contextDir, err := os.MkdirTemp("", "vamos-context-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(contextDir)
	if err := st.cache.checkout(files, contextDir); err != nil {
		return fmt.Errorf("copying sources from the build cache: %w", err)
	}
	for _, src := range sourcePaths([]*fileSet{set}) {
		if err := addPath(st.builder, contextDir, src, dest); err != nil {
			return fmt.Errorf("adding %q from %s: %w", src, st.workspace.display(set.label), err)
		}
	}
	return nil
}

// addPath copies dir/src into builder's container, at destination/src. Only src and what it
// holds are copied: parent directories the image lacks are made, and those it has are left as
// they are, rather than replaced by the folders that hold src on the host.
func addPath(builder *buildah.Builder, dir, src, destination string) error {
	options := buildah.AddAndCopyOptions{ContextDir: dir, AllowWildcard: types.OptionalBoolFalse}
	if src != "." {
		// Copy a directory itself, with its mode, rather than only what it holds. Only the
		// whole of dir, which is a temporary folder, is copied by its contents.
		options.DirCopyContents = types.OptionalBoolFalse
	}
	// A trailing slash makes the parent a directory, keeping src's own name within it.
	parent := path.Join(destination, filepath.ToSlash(filepath.Dir(src)))
	if !strings.HasSuffix(parent, "/") {
		parent += "/"
	}
	return builder.Add(parent, false, options, filepath.Join(dir, src))
}

// collectOutput saves the files of task s from its container, mounted at mountPoint, to c,
// returning their tree.
func collectOutput(mountPoint string, s *targetSpec, c *cache) (blob, error) {
	// Resolve links in source as the container would, so they can't lead outside it.
	resolved, err := securejoin.SecureJoin(mountPoint, s.source)
	if err != nil {
		return blob{}, err
	}
	rel, err := filepath.Rel(mountPoint, resolved)
	if err != nil {
		return blob{}, err
	}
	source := filepath.Join("/", rel) // as the container sees it
	container := afero.NewBasePathFs(afero.NewOsFs(), mountPoint)
	info, err := container.Stat(source)
	if errors.Is(err, fs.ErrNotExist) {
		return blob{}, fmt.Errorf("source %q not found", s.source)
	} else if err != nil {
		return blob{}, err
	} else if !info.IsDir() {
		return blob{}, fmt.Errorf("source %q is not a directory", s.source)
	}

	paths := sourcePaths([]*fileSet{s.output})
	for _, p := range paths {
		// checkInclude refuses links on the way, which the host would resolve outside the container.
		if err := checkInclude(container, source, p); err != nil {
			return blob{}, err
		}
	}
	output, err := c.putTree(container, source, paths)
	if err != nil {
		return blob{}, fmt.Errorf("collecting output: %w", err)
	}
	return output, nil
}

// sourcePaths returns the paths in sets, sorted, leaving out any within another, since
// copying a directory copies everything in it.
func sourcePaths(sets []*fileSet) []string {
	var all []string
	for _, set := range sets {
		all = append(all, set.paths...)
	}
	// "." holds everything, though names such as "-x" sort before it.
	if slices.Contains(all, ".") {
		return []string{"."}
	}
	slices.Sort(all)
	var paths []string
	for _, p := range slices.Compact(all) {
		covered := slices.ContainsFunc(paths, func(dir string) bool { return within(p, dir) })
		if !covered {
			paths = append(paths, p)
		}
	}
	return paths
}

// imageUnavailable reports whether err says that a registry has no such image, or won't
// show it without credentials, as Docker Hub does for repositories that don't exist.
func imageUnavailable(err error) bool {
	var unauthorized docker.ErrUnauthorizedForCredentials
	if errors.As(err, &unauthorized) || errors.Is(err, storage.ErrImageUnknown) {
		return true
	}
	var registryErr errcode.Error
	if !errors.As(err, &registryErr) {
		return false
	}
	switch registryErr.Code {
	case errcode.ErrorCodeDenied, errcode.ErrorCodeUnauthorized, v2.ErrorCodeNameUnknown, v2.ErrorCodeManifestUnknown:
		return true
	}
	return false
}

// readDirNames returns the names of the entries in the directory dir in fsys.
func readDirNames(fsys afero.Fs, dir string) ([]string, error) {
	f, err := fsys.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}

// fileMode returns the permission and special bits of info's mode.
func fileMode(info fs.FileInfo) fs.FileMode {
	return info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
}
