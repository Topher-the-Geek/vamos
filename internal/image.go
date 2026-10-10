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
	"strconv"
	"strings"

	securejoin "github.com/cyphar/filepath-securejoin"
	"github.com/docker/distribution/registry/api/errcode"
	v2 "github.com/docker/distribution/registry/api/v2"
	"github.com/spf13/afero"
	"go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/pkg/parse"
	"go.podman.io/common/pkg/config"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// buildOptions describes one image build.
type buildOptions struct {
	fs     afero.Fs // holds dir and the build file's sources
	dir    string
	file   *buildFile
	dest   func() (types.ImageReference, error) // called once the image is ready to commit
	action string                               // describes sending the image, e.g. "pushing to app:latest"

	systemContext  *types.SystemContext
	stdout, stderr io.Writer // receive the output of run steps
}

// buildImage runs o.file's steps with buildah and commits the last layer to o.dest.
// Images of earlier layers are kept in local storage only until then.
func buildImage(ctx context.Context, o buildOptions) (_ string, err error) {
	buildStoreOptions, err := storage.DefaultStoreOptions()
	if err != nil {
		return "", err
	}
	buildStore, err := storage.GetStore(buildStoreOptions)
	if err != nil {
		return "", err
	}
	defer buildStore.Shutdown(false)

	// Pull everything first, so a bad image name is reported before any slow step runs.
	images := map[string]string{} // image ID of each step, by name
	for _, p := range o.file.pulls {
		if images[p.name], err = pullImage(ctx, buildStore, p, o.systemContext); err != nil {
			return "", fmt.Errorf("pull %q: %w", p.name, err)
		}
	}

	var intermediates []string
	defer func() {
		// Report an image left behind, even once the build has succeeded, so it isn't missed.
		for _, id := range intermediates {
			if _, deleteErr := buildStore.DeleteImage(id, true); deleteErr != nil {
				err = errors.Join(err, fmt.Errorf("removing intermediate image %s: %w", id, deleteErr))
			}
		}
	}()
	// Tasks' output waits here until the steps that use it.
	outputDir, err := os.MkdirTemp("", "vamos-output-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(outputDir)

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

	images[scratch] = scratch // buildah starts an empty image from this name
	r := stepRunner{
		buildOptions: o,
		store:        buildStore,
		capabilities: capabilities,
		isolation:    isolation,
		outputs:      map[*fileSet]string{},
	}
	// loadBuildFile makes sure every task is used by a later step, so the last step is the final layer.
	steps := o.file.steps
	for i, s := range steps[:len(steps)-1] {
		if s.output != nil {
			r.outputs[s.output] = filepath.Join(outputDir, strconv.Itoa(i))
		}
		id, err := r.run(ctx, s, images[s.base], false)
		if err != nil {
			return "", err
		}
		if s.output == nil {
			intermediates = append(intermediates, id)
			images[s.name] = id
		}
	}
	s := steps[len(steps)-1]
	return r.run(ctx, s, images[s.base], true)
}

// pullImage fetches p's image into store, unless it is already there, and returns its ID.
func pullImage(
	ctx context.Context, store storage.Store, p *pullSpec, systemContext *types.SystemContext,
) (string, error) {
	image := p.image.String()
	id, err := buildah.Pull(ctx, image, buildah.PullOptions{
		Store:         store,
		SystemContext: systemContext,
		PullPolicy:    buildah.PullIfMissing,
	})
	if err != nil {
		if imageUnavailable(err) {
			return "", fmt.Errorf("image %q: %w (check the name, and log in if the image is private)", image, err)
		}
		return "", fmt.Errorf("image %q: %w", image, err)
	}
	// buildah.Pull can't be cancelled, so check for an interrupt after it.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return id, nil
}

// stepRunner runs the layers and tasks of a build.
type stepRunner struct {
	buildOptions
	store        storage.Store
	capabilities []string // given to run steps, as root
	isolation    define.Isolation
	outputs      map[*fileSet]string // where each task's output is kept on disk, once it has run
}

// run runs step s on the image baseID. A task's output is collected into r.outputs. A layer is
// committed to r.dest if final, or else to r.store, without a name. It returns the layer's image ID.
func (r *stepRunner) run(ctx context.Context, s *stepSpec, baseID string, final bool) (string, error) {
	step := fmt.Sprintf("layer %q", s.name)
	if s.output != nil {
		step = fmt.Sprintf("task %q", s.name)
	}
	builder, err := buildah.NewBuilder(ctx, r.store, buildah.BuilderOptions{
		FromImage:     baseID,
		PullPolicy:    buildah.PullNever, // only pull() reaches registries
		Capabilities:  r.capabilities,
		SystemContext: r.systemContext,
	})
	if err != nil {
		return "", fmt.Errorf("%s: %w", step, err)
	}
	defer builder.Delete()

	if len(s.sources) > 0 {
		if err := r.addSources(builder, s); err != nil {
			return "", fmt.Errorf("%s: %w", step, err)
		}
	}

	// builder.Run can't be cancelled, so check for an interrupt before and after it.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(s.run) > 0 {
		err = builder.Run(s.run, buildah.RunOptions{
			Isolation: r.isolation,
			Terminal:  buildah.WithoutTerminal,
			Stdout:    r.stdout,
			Stderr:    r.stderr,
		})
		if err != nil {
			what := "run"
			if s.output != nil {
				what = "command"
			}
			return "", fmt.Errorf("%s: %s %q failed: %w", step, what, s.run, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	if s.output != nil {
		if err := collectOutput(builder, s, r.outputs[s.output]); err != nil {
			return "", fmt.Errorf("%s: %w", step, err)
		}
		return "", nil
	}

	if len(s.command) > 0 {
		builder.SetCmd(s.command)
	}
	if !final {
		// A nil reference commits to store under a temporary name, which buildah then removes.
		imageId, _, _, err := builder.Commit(ctx, nil, buildah.CommitOptions{SystemContext: r.systemContext})
		if err != nil {
			return "", fmt.Errorf("%s: saving image: %w", step, err)
		}
		return imageId, nil
	}
	ref, err := r.dest()
	if err != nil {
		return "", fmt.Errorf("%s: %w", r.action, err)
	}
	imageId, _, _, err := builder.Commit(ctx, ref, buildah.CommitOptions{SystemContext: r.systemContext})
	if err != nil {
		return "", fmt.Errorf("%s: %w", r.action, err)
	}
	return imageId, nil
}

// addSources copies s's sources into builder's container, beneath s.destination.
func (r *stepRunner) addSources(builder *buildah.Builder, s *stepSpec) error {
	for _, group := range groupByOrigin(s.sources) {
		paths := sourcePaths(group)
		dir := ""
		if task := group[0].task; task != nil {
			dir = r.outputs[task.output] // already on disk
		} else {
			// buildah reads sources from disk, so copy the project's into a temporary folder.
			contextDir, err := os.MkdirTemp("", "vamos-context-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(contextDir)
			for _, src := range paths {
				if err := copyFromFs(r.fs, filepath.Join(r.dir, src), filepath.Join(contextDir, src)); err != nil {
					return fmt.Errorf("copying %q from %s: %w", src, group[0].name, err)
				}
			}
			dir = contextDir
		}
		for _, src := range paths {
			if err := addPath(builder, dir, src, s.destination); err != nil {
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

// collectOutput copies the files of task s out of builder's container, into dir.
func collectOutput(builder *buildah.Builder, s *stepSpec, dir string) error {
	mountPoint, err := builder.Mount("")
	if err != nil {
		return err
	}
	defer builder.Unmount()

	// Resolve links in source as the container would, so they can't lead outside it.
	resolved, err := securejoin.SecureJoin(mountPoint, s.source)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(mountPoint, resolved)
	if err != nil {
		return err
	}
	source := filepath.Join("/", rel) // as the container sees it
	container := afero.NewBasePathFs(afero.NewOsFs(), mountPoint)
	info, err := container.Stat(source)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("source %q not found", s.source)
	} else if err != nil {
		return err
	} else if !info.IsDir() {
		return fmt.Errorf("source %q is not a directory", s.source)
	}

	for _, p := range sourcePaths([]*fileSet{s.output}) {
		// checkInclude refuses links on the way, which the host would resolve outside the container.
		if err := checkInclude(container, source, p); err != nil {
			return err
		}
		if err := copyFromFs(container, filepath.Join(source, p), filepath.Join(dir, p)); err != nil {
			return fmt.Errorf("collecting %q: %w", p, err)
		}
	}
	return nil
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

// copyFromFs copies the file or directory tree at src in fsys to dst on disk, keeping each
// file's mode, special bits included. Symbolic links are copied as links, as Docker's COPY does,
// rather than followed, so they can't bring in files from outside the project.
func copyFromFs(fsys afero.Fs, src, dst string) error {
	// Follow a link at src itself, as at a project directory reached through a linked $PWD.
	// checkInclude refuses links anywhere else on the way to src.
	info, err := fsys.Stat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return copyEntry(fsys, src, dst, info)
}

// copyEntry copies the file, directory tree or symbolic link at src in fsys, described by info,
// to dst on disk.
func copyEntry(fsys afero.Fs, src, dst string, info fs.FileInfo) error {
	switch {
	case info.IsDir():
		if err := os.MkdirAll(dst, 0o700); err != nil {
			return err
		}
		names, err := readDirNames(fsys, src)
		if err != nil {
			return err
		}
		for _, name := range names {
			path := filepath.Join(src, name)
			info, _, err := lstat(fsys, path)
			if err != nil {
				return err
			}
			if err := copyEntry(fsys, path, filepath.Join(dst, name), info); err != nil {
				return err
			}
		}
		// Set the mode once the directory holds everything, in case it is read-only.
		return os.Chmod(dst, fileMode(info))
	case info.Mode()&fs.ModeSymlink != 0:
		reader, ok := fsys.(afero.LinkReader)
		if !ok {
			return fmt.Errorf("%s: cannot read symbolic links", src)
		}
		link, err := reader.ReadlinkIfPossible(src)
		if err != nil {
			return err
		}
		return os.Symlink(link, dst)
	case !info.Mode().IsRegular():
		// Opening a named pipe, for one, would wait for a writer.
		return fmt.Errorf("%s is not a regular file, directory or symbolic link", src)
	}
	in, err := fsys.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// Set the mode afterwards, since the umask would mask it and OpenFile ignores special bits.
	return os.Chmod(dst, fileMode(info))
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
