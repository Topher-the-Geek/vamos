package internal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/distribution/registry/api/errcode"
	v2 "github.com/docker/distribution/registry/api/v2"
	"github.com/spf13/afero"
	"go.podman.io/buildah"
	"go.podman.io/buildah/pkg/parse"
	"go.podman.io/common/pkg/config"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

// buildOptions describes one image build.
type buildOptions struct {
	fs     afero.Fs // holds dir and the spec's sources
	dir    string
	spec   *buildSpec
	dest   func() (types.ImageReference, error) // called once the image is ready to commit
	action string                               // describes sending the image, e.g. "pushing to app:latest"

	systemContext  *types.SystemContext
	stdout, stderr io.Writer // receive the output of the spec's run step
}

// buildImage builds o.spec with buildah and commits the result to o.dest.
func buildImage(ctx context.Context, o buildOptions) (string, error) {
	buildStoreOptions, err := storage.DefaultStoreOptions()
	if err != nil {
		return "", err
	}

	conf, err := config.Default()
	if err != nil {
		return "", err
	}
	capabilitiesForRoot, err := conf.Capabilities("root", nil, nil)
	if err != nil {
		return "", err
	}

	buildStore, err := storage.GetStore(buildStoreOptions)
	if err != nil {
		return "", err
	}
	defer buildStore.Shutdown(false)

	builderOpts := buildah.BuilderOptions{
		FromImage:     o.spec.base,
		Capabilities:  capabilitiesForRoot,
		SystemContext: o.systemContext,
	}

	builder, err := buildah.NewBuilder(ctx, buildStore, builderOpts)
	if err != nil {
		if imageUnavailable(err) {
			return "", fmt.Errorf("base image %q: %w (check the name, and log in if the image is private)",
				o.spec.base, err)
		}
		return "", fmt.Errorf("base image %q: %w", o.spec.base, err)
	}
	defer builder.Delete()

	if len(o.spec.sources) > 0 {
		// buildah reads sources from disk, so copy them into a temporary build context.
		contextDir, err := os.MkdirTemp("", "vamos-context-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(contextDir)

		sources := make([]string, len(o.spec.sources))
		for i, src := range o.spec.sources {
			if err := copyFromFs(o.fs, filepath.Join(o.dir, src), filepath.Join(contextDir, src)); err != nil {
				return "", fmt.Errorf("copying source %q: %w", src, err)
			}
			sources[i] = filepath.Join(contextDir, src)
		}
		destination := o.spec.destination
		if !strings.HasSuffix(destination, "/") {
			destination += "/"
		}
		err = builder.Add(destination, false, buildah.AddAndCopyOptions{ContextDir: contextDir}, sources...)
		if err != nil {
			return "", fmt.Errorf("adding sources: %w", err)
		}
	}

	// builder.Run can't be cancelled, so check for an interrupt before and after it.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(o.spec.run) > 0 {
		isolation, err := parse.IsolationOption("")
		if err != nil {
			return "", err
		}
		err = builder.Run(o.spec.run, buildah.RunOptions{
			Isolation: isolation,
			Terminal:  buildah.WithoutTerminal,
			Stdout:    o.stdout,
			Stderr:    o.stderr,
		})
		if err != nil {
			return "", fmt.Errorf("run %q failed: %w", o.spec.run, err)
		}
	}

	if len(o.spec.command) > 0 {
		builder.SetCmd(o.spec.command)
	}

	if err := ctx.Err(); err != nil {
		return "", err
	}
	dest, err := o.dest()
	if err != nil {
		return "", fmt.Errorf("%s: %w", o.action, err)
	}
	imageId, _, _, err := builder.Commit(ctx, dest, buildah.CommitOptions{SystemContext: o.systemContext})
	if err != nil {
		return "", fmt.Errorf("%s: %w", o.action, err)
	}
	return imageId, nil
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

// copyFromFs copies the file or directory tree at src in fsys to dst on disk.
// Symbolic links are copied as links, as Docker's COPY does, rather than followed,
// so they can't bring in files from outside the project.
func copyFromFs(fsys afero.Fs, src, dst string) error {
	return afero.Walk(fsys, src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			reader, ok := fsys.(afero.LinkReader)
			if !ok {
				return fmt.Errorf("%s: cannot read symbolic links", path)
			}
			link, err := reader.ReadlinkIfPossible(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case !info.Mode().IsRegular():
			// Opening a named pipe, for one, would wait for a writer.
			return fmt.Errorf("%s is not a regular file, directory or symbolic link", path)
		}
		in, err := fsys.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}
