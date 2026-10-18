package run

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/fileset"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/sched"
	"github.com/Topher-the-Geek/vamos/internal/tracing"

	"github.com/containerd/platforms"
	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/docker/distribution/registry/api/errcode"
	v2 "github.com/docker/distribution/registry/api/v2"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/afero"
	"go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/docker/reference"
	is "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// cacheDir returns where the build cache is kept: beside the container store,
// so that a volume holding /var/lib/containers keeps both.
func cacheDir(options storage.StoreOptions) string {
	return filepath.Join(filepath.Dir(options.GraphRoot), "vamos")
}

// CacheRepository names the images that builds use in the container store, as CacheRepository:<image ID>:
// the layers built, which it keeps tools that remove unnamed images from removing, and the images pulled,
// which have their digest names too, so that every image is located one way, by its image ID.
const CacheRepository = "localhost/vamos-cache"

// localRunner executes actions with buildah, on the local container store, and publishes their results.
// It never reads the graph: an action carries everything it needs.
type localRunner struct {
	store         storage.Store
	content       cache.ContentStore
	systemContext *types.SystemContext
	capabilities  []string // given to run() steps, as root
	isolation     define.Isolation
	// attempt names the attempt this runner runs, on a worker, so that its container is named for it and found
	// after a kill; "" for the local executor, whose containers buildah names.
	attempt string
	// cacheRepository is the registry repository that built layers are pushed to, tagged by image ID, before their
	// result is reported, so that any worker can fetch them; "" pushes nothing, as the local executor does.
	cacheRepository string
	// pullThrough maps each upstream registry to the pull-through cache that mirrors it, by host; empty pulls
	// from upstream directly, as the local executor does.
	pullThrough map[string]string
}

// attemptPrefix starts the name of an attempt's container, so that a worker finds what a killed attempt left.
const attemptPrefix = "vamos-attempt-"

// Check reports why result, the action a's, can't be used, if it can't:
// something it names may have been removed since, from the content store or the container store, or corrupted.
// A store that can't be reached is an error instead.
// The files of a task's tree are checked to be present, but read, and so verified, only when a target adds them.
// The log isn't checked: the scheduler reads it, and so verifies it, whenever it uses the result.
// A layer's or a resolve's image must be in the container store, for the action's platform,
// whatever locations the result names:
// the local executor pulls through a resolve alone, so a cached resolve whose image is gone resolves again,
// as TestPullTag requires, from the images pulled before if the store still holds them by digest.
func (r *localRunner) Check(ctx context.Context, a *sched.Action, result *cache.ActionResult) (problem, err error) {
	switch a.Kind {
	case sched.KindTask:
		if a.Output == nil {
			break
		}
		if result.Output == nil {
			return errors.New("it has no files"), nil
		}
		if err := cache.CheckTree(ctx, r.content, *result.Output); err != nil {
			return sched.StoreProblem("its files can't be used from the cache", err)
		}
	case sched.KindLayer, sched.KindResolve:
		if result.Image == "" {
			return errors.New("it has no image"), nil
		}
		if result.Platform == "" {
			return errors.New("it doesn't record its image's platform"), nil
		}
		id := result.Image.Encoded()
		_, got, err := locateImage(ctx, r.store, id)
		if errors.Is(err, storage.ErrImageUnknown) {
			return fmt.Errorf("its image %s is no longer in the container store", shortID(id)), nil
		} else if errors.Is(err, errWrongImage) {
			return fmt.Errorf("its image %s in the container store: %w", shortID(id), err), nil
		} else if err != nil {
			return nil, err
		}
		// A record that names another platform's image, as no writer makes, is a miss rather than a wrong base.
		if want, err := graph.ParsePlatform(a.Platform); err != nil {
			return nil, err
		} else if !platforms.NewMatcher(want).Match(got) {
			return fmt.Errorf("its image %s is for %s, not %s", shortID(id), platforms.Format(got), a.Platform), nil
		}
	}
	return nil, nil
}

// Run executes req: a resolve through the puller, or a layer or task in a container, publishing its artifact,
// and returns what to record: the result, and for a resolve the tag record, which the caller writes.
func (r *localRunner) Run(ctx context.Context, req sched.Request, ev sched.Output) (*sched.Report, error) {
	switch req.Action.Kind {
	case sched.KindResolve:
		// A resolve's result isn't recorded: the tag record is, and it carries no provenance.
		pl := &puller{store: r.store, systemContext: r.systemContext, pullThrough: r.pullThrough}
		return pl.resolve(ctx, req)
	case sched.KindTask:
		return r.runTarget(ctx, req, ev, func(st *targetState, result *cache.ActionResult) error {
			a := req.Action
			if a.Output == nil {
				return nil
			}
			_, span := tracing.Start(ctx, "collect output", "source", a.Source,
				"include", a.Output.Include, "exclude", a.Output.Exclude)
			output, stats, err := collectOutput(ctx, st.mountPoint, a.Source, a.Output,
				sched.Rewriting{ContentStore: r.content, Corrupt: req.Rewrite})
			result.Output = &output
			if err == nil {
				span.Set(stats.Args()...)
				span.Set("tree", output.Digest.String())
				span.Produce(graph.TreeFlow(output))
			}
			span.EndWith(err)
			return err
		})
	case sched.KindLayer:
		return r.runTarget(ctx, req, ev, func(st *targetState, result *cache.ActionResult) error {
			return r.commitLayer(ctx, st.builder, result)
		})
	}
	return nil, fmt.Errorf("unknown action kind %q", req.Action.Kind)
}

// runTarget runs req, a layer or task, and saves what it printed to the content store.
// It then calls finish with the target's container, still mounted, to complete its result:
// to commit a layer's image, or collect a task's files; the result is returned for the caller to record,
// so that the artifact is published before the result is.
// The container is removed whatever happens, and a failure to do so is reported with the primary error.
func (r *localRunner) runTarget(
	ctx context.Context, req sched.Request, ev sched.Output,
	finish func(*targetState, *cache.ActionResult) error,
) (_ *sched.Report, err error) {
	log, err := cache.NewLog()
	if err != nil {
		return nil, fmt.Errorf("recording its output: %w", err)
	}
	defer log.Discard()
	st, err := r.start(ctx, req, log, ev)
	defer func() {
		if st == nil {
			return
		}
		cerr := st.delete()
		var se *sched.StepError
		switch {
		case cerr == nil:
		case errors.As(err, &se):
			se.Cleanup = cerr // the scheduler describes the step, and this with it
		default:
			err = errors.Join(err, fmt.Errorf("removing its container: %w", cerr))
		}
	}()
	if err != nil {
		// What a failed or cancelled attempt printed is kept, so that it can be read after the job ends.
		// The failure is what matters: a log that can't be saved is traced and left out.
		saved, serr := log.Save(ctx, r.content)
		if serr != nil {
			tracing.Current(ctx).Set("log not saved", serr.Error())
		}
		var se *sched.StepError
		if errors.As(err, &se) {
			se.Log = saved
			return nil, err
		}
		return nil, sched.WithLog(err, saved)
	}

	result := &cache.ActionResult{Epoch: req.Epoch.Unix(), Provenance: r.provenance(st.emulated)}
	// Save the log first, so that a failure saving it can't leave an image no result names.
	if result.Log, err = log.Save(ctx, r.content); err != nil {
		return nil, fmt.Errorf("saving to the build cache: %w", err)
	}
	// A failure from here on keeps the log too, since what the attempt printed may explain it.
	if err := finish(st, result); err != nil {
		return nil, sched.WithLog(err, result.Log)
	}
	return &sched.Report{Result: result}, nil
}

// provenance describes this executor, for the results it makes, and whether a result's commands ran emulated.
func (r *localRunner) provenance(emulated bool) *cache.Provenance {
	return &cache.Provenance{
		Platform:     platforms.Format(HostPlatform),
		Isolation:    r.isolation.String(),
		Capabilities: r.capabilities,
		Version:      vamosVersion(),
		Emulated:     emulated,
	}
}

// commitLayer commits the image of a layer from builder to the container store.
// HistoryTimestamp stamps all added files;
// RewriteTimestamp would only move times earlier, leaving later epochs dependent on build times or source mtimes.
func (r *localRunner) commitLayer(ctx context.Context, builder *buildah.Builder, result *cache.ActionResult) (
	err error) {
	_, span := tracing.Start(ctx, "commit")
	defer func() { span.EndWith(err) }()
	epoch := time.Unix(result.Epoch, 0)
	// A nil reference commits to the store under a temporary name, which buildah then removes.
	storeID, _, _, err := builder.Commit(ctx, nil, buildah.CommitOptions{
		SystemContext: forPlatform(r.systemContext, builderPlatform(builder)),
		// Stamp every file the layer adds, its creation and its history with the epoch,
		// so that the same files make the same layer whenever they are built and whatever times they had on disk.
		HistoryTimestamp: &epoch,
	})
	if err != nil {
		return fmt.Errorf("saving image: %w", err)
	}
	// The same files can make an image cached before, which already has its name and may be in use.
	img, err := r.store.Image(storeID)
	if err != nil {
		return fmt.Errorf("saving image: %w", err)
	}
	// An image this commit made, which has no name, is removed if it can't be given its own,
	// since no result would name it and prune finds images by their names.
	defer func() {
		if err != nil && len(img.Names) == 0 {
			r.store.DeleteImage(storeID, true)
		}
	}()
	id, platform, err := imageIdentity(ctx, r.store, storeID)
	if err != nil {
		return fmt.Errorf("saving image: %w", err)
	}
	if err := nameImage(r.store, img, id.Encoded()); err != nil {
		return err
	}
	result.Image = id
	result.Platform = platforms.Format(platform)
	result.Locations = []cache.Location{storeLocation(id.Encoded())}
	span.Set("image ID", id.Encoded(), "name", cacheName(id.Encoded()))
	if r.cacheRepository != "" {
		// The image is published to the cache registry before its result is reported, so that a result never names
		// an image other workers can't fetch, and survives this worker.
		location, err := r.publish(ctx, img.ID, id.Encoded())
		if err != nil {
			return fmt.Errorf("pushing the image to %s: %w", r.cacheRepository, err)
		}
		result.Locations = append(result.Locations, location)
	}
	if tracing.Enabled(ctx) {
		if layer, err := r.store.Layer(img.TopLayer); err == nil {
			span.Set("top layer", layer.ID, "layer digest", layer.UncompressedDigest.String(),
				"layer size", layer.UncompressedSize)
		}
	}
	span.Produce(graph.ImageFlow(id.Encoded()))
	return nil
}

// publish pushes the image storeID, whose ID is id, to the cache repository, tagged by its ID, and returns its
// location there; what arrives must have the ID it was pushed under.
func (r *localRunner) publish(ctx context.Context, storeID, id string) (_ cache.Location, err error) {
	_, span := tracing.Start(ctx, "publish", "repository", r.cacheRepository)
	defer func() { span.EndWith(err) }()
	named, err := reference.ParseNamed(r.cacheRepository)
	if err != nil {
		return cache.Location{}, fmt.Errorf("cache repository %q: %w", r.cacheRepository, err)
	}
	dest, err := reference.WithTag(reference.TrimNamed(named), id)
	if err != nil {
		return cache.Location{}, err
	}
	src, err := is.Transport.NewStoreReference(r.store, nil, storeID)
	if err != nil {
		return cache.Location{}, err
	}
	pushed, err := pushImageFrom(ctx, r.systemContext, src, dest)
	if err != nil {
		return cache.Location{}, err
	}
	if pushed != id {
		return cache.Location{}, fmt.Errorf("the registry holds image %s under %s, not %s", shortID(pushed), dest,
			shortID(id))
	}
	return registryLocation(dest), nil
}

// start makes a container for req, a layer or task, from its base image, mounts it,
// and applies its steps, recording what they print in log and passing it on to ev.
// A layer's default command is set too.
// The caller must delete the returned target's container, which is returned, to be deleted, even on failure,
// once it exists.
func (r *localRunner) start(
	ctx context.Context, req sched.Request, log *cache.LogRecorder, ev sched.Output,
) (_ *targetState, err error) {
	a := req.Action
	sys := r.systemContext
	var platform imgspecv1.Platform
	if req.Platform != "" {
		if platform, err = graph.ParsePlatform(req.Platform); err != nil {
			return nil, err
		}
		sys = forPlatform(sys, platform)
	}
	_, startSpan := tracing.Start(ctx, "start", "base", a.Base)
	if a.Base != sched.Scratch {
		startSpan.Use(graph.ImageFlow(a.Base))
	}
	defer func() { startSpan.EndWith(err) }() // if it fails before the container is mounted
	from := a.Base
	if from != sched.Scratch {
		// The image is located by its ID through the cache repository, and used by the store's ID found,
		// or else fetched from one of the locations the request names.
		if from, err = r.materialize(ctx, a.Base, platform, req.Bases); err != nil {
			return nil, fmt.Errorf("base image %s: %w", shortID(a.Base), err)
		}
	}
	options := buildah.BuilderOptions{
		FromImage:     from,
		PullPolicy:    buildah.PullNever, // only resolve actions reach registries
		Capabilities:  r.capabilities,
		SystemContext: sys,
	}
	if r.attempt != "" {
		options.Container = attemptPrefix + r.attempt
	}
	builder, err := buildah.NewBuilder(ctx, r.store, options)
	if err != nil {
		return nil, err
	}
	st := &targetState{runner: r, builder: builder, log: log, events: ev, buildEnv: map[string]string{}}
	if a.Kind == sched.KindLayer {
		if err := setPlatform(builder, a); err != nil {
			return st, err
		}
	}
	// Mount the container once for the whole target, rather than for each step that needs its files.
	// buildah's own Run and Add then only count another use of the mount.
	if st.mountPoint, err = builder.Mount(""); err != nil {
		return st, err
	}
	startSpan.Set("container", builder.ContainerID)
	startSpan.End()

	for i, in := range a.Steps {
		// builder.Run can't be cancelled, so check for an interrupt between steps.
		if err := ctx.Err(); err != nil {
			return st, err
		}
		display := in.Op
		if i < len(req.Displays) {
			display = req.Displays[i]
		}
		if in.Op == graph.OpRun && emulated(builder) {
			st.emulated = true
		}
		if err := st.tracedApply(ctx, in, display); err != nil {
			return st, &sched.StepError{Step: i, Err: err, Hint: emulatorHint(in, builder)}
		}
	}
	if err := ctx.Err(); err != nil {
		return st, err
	}

	if a.Kind == sched.KindLayer && len(a.Command) > 0 {
		builder.SetCmd(a.Command)
	}
	return st, nil
}

// setPlatform makes the image of the layer a say it is for its platform: an image on sched.Scratch,
// which has none, says so once set, and one on another image must already be for it, since the base's files are;
// the scheduler checks that before running the layer, naming the base.
func setPlatform(builder *buildah.Builder, a *sched.Action) error {
	want, err := graph.ParsePlatform(a.Platform)
	if err != nil {
		return err
	}
	if a.Base == sched.Scratch {
		builder.SetOS(want.OS)
		builder.SetArchitecture(want.Architecture)
		builder.SetVariant(want.Variant)
		return nil
	}
	got := builderPlatform(builder)
	if !platforms.NewMatcher(want).Match(got) {
		return fmt.Errorf("its base is for %s, not %s; a layer's base must be for its platform",
			platforms.Format(got), a.Platform)
	}
	return nil
}

// builderPlatform returns the platform that builder's image says it is for, normalized.
func builderPlatform(builder *buildah.Builder) imgspecv1.Platform {
	return platforms.Normalize(imgspecv1.Platform{
		OS:           builder.OS(),
		Architecture: builder.Architecture(),
		Variant:      builder.Variant(),
	})
}

// emulated reports whether builder's image is for another platform than the host's,
// so that its programs run under emulation.
func emulated(builder *buildah.Builder) bool {
	return !platforms.NewMatcher(HostPlatform).Match(builderPlatform(builder))
}

// emulatorHint explains, if in is a run() step in an image for another platform than the host's,
// that its programs run under emulation, which may be why it failed.
// The error that says so, "exec format error", is printed by buildah, not returned.
func emulatorHint(in sched.Step, builder *buildah.Builder) string {
	if in.Op != graph.OpRun || !emulated(builder) {
		return ""
	}
	return fmt.Sprintf(`the image is for %s, which runs here under emulation: "exec format error" above means `+
		"no emulator for it is registered (tonistiigi/binfmt registers them)",
		platforms.Format(builderPlatform(builder)))
}

// delete unmounts and removes the target's container, once, reporting what it couldn't.
func (st *targetState) delete() error {
	if st.builder == nil {
		return nil
	}
	var errs []error
	if st.mountPoint != "" {
		if err := st.builder.Unmount(); err != nil {
			errs = append(errs, fmt.Errorf("unmounting: %w", err))
		}
	}
	if err := st.builder.Delete(); err != nil {
		errs = append(errs, fmt.Errorf("deleting: %w", err))
	}
	st.builder = nil
	return errors.Join(errs...)
}

// prune removes every cached result:
// the images of cached layers from the container store, other than names that other tools gave them,
// and the build cache.
// Pulled images are kept, and so are the records of what their tags named, which say which pulled images builds use;
// they lose their cache-repository name until the next resolve gives it back, and aren't counted as removed.
func (l *Local) prune(ctx context.Context) (api.Pruned, error) {
	if err := l.open(); err != nil {
		return api.Pruned{}, err
	}
	// The directory's lock excludes every other use of it, in this process or another, while it is pruned;
	// the images are removed under it too, so that a prune refused for a use elsewhere removes nothing.
	var pruned api.Pruned
	var err error
	pruned.Results, pruned.Blobs, err = l.directory.Prune(ctx, func() (err error) {
		pruned.Images, err = l.removeCachedImages()
		return err
	})
	return pruned, err
}

// removeCachedImages removes the images of cached layers from the container store, and the cache-repository names
// of images that other tools named too, as prune does, and returns how many images it removed.
func (l *Local) removeCachedImages() (int, error) {
	images, err := l.store.Images()
	if err != nil {
		return 0, err
	}
	var errs []error
	removed := 0
	for _, img := range images {
		var cached, other []string
		for _, name := range img.Names {
			if strings.HasPrefix(name, CacheRepository+":") {
				cached = append(cached, name)
			} else {
				other = append(other, name)
			}
		}
		if len(cached) == 0 {
			continue
		}
		if len(other) > 0 {
			if err := l.store.RemoveNames(img.ID, cached); err != nil {
				errs = append(errs, fmt.Errorf("removing the names of image %s: %w", shortID(img.ID), err))
			}
			continue
		}
		if _, err := l.store.DeleteImage(img.ID, true); err != nil {
			errs = append(errs, fmt.Errorf("removing image %s: %w", shortID(img.ID), err))
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// addSource copies the files of the tree stored as blob files into the target's container, beneath dest,
// owned by chown as buildah's Chown option takes it.
// A failure to remove the checkout is reported with the primary error.
func (st *targetState) addSource(ctx context.Context, files cache.Blob, dest, chown string) (err error) {
	// buildah reads sources from disk, so check them out into a temporary folder:
	// from the content store, even the project's, since the project's files may have changed since they were stored.
	contextDir, err := os.MkdirTemp("", "vamos-context-")
	if err != nil {
		return err
	}
	defer func() {
		if rerr := os.RemoveAll(contextDir); rerr != nil {
			err = errors.Join(err, fmt.Errorf("removing the copied sources: %w", rerr))
		}
	}()
	_, checkout := tracing.Start(ctx, "checkout", "tree", files.Digest.String())
	t, err := cache.Checkout(ctx, st.runner.content, files, contextDir)
	if err != nil {
		checkout.EndWith(err)
		return fmt.Errorf("copying sources from the build cache: %w", err)
	}
	if tracing.Enabled(ctx) {
		count := t.Count().Args()
		checkout.Set(count...)
		tracing.Current(ctx).Set(count...) // the add() step's
	}
	checkout.End()
	_, add := tracing.Start(ctx, "copy", "dest", dest)
	defer add.End()
	// Copy only the tree's entries, outermost first:
	// folders above them that the image already has are left as they are,
	// rather than replaced by the temporary ones that hold them.
	dirs := map[string]bool{}
	tops := map[string][]string{} // the outermost entries, by their folder
	for _, e := range t.Entries {
		if e.Type == cache.EntryDir {
			dirs[e.Path] = true
		}
		if parent := path.Dir(e.Path); !dirs[parent] {
			tops[parent] = append(tops[parent], e.Path)
		}
	}
	for _, parent := range slices.Sorted(maps.Keys(tops)) {
		if err := addPaths(st.builder, contextDir, parent, tops[parent], dest, chown); err != nil {
			add.SetError(err)
			return fmt.Errorf("adding the files: %w", err)
		}
	}
	return nil
}

// addPaths copies the files at srcs, relative to dir and all in its folder parent,
// into builder's container at the same paths beneath destination, owned by chown.
// Directories are copied with what they hold, and links as links.
// Folders above them that the image lacks are made.
func addPaths(builder *buildah.Builder, dir, parent string, srcs []string, destination, chown string) error {
	options := buildah.AddAndCopyOptions{
		ContextDir:      dir,
		AllowWildcard:   types.OptionalBoolFalse,
		DirCopyContents: types.OptionalBoolFalse, // copy a directory itself, with its mode
		FollowSymlink:   types.OptionalBoolFalse, // copy a link as a link, as within a directory
		Chown:           chown,
	}
	sources := make([]string, len(srcs))
	for i, src := range srcs {
		sources[i] = filepath.Join(dir, src)
	}
	// A trailing slash makes the destination a directory, keeping each source's own name within it.
	return builder.Add(path.Join(destination, parent)+"/", false, options, sources...)
}

// collectOutput saves the files that sel selects within source in a task's container,
// mounted at mountPoint, to store, returning their tree and their stats.
func collectOutput(
	ctx context.Context, mountPoint, source string, sel *fileset.Selection, store cache.ContentStore,
) (cache.Blob, fileset.Stats, error) {
	// The selection is compiled as the graph was validated, but an action may have crossed a wire since.
	if err := sel.Compile(source); err != nil {
		return cache.Blob{}, fileset.Stats{}, err
	}
	// Resolve links in source as the container would, so they can't lead outside it.
	resolved, err := securejoin.SecureJoin(mountPoint, source)
	if err != nil {
		return cache.Blob{}, fileset.Stats{}, err
	}
	rel, err := filepath.Rel(mountPoint, resolved)
	if err != nil {
		return cache.Blob{}, fileset.Stats{}, err
	}
	within := filepath.Join("/", rel) // as the container sees it
	container := afero.NewBasePathFs(afero.NewOsFs(), mountPoint)
	info, err := container.Stat(within)
	if errors.Is(err, fs.ErrNotExist) {
		return cache.Blob{}, fileset.Stats{}, fmt.Errorf("source %q not found", source)
	} else if err != nil {
		return cache.Blob{}, fileset.Stats{}, err
	} else if !info.IsDir() {
		return cache.Blob{}, fileset.Stats{}, fmt.Errorf("source %q is not a directory", source)
	}

	// The walk refuses links on the way to each include, which the host would resolve outside the container.
	return fileset.Store(ctx, store, container, within, sel)
}

// imageUnavailable reports whether err says that a registry has no such image,
// or won't show it without credentials, as Docker Hub does for repositories that don't exist.
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
