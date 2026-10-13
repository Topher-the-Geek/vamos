package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/fileset"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/tracing"

	"github.com/containerd/platforms"
	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/docker/distribution/registry/api/errcode"
	v2 "github.com/docker/distribution/registry/api/v2"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/afero"
	"go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// cacheDir returns where the build cache is kept: beside the container store, so that a volume
// holding /var/lib/containers keeps both.
func cacheDir(options storage.StoreOptions) string {
	return filepath.Join(filepath.Dir(options.GraphRoot), "vamos")
}

// CacheRepository names the images of cached layers in the container store, as
// CacheRepository:<image ID>, which keeps tools that remove unnamed images from removing them.
const CacheRepository = "localhost/vamos-cache"

// scratch is the name buildah starts an empty image from, as in a Dockerfile's "FROM scratch".
const scratch = "scratch"

// builtImage is the image of a pull or a layer, once it is in the container store.
type builtImage struct {
	id    string    // its ID in the container store, or scratch
	epoch time.Time // when it was made; files that targets add to it are stamped with this, or a later epoch
}

// setTree is the files of a task, once they are in the build cache.
type setTree struct {
	tree  cache.Blob // the files, as a tree
	epoch time.Time  // as for builtImage
}

// targetRunner runs the layers, tasks and indexes of a build.
type targetRunner struct {
	graph         *graph.Graph
	store         storage.Store
	cache         *cache.Cache
	systemContext *types.SystemContext
	timestamp     time.Time // if set, the earliest time that targets which run stamp their files with
	events        Events
	capabilities  []string // given to run() steps, as root
	isolation     define.Isolation
	images        map[graph.Ref]builtImage // the images of pulls and layers, once they are ready
	trees         map[graph.Ref]setTree    // the files of tasks, once they have run
	indexes       map[graph.Ref][]Manifest // the images of indexes, once they are ready
}

// runCached makes the result of the layer or task ref ready for later targets, reusing the result the cache
// holds for its inputs or else running it and saving the result to the cache. It reports which
// it did, and replays a reused target's output, so a build prints what the target printed. With
// rebuild, it runs the target even if its result is cached, and replaces that result.
func (r *targetRunner) runCached(ctx context.Context, ref graph.Ref, rebuild bool) (err error) {
	n := r.graph.Node(ref)
	target := n.Desc
	a := r.inputs(n)
	key := a.key()
	ctx, span := tracing.StartTrack(ctx, target, target, "key", key.String(), "base", a.Base)
	if n.Layer != nil {
		span.Set("platform", n.Layer.Platform)
	}
	defer func() { span.EndWith(err) }()
	status := func(status string) {
		span.Set("outcome", status)
		r.events.Status(ref, status)
	}
	if rebuild {
		status("rebuilding")
	} else {
		result, problem, err := r.lookup(ctx, n, key)
		switch {
		case err != nil:
			return fmt.Errorf("%s: reading the build cache: %w", target, err)
		case result == nil:
			status("building")
		case problem == nil:
			status("cached")
			if err := r.replay(ref, result); err != nil {
				return fmt.Errorf("%s: replaying its output from the build cache: %w", target, err)
			}
			r.record(ref, result)
			if tracing.Enabled(ctx) {
				span.Set(resultArgs(result)...)
			}
			span.Produce(graph.FlowKey(ref)) // as the commit or collection that made it would
			return nil
		default:
			status(fmt.Sprintf("building, since the cached result can't be used: %v", problem))
		}
	}

	var result *cache.ActionResult
	if n.Task != nil {
		result, err = r.runTarget(ctx, ref, a, func(st *targetState, result *cache.ActionResult) error {
			if n.Task.Output == nil {
				return nil
			}
			_, span := tracing.Start(ctx, "collect output", "source", n.Task.Output.Source,
				"include", n.Task.Output.Selection.Include, "exclude", n.Task.Output.Selection.Exclude)
			output, stats, err := collectOutput(st.mountPoint, n.Task.Output, r.cache)
			result.Output = &output
			if err == nil {
				span.Set(stats.Args()...)
				span.Set("tree", output.Digest.String())
				span.Produce(graph.FlowKey(ref))
			}
			span.EndWith(err)
			return err
		})
	} else {
		result, err = r.runTarget(ctx, ref, a, func(st *targetState, result *cache.ActionResult) error {
			return r.commitLayer(ctx, ref, st.builder, result)
		})
	}
	if err != nil {
		return err
	}
	if err := r.saveResult(ctx, key, result); err != nil {
		return fmt.Errorf("%s: saving to the build cache: %w", target, err)
	}
	r.record(ref, result)
	if tracing.Enabled(ctx) {
		span.Set(resultArgs(result)...)
	}
	return nil
}

// lookup returns the result that the cache holds for n's key, and why it can't be used, if it can't, or a nil
// result if the cache has none.
func (r *targetRunner) lookup(ctx context.Context, n *graph.Node, key digest.Digest) (
	_ *cache.ActionResult, problem error, err error) {
	_, span := tracing.Start(ctx, "cache lookup", "key", key.String())
	defer func() { span.EndWith(err) }()
	result, err := r.cache.Result(key)
	if err != nil || result == nil {
		span.Set("hit", false)
		return nil, nil, err
	}
	problem = r.checkResult(n, result)
	span.Set("hit", problem == nil)
	if problem != nil {
		span.Set("unusable", problem.Error())
	}
	return result, problem, nil
}

// saveResult saves result to the cache as key's.
func (r *targetRunner) saveResult(ctx context.Context, key digest.Digest, result *cache.ActionResult) (err error) {
	_, span := tracing.Start(ctx, "cache write", "key", key.String())
	defer func() { span.EndWith(err) }()
	if result.Log != nil {
		span.Set("log", result.Log.Digest.String(), "log bytes", result.Log.Size)
	}
	return r.cache.SetResult(key, result)
}

// resultArgs returns what a trace records of a target's result: its image or its files, and its epoch.
func resultArgs(result *cache.ActionResult) []any {
	args := []any{"epoch", time.Unix(result.Epoch, 0).UTC().Format(time.RFC3339)}
	if result.Image != "" {
		args = append(args, "image ID", result.Image.Encoded())
	}
	if result.Output != nil {
		args = append(args, "files", result.Output.Digest.String())
	}
	return args
}

// inputs returns the action of the layer or task n: its inputs, which must be ready. The files it adds are
// in the cache, so that the target is built from the files its key describes.
func (r *targetRunner) inputs(n *graph.Node) *action {
	a := &action{
		Version: actionVersion,
		Kind:    "layer",
		Base:    r.images[n.Base()].id,
	}
	if n.Task != nil {
		a.Kind = "task"
		if out := n.Task.Output; out != nil {
			a.Source = out.Source
			a.Output = out.Selection
		}
	} else {
		// A layer's image says what it is for, even one on scratch, which has no platform of its own.
		a.Platform = n.Layer.Platform
		a.Command = n.Layer.Command
	}
	for _, in := range n.Steps() {
		entry := actionEntry(in)
		if in.From != nil {
			b := r.files(*in.From)
			entry.Files = &b
			// The tree has no times, and the task's epoch can differ for the same files, e.g. after --rebuild.
			if e := r.trees[*in.From].epoch; !e.IsZero() {
				entry.Epoch = e.Unix()
			}
		}
		a.Steps = append(a.Steps, entry)
	}
	return a
}

// files returns the tree of the files of ref, a glob or a task that has run, stored in the cache.
func (r *targetRunner) files(ref graph.Ref) cache.Blob {
	if src := r.graph.Node(ref).Source; src != nil {
		return src.Tree
	}
	return r.trees[ref].tree
}

// checkResult reports why a cached result for n can't be used, if it can't: something it
// names may have been removed since, from the cache or the container store.
func (r *targetRunner) checkResult(n *graph.Node, result *cache.ActionResult) error {
	if n.Task != nil && n.Task.Output != nil {
		if result.Output == nil {
			return errors.New("it has no files")
		}
		if err := r.cache.CheckTree(*result.Output); err != nil {
			return fmt.Errorf("its files are missing from the cache: %w", err)
		}
	} else if n.Layer != nil {
		if result.Image == "" {
			return errors.New("it has no image")
		}
		if _, err := r.store.Image(result.Image.Encoded()); err != nil {
			return fmt.Errorf("its image %s is no longer in the container store", result.Image.Encoded()[:12])
		}
	}
	if result.Log != nil {
		if err := r.cache.Has(*result.Log); err != nil {
			return fmt.Errorf("its output is missing from the cache: %w", err)
		}
	}
	return nil
}

// replay passes on what the cached target ref printed to its events.
func (r *targetRunner) replay(ref graph.Ref, result *cache.ActionResult) error {
	if result.Log == nil {
		return nil
	}
	log, err := r.cache.OpenBlob(*result.Log)
	if err != nil {
		return err
	}
	defer log.Close()
	return cache.ReplayLog(log, r.output(ref, cache.StreamStdout), r.output(ref, cache.StreamStderr))
}

// record makes the result of the layer or task ref available to later targets.
func (r *targetRunner) record(ref graph.Ref, result *cache.ActionResult) {
	n := r.graph.Node(ref)
	epoch := time.Unix(result.Epoch, 0)
	if n.Task != nil {
		if n.Task.Output != nil {
			r.trees[ref] = setTree{tree: *result.Output, epoch: epoch}
		}
	} else {
		r.images[ref] = builtImage{id: result.Image.Encoded(), epoch: epoch}
	}
}

// output returns a writer that passes what the target ref prints to stream on to its events.
func (r *targetRunner) output(ref graph.Ref, stream byte) io.Writer {
	return eventWriter{events: r.events, node: ref, stream: stream}
}

// epoch returns the time to stamp the files of n's result with when it runs: the latest of the
// epochs of its base and of the tasks whose files it adds, and of r.timestamp.
func (r *targetRunner) epoch(n *graph.Node) time.Time {
	epoch := r.images[n.Base()].epoch
	if r.timestamp.After(epoch) {
		epoch = r.timestamp
	}
	for _, in := range n.Steps() {
		if in.From != nil {
			if e := r.trees[*in.From].epoch; e.After(epoch) {
				epoch = e
			}
		}
	}
	return epoch
}

// runTarget runs the layer or task ref, whose action is a, and saves what it printed to the cache. It then calls
// finish with the target's container, still mounted, to complete its result: to commit a layer's
// image, or collect a task's files.
func (r *targetRunner) runTarget(
	ctx context.Context, ref graph.Ref, a *action, finish func(*targetState, *cache.ActionResult) error,
) (*cache.ActionResult, error) {
	n := r.graph.Node(ref)
	target := n.Desc
	log, err := r.cache.NewLog()
	if err != nil {
		return nil, fmt.Errorf("%s: saving to the build cache: %w", target, err)
	}
	defer log.Discard()
	st, err := r.start(ctx, ref, a, log)
	if err != nil {
		return nil, err
	}
	defer st.delete()

	result := &cache.ActionResult{Epoch: r.epoch(n).Unix()}
	// Save the log first, so that a failure saving it can't leave an image no result names.
	if result.Log, err = log.Save(r.cache); err != nil {
		return nil, fmt.Errorf("%s: saving to the build cache: %w", target, err)
	}
	if err := finish(st, result); err != nil {
		return nil, fmt.Errorf("%s: %w", target, err)
	}
	return result, nil
}

// commitLayer commits the image of the layer ref from builder to the container store.
func (r *targetRunner) commitLayer(
	ctx context.Context, ref graph.Ref, builder *buildah.Builder, result *cache.ActionResult,
) (err error) {
	_, span := tracing.Start(ctx, "commit")
	defer func() { span.EndWith(err) }()
	epoch := time.Unix(result.Epoch, 0)
	// A nil reference commits to the store under a temporary name, which buildah then removes.
	id, _, _, err := builder.Commit(ctx, nil, buildah.CommitOptions{
		SystemContext: forPlatform(r.systemContext, builderPlatform(builder)),
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
	if err := r.store.AddNames(id, []string{CacheRepository + ":" + id}); err != nil {
		if len(img.Names) == 0 {
			r.store.DeleteImage(id, true)
		}
		return fmt.Errorf("naming image %s: %w", id, err)
	}
	result.Image = digest.NewDigestFromEncoded(digest.SHA256, id)
	span.Set("image ID", id, "name", CacheRepository+":"+id)
	if tracing.Enabled(ctx) {
		if layer, err := r.store.Layer(img.TopLayer); err == nil {
			span.Set("top layer", layer.ID, "layer digest", layer.UncompressedDigest.String(),
				"layer size", layer.UncompressedSize)
		}
	}
	span.Produce(graph.FlowKey(ref))
	return nil
}

// writeLayoutIndex stores index as a blob of the OCI layout in dir, and makes the layout's index.json name it,
// as ref, alone.
func writeLayoutIndex(dir string, index imgspecv1.Index, ref string) error {
	data, err := json.Marshal(index)
	if err != nil {
		return err
	}
	d := digest.FromBytes(data)
	if err := os.WriteFile(filepath.Join(dir, "blobs", d.Algorithm().String(), d.Encoded()), data, 0o644); err != nil {
		return err
	}
	top := imgspecv1.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: imgspecv1.MediaTypeImageIndex,
		Manifests: []imgspecv1.Descriptor{{
			MediaType:   imgspecv1.MediaTypeImageIndex,
			Digest:      d,
			Size:        int64(len(data)),
			Annotations: map[string]string{imgspecv1.AnnotationRefName: ref},
		}},
	}
	if data, err = json.Marshal(top); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "index.json"), data, 0o644)
}

// start makes a container for the layer or task ref from its base image, mounts it, and applies its steps,
// whose files are stored as a says, recording what they print in log. A layer's default command is
// set too. The caller must delete the returned target's container.
func (r *targetRunner) start(
	ctx context.Context, ref graph.Ref, a *action, log *cache.LogRecorder,
) (_ *targetState, err error) {
	n := r.graph.Node(ref)
	target := n.Desc
	base := n.Base()
	sys := r.systemContext
	if base != graph.Scratch {
		platform, err := graph.ParsePlatform(r.graph.Node(base).Platform())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", target, err)
		}
		sys = forPlatform(sys, platform)
	}
	_, startSpan := tracing.Start(ctx, "start", "base", r.images[base].id)
	if base != graph.Scratch {
		startSpan.Use(graph.FlowKey(base))
	}
	defer func() { startSpan.EndWith(err) }() // if it fails before the container is mounted
	builder, err := buildah.NewBuilder(ctx, r.store, buildah.BuilderOptions{
		FromImage:     r.images[base].id,
		PullPolicy:    buildah.PullNever, // only pull() reaches registries
		Capabilities:  r.capabilities,
		SystemContext: sys,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", target, err)
	}
	st := &targetState{targetRunner: r, ref: ref, builder: builder, log: log, buildEnv: map[string]string{}}
	defer func() {
		if err != nil {
			st.delete()
		}
	}()
	if n.Layer != nil {
		if err := r.setPlatform(builder, n.Layer); err != nil {
			return nil, fmt.Errorf("%s: %w", target, err)
		}
	}
	// Mount the container once for the whole target, rather than for each step that needs its
	// files. buildah's own Run and Add then only count another use of the mount.
	if st.mountPoint, err = builder.Mount(""); err != nil {
		return nil, fmt.Errorf("%s: %w", target, err)
	}
	startSpan.Set("container", builder.ContainerID)
	startSpan.End()

	for i, in := range n.Steps() {
		// builder.Run can't be cancelled, so check for an interrupt between steps.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := st.tracedApply(ctx, in, a.Steps[i].Files); err != nil {
			return nil, fmt.Errorf("%s: %s failed: %w%s", target, in.Display, err, emulatorHint(in, builder))
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if n.Layer != nil && len(n.Layer.Command) > 0 {
		builder.SetCmd(n.Layer.Command)
	}
	return st, nil
}

// setPlatform makes layer l's image say it is for its platform: an image on scratch, which has none, says so
// once set, and one on another image must already be for it, since the base's files are.
func (r *targetRunner) setPlatform(builder *buildah.Builder, l *graph.Layer) error {
	want, err := graph.ParsePlatform(l.Platform)
	if err != nil {
		return err
	}
	if l.Base == graph.Scratch {
		builder.SetOS(want.OS)
		builder.SetArchitecture(want.Architecture)
		builder.SetVariant(want.Variant)
		return nil
	}
	got := builderPlatform(builder)
	if !platforms.NewMatcher(want).Match(got) {
		return fmt.Errorf("its base %s is for %s, not %s; a layer's base must be for its platform",
			r.graph.Node(l.Base).Label, platforms.Format(got), l.Platform)
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

// emulatorHint explains, if in is a run() step in an image for another platform than the host's, that its
// programs run under emulation, which may be why it failed. The error that says so, "exec format error", is
// printed by buildah, not returned.
func emulatorHint(in graph.Step, builder *buildah.Builder) string {
	if in.Op != graph.OpRun {
		return ""
	}
	p := builderPlatform(builder)
	if platforms.NewMatcher(HostPlatform).Match(p) {
		return ""
	}
	return fmt.Sprintf(`; the image is for %s, which runs here under emulation: "exec format error" above means `+
		"no emulator for it is registered (tonistiigi/binfmt registers them)", platforms.Format(p))
}

// delete unmounts and removes the target's container.
func (st *targetState) delete() {
	if st.mountPoint != "" {
		st.builder.Unmount()
	}
	st.builder.Delete()
}

// Prune removes every cached result: the images of cached layers from the container store,
// other than names that other tools gave them, and the build cache. Pulled images are kept, and so are the
// records of what their tags named, which say which pulled images builds use.
func Prune(w io.Writer) error {
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
	c, err := cache.Open(dir)
	if err != nil {
		return err
	}
	results, blobs, err := c.Clear()
	if err != nil {
		errs = append(errs, err)
	}
	fmt.Fprintf(w, "Removed %d images from the container store, and %d results and %d files from %s\n",
		removed, results, blobs, dir)
	return errors.Join(errs...)
}

// addSource copies the files of the glob or task from, the tree stored as blob files, into the target's container,
// beneath dest, owned by chown as buildah's Chown option takes it.
func (st *targetState) addSource(ctx context.Context, from graph.Ref, files cache.Blob, dest, chown string) error {
	// buildah reads sources from disk, so check them out into a temporary folder: from the cache,
	// even the project's, since the project's files may have changed since they were stored.
	contextDir, err := os.MkdirTemp("", "vamos-context-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(contextDir)
	_, checkout := tracing.Start(ctx, "checkout", "tree", files.Digest.String())
	t, err := st.cache.Checkout(files, contextDir)
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
	// Copy only the tree's entries, outermost first: folders above them that the image already has are
	// left as they are, rather than replaced by the temporary ones that hold them.
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
			return fmt.Errorf("adding %s: %w", st.graph.Node(from).Label, err)
		}
	}
	return nil
}

// addPaths copies the files at srcs, relative to dir and all in its folder parent, into builder's
// container at the same paths beneath destination, owned by chown. Directories are copied with what they
// hold, and links as links. Folders above them that the image lacks are made.
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

// collectOutput saves the files that out selects from a task's container, mounted at mountPoint, to c,
// returning their tree and their stats.
func collectOutput(mountPoint string, out *graph.Output, c *cache.Cache) (cache.Blob, fileset.Stats, error) {
	// Resolve links in source as the container would, so they can't lead outside it.
	resolved, err := securejoin.SecureJoin(mountPoint, out.Source)
	if err != nil {
		return cache.Blob{}, fileset.Stats{}, err
	}
	rel, err := filepath.Rel(mountPoint, resolved)
	if err != nil {
		return cache.Blob{}, fileset.Stats{}, err
	}
	source := filepath.Join("/", rel) // as the container sees it
	container := afero.NewBasePathFs(afero.NewOsFs(), mountPoint)
	info, err := container.Stat(source)
	if errors.Is(err, fs.ErrNotExist) {
		return cache.Blob{}, fileset.Stats{}, fmt.Errorf("source %q not found", out.Source)
	} else if err != nil {
		return cache.Blob{}, fileset.Stats{}, err
	} else if !info.IsDir() {
		return cache.Blob{}, fileset.Stats{}, fmt.Errorf("source %q is not a directory", out.Source)
	}

	// The walk refuses links on the way to each include, which the host would resolve outside the container.
	return fileset.Store(c, container, source, out.Selection)
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
