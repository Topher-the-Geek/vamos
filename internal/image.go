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

// buildOptions describes one build: of the image, sent to dest, or else of targets, kept in the cache.
type buildOptions struct {
	fs     afero.Fs // holds dir and the build file's sources
	dir    string
	file   *buildFile
	dest   func() (types.ImageReference, error) // called once the image is ready to commit
	action string                               // describes sending the image, e.g. "pushing to app:latest"

	targets   []string  // steps to build when dest is nil, as `vamos build` does
	rebuild   bool      // run targets again, rather than reuse their cached results
	timestamp time.Time // if set, the earliest time that steps which run stamp their files with

	systemContext  *types.SystemContext
	stdout, stderr io.Writer // receive the output of run steps
}

// buildImage runs o.file's steps with buildah and copies the image of the last layer to o.dest,
// returning its image ID there. If o.dest is nil, it runs only o.targets and the steps they use,
// and returns "". Each step's result is kept in the build cache, and reused instead of running
// the step again whenever a later build gives it the same inputs, unless it is a target to rebuild.
func buildImage(ctx context.Context, o buildOptions) (string, error) {
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

	var needed map[string]bool // steps to run, if not all
	rebuild := map[string]bool{}
	if o.dest == nil {
		needed = o.file.needs(o.targets)
		for _, name := range o.targets {
			rebuild[name] = o.rebuild
		}
	}

	// Pull everything first, so a bad image name is reported before any slow step runs.
	images := map[string]builtImage{
		scratch: {id: scratch, epoch: time.Unix(0, 0)}, // buildah starts an empty image from this name
	}
	for _, p := range o.file.pulls {
		if needed != nil && !needed[p.name] {
			continue
		}
		policy := buildah.PullIfMissing
		if rebuild[p.name] {
			// The tag may name a newer image now.
			fmt.Fprintf(o.stdout, "pull %q: pulling again\n", p.name)
			policy = buildah.PullAlways
		}
		if images[p.name], err = pullImage(ctx, buildStore, p, policy, o.systemContext); err != nil {
			return "", fmt.Errorf("pull %q: %w", p.name, err)
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

	r := stepRunner{
		buildOptions: o,
		store:        buildStore,
		cache:        buildCache,
		capabilities: capabilities,
		isolation:    isolation,
		images:       images,
		outputs:      map[*fileSet]taskOutput{},
	}
	for _, s := range o.file.steps {
		if needed != nil && !needed[s.name] {
			continue
		}
		if err := r.runCached(ctx, s, rebuild[s.name]); err != nil {
			return "", err
		}
	}
	if o.dest == nil {
		return "", nil
	}
	// loadBuildFile makes sure every task is used by a later step, so the last step is the final layer.
	return r.send(ctx, r.images[o.file.steps[len(o.file.steps)-1].name].id)
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
	epoch time.Time // when it was made; files that steps add to it are stamped with this, or a later epoch
}

// taskOutput is the files a task made, once they are in the build cache.
type taskOutput struct {
	tree  blob      // the files, as a tree
	epoch time.Time // as for builtImage
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

// stepRunner runs the layers and tasks of a build.
type stepRunner struct {
	buildOptions
	store        storage.Store
	cache        *cache
	capabilities []string // given to run steps, as root
	isolation    define.Isolation
	images       map[string]builtImage   // the images of pulls and layers, by name, once they are ready
	outputs      map[*fileSet]taskOutput // tasks' files, once they are ready
}

// describeStep names step s for messages, e.g. `layer "app"`.
func describeStep(s *stepSpec) string {
	if s.output != nil {
		return fmt.Sprintf("task %q", s.name)
	}
	return fmt.Sprintf("layer %q", s.name)
}

// runCached makes the result of step s ready for later steps, reusing the result the cache
// holds for its inputs or else running it and saving the result to the cache. It reports which
// it did, and replays a reused step's output, so a build prints what the step printed. With
// rebuild, it runs s even if its result is cached, and replaces that result.
func (r *stepRunner) runCached(ctx context.Context, s *stepSpec, rebuild bool) error {
	step := describeStep(s)
	a, err := r.inputs(s)
	if err != nil {
		return fmt.Errorf("%s: %w", step, err)
	}
	key := a.key()
	if rebuild {
		fmt.Fprintf(r.stdout, "%s: rebuilding\n", step)
	} else {
		result, err := r.cache.result(key)
		switch {
		case err != nil:
			return fmt.Errorf("%s: reading the build cache: %w", step, err)
		case result == nil:
			fmt.Fprintf(r.stdout, "%s: building\n", step)
		default:
			problem := r.checkResult(s, result)
			if problem == nil {
				fmt.Fprintf(r.stdout, "%s: cached\n", step)
				if err := r.replay(result); err != nil {
					return fmt.Errorf("%s: replaying its output from the build cache: %w", step, err)
				}
				r.record(s, result)
				return nil
			}
			fmt.Fprintf(r.stdout, "%s: building, since the cached result can't be used: %v\n", step, problem)
		}
	}

	var result *actionResult

	if s.output != nil {
		result, err = r.runStep(ctx, s, a.Sources, func(builder *buildah.Builder, result *actionResult) error {
			output, err := collectOutput(builder, s, r.cache)
			result.Output = &output
			return err
		})
	} else {
		result, err = r.runStep(ctx, s, a.Sources, func(builder *buildah.Builder, result *actionResult) error {
			return r.commitLayer(ctx, builder, result)
		})
	}
	if err != nil {
		return err
	}
	if err := r.cache.setResult(key, result); err != nil {
		return fmt.Errorf("%s: saving to the build cache: %w", step, err)
	}
	r.record(s, result)
	return nil
}

// inputs returns the action of step s: its inputs, which must be ready. Its sources are stored
// in the cache, so that the step is built from the files its key describes.
func (r *stepRunner) inputs(s *stepSpec) (*action, error) {
	a := &action{
		Version:     actionVersion,
		Kind:        "layer",
		Base:        r.images[s.base].id,
		Destination: s.destination,
		Run:         s.run,
		Command:     s.command,
	}
	if s.output != nil {
		a.Kind = "task"
		a.Source = s.source
		a.Include = sourcePaths([]*fileSet{s.output})
	}
	if len(s.sources) > 0 {
		sources, err := r.sourceTree(s)
		if err != nil {
			return nil, err
		}
		b, err := r.cache.putBytes(sources.encode())
		if err != nil {
			return nil, fmt.Errorf("saving sources to the build cache: %w", err)
		}
		a.Sources = &b
	}
	return a, nil
}

// sourceTree describes s's sources as one tree, as they are placed beneath s.destination.
// Project files are stored in the cache as they are read, so that a file changed later, even
// during the build, can't make the step's files differ from what its key says.
func (r *stepRunner) sourceTree(s *stepSpec) (*tree, error) {
	sources := &tree{}
	for _, group := range groupByOrigin(s.sources) {
		var t *tree
		var err error
		if task := group[0].task; task != nil {
			t, err = r.cache.readTree(r.outputs[task.output].tree)
		} else {
			t, err = r.cache.putFiles(r.fs, r.dir, sourcePaths(group))
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", group[0].name, err)
		}
		// checkOverlap makes sure sources from different places don't share paths.
		sources.Entries = append(sources.Entries, t.Entries...)
	}
	slices.SortFunc(sources.Entries, func(a, b treeEntry) int { return strings.Compare(a.Path, b.Path) })
	return sources, nil
}

// checkResult reports why a cached result for s can't be used, if it can't: something it
// names may have been removed since, from the cache or the container store.
func (r *stepRunner) checkResult(s *stepSpec, result *actionResult) error {
	if s.output != nil {
		if result.Output == nil {
			return errors.New("it has no files")
		}
		if err := r.cache.checkTree(*result.Output); err != nil {
			return fmt.Errorf("its files are missing from the cache: %w", err)
		}
	} else {
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

// replay writes what a cached step printed to stdout and stderr.
func (r *stepRunner) replay(result *actionResult) error {
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

// record makes the result of step s available to later steps.
func (r *stepRunner) record(s *stepSpec, result *actionResult) {
	epoch := time.Unix(result.Epoch, 0)
	if s.output != nil {
		r.outputs[s.output] = taskOutput{tree: *result.Output, epoch: epoch}
	} else {
		r.images[s.name] = builtImage{id: result.Image.Encoded(), epoch: epoch}
	}
}

// epoch returns the time to stamp the files of s's result with when it runs: the latest of the
// epochs of its base and of the tasks among its sources, and of r.timestamp.
func (r *stepRunner) epoch(s *stepSpec) time.Time {
	epoch := r.images[s.base].epoch
	if r.timestamp.After(epoch) {
		epoch = r.timestamp
	}
	for _, set := range s.sources {
		if set.task != nil {
			if e := r.outputs[set].epoch; e.After(epoch) {
				epoch = e
			}
		}
	}
	return epoch
}

// runStep runs step s with its sources, the tree stored as blob sources if it has any, and saves
// what it printed to the cache. It then calls finish with the step's container to complete its
// result: to commit a layer's image, or collect a task's files.
func (r *stepRunner) runStep(
	ctx context.Context, s *stepSpec, sources *blob, finish func(*buildah.Builder, *actionResult) error,
) (*actionResult, error) {
	step := describeStep(s)
	log, err := r.cache.newLog()
	if err != nil {
		return nil, fmt.Errorf("%s: saving to the build cache: %w", step, err)
	}
	defer log.discard()
	builder, err := r.start(ctx, s, sources, log)
	if err != nil {
		return nil, err
	}
	defer builder.Delete()

	result := &actionResult{Epoch: r.epoch(s).Unix()}
	// Save the log first, so that a failure saving it can't leave an image no result names.
	if result.Log, err = log.save(r.cache); err != nil {
		return nil, fmt.Errorf("%s: saving to the build cache: %w", step, err)
	}
	if err := finish(builder, result); err != nil {
		return nil, fmt.Errorf("%s: %w", step, err)
	}
	return result, nil
}

// commitLayer commits the image of a layer from builder to the container store.
func (r *stepRunner) commitLayer(ctx context.Context, builder *buildah.Builder, result *actionResult) error {
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
func (r *stepRunner) send(ctx context.Context, id string) (string, error) {
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

// start makes a container for step s from its base image, adds its sources, the tree stored as
// blob sources, and runs its run step or command, recording what it prints in log. A layer's
// default command is set too. The caller must delete the returned builder.
func (r *stepRunner) start(
	ctx context.Context, s *stepSpec, sources *blob, log *logRecorder,
) (_ *buildah.Builder, err error) {
	step := describeStep(s)
	builder, err := buildah.NewBuilder(ctx, r.store, buildah.BuilderOptions{
		FromImage:     r.images[s.base].id,
		PullPolicy:    buildah.PullNever, // only pull() reaches registries
		Capabilities:  r.capabilities,
		SystemContext: r.systemContext,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", step, err)
	}
	defer func() {
		if err != nil {
			builder.Delete()
		}
	}()

	if sources != nil {
		if err := r.addSources(builder, s, *sources); err != nil {
			return nil, fmt.Errorf("%s: %w", step, err)
		}
	}

	// builder.Run can't be cancelled, so check for an interrupt before and after it.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(s.run) > 0 {
		err = builder.Run(s.run, buildah.RunOptions{
			Isolation: r.isolation,
			Terminal:  buildah.WithoutTerminal,
			Stdout:    log.writer(streamStdout, r.stdout),
			Stderr:    log.writer(streamStderr, r.stderr),
		})
		if err != nil {
			what := "run"
			if s.output != nil {
				what = "command"
			}
			return nil, fmt.Errorf("%s: %s %q failed: %w", step, what, s.run, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if s.output == nil && len(s.command) > 0 {
		builder.SetCmd(s.command)
	}
	return builder, nil
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

// addSources copies s's sources, the tree stored as blob sources, into builder's container,
// beneath s.destination.
func (r *stepRunner) addSources(builder *buildah.Builder, s *stepSpec, sources blob) error {
	// buildah reads sources from disk, so check them out into a temporary folder: from the cache,
	// even the project's, since the project's files may have changed since they were stored.
	contextDir, err := os.MkdirTemp("", "vamos-context-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(contextDir)
	if err := r.cache.checkout(sources, contextDir); err != nil {
		return fmt.Errorf("copying sources from the build cache: %w", err)
	}
	for _, group := range groupByOrigin(s.sources) {
		for _, src := range sourcePaths(group) {
			if err := addPath(builder, contextDir, src, s.destination); err != nil {
				return fmt.Errorf("adding %q from %s: %w", src, group[0].name, err)
			}
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

// collectOutput saves the files of task s from builder's container to c, returning their tree.
func collectOutput(builder *buildah.Builder, s *stepSpec, c *cache) (blob, error) {
	mountPoint, err := builder.Mount("")
	if err != nil {
		return blob{}, err
	}
	defer builder.Unmount()

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

// groupByOrigin groups sets by where their files are: the project, for globs, or a task's output.
func groupByOrigin(sets []*fileSet) [][]*fileSet {
	var groups [][]*fileSet
	for _, set := range sets {
		i := slices.IndexFunc(groups, func(g []*fileSet) bool { return g[0].task == set.task })
		if i < 0 {
			groups = append(groups, []*fileSet{set})
		} else {
			groups[i] = append(groups[i], set)
		}
	}
	return groups
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
