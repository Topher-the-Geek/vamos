package finish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/tracing"
	"github.com/Topher-the-Geek/vamos/internal/transfer"

	"github.com/containerd/platforms"
	dockerclient "github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/jsonmessage"
	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker/archive"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/types"
)

// PushFrom says which side pushes an image: the executor,
// which holds it, with its credentials and registry settings, or the client, with its own.
type PushFrom string

const (
	FromExecutor PushFrom = "executor"
	FromClient   PushFrom = "client"
)

// Push pushes the image or index that exp holds, read from store, to the tag name at its registry,
// with the client's credentials and registry settings, sys, retrying transient failures and reading the tag back,
// and returns the image's ID there, or the index's digest.
// The executor's own push is a job of api.Executor; this is the client's.
func Push(ctx context.Context, store cache.ContentStore, exp api.Exported, name reference.NamedTagged,
	sys *types.SystemContext) (string, error) {
	return deliver(ctx, store, exp, sys, func(
		ctx context.Context, policyContext *signature.PolicyContext, src types.ImageReference,
		options *imagecopy.Options,
	) ([]byte, error) {
		return transfer.Push(ctx, policyContext, name, src, sys, options)
	})
}

// sender copies an image or index from src, a layout, to a destination, as transfer.Image does,
// returning the manifest written.
type sender func(
	ctx context.Context, policyContext *signature.PolicyContext, src types.ImageReference, options *imagecopy.Options,
) ([]byte, error)

// copyTo copies the image or index that exp holds, read from store, to ref, a destination that sys describes,
// and returns the image's ID there, or the index's digest.
func copyTo(ctx context.Context, store cache.ContentStore, exp api.Exported, ref types.ImageReference,
	sys *types.SystemContext) (string, error) {
	return deliver(ctx, store, exp, sys, func(
		ctx context.Context, policyContext *signature.PolicyContext, src types.ImageReference,
		options *imagecopy.Options,
	) ([]byte, error) {
		return transfer.Image(ctx, policyContext, ref, src, options)
	})
}

// deliver lays the image or index that exp holds out from store in a temporary folder, sends it with send,
// as sys describes the destination, and returns the image's ID there, or the index's digest.
func deliver(ctx context.Context, store cache.ContentStore, exp api.Exported, sys *types.SystemContext,
	send sender) (_ string, err error) {
	if exp.Manifest.Digest == "" {
		return "", errors.New("the target made no image")
	}
	dir, err := os.MkdirTemp("", "vamos-export-")
	if err != nil {
		return "", err
	}
	defer func() {
		if rerr := os.RemoveAll(dir); rerr != nil {
			err = errors.Join(err, fmt.Errorf("removing the copied blobs: %w", rerr))
		}
	}()
	_, span := tracing.Start(ctx, "read blobs", "manifest", exp.Manifest.Digest.String(), "blobs", len(exp.Blobs))
	src, err := transfer.Layout(ctx, store, dir, exp.Manifest, exp.MediaType, exp.Blobs)
	span.EndWith(err)
	if err != nil {
		return "", fmt.Errorf("reading the image from the content store: %w", err)
	}
	policyContext, err := transfer.Policy(sys)
	if err != nil {
		return "", err
	}
	defer policyContext.Destroy()
	options := &imagecopy.Options{DestinationCtx: sys}
	if manifest.MIMETypeIsMultiImage(exp.MediaType) {
		options.ImageListSelection = imagecopy.CopyAllImages
	}
	written, err := send(ctx, policyContext, src, options)
	if err != nil {
		return "", err
	}
	if manifest.MIMETypeIsMultiImage(manifest.GuessMIMEType(written)) {
		return digest.FromBytes(written).String(), nil
	}
	// The ID the image has where it was written: the digest of its config, as written there,
	// which may differ from the executor's if its manifest is converted, as docker-archive does.
	m, err := manifest.FromBlob(written, manifest.GuessMIMEType(written))
	if err != nil {
		return "", fmt.Errorf("reading the manifest written: %w", err)
	}
	return m.ConfigInfo().Digest.Encoded(), nil
}

// Save writes the image that exp holds, read from store, to a tarball at path,
// which `docker load` imports, recording name in it unless name is nil, and returns its image ID there.
// It writes a temporary file beside path and renames it into place,
// so that an existing file is replaced only by a complete image.
func Save(ctx context.Context, store cache.ContentStore, exp api.Exported, path string, name reference.NamedTagged,
	sys *types.SystemContext) (_ string, err error) {
	tmp, err := createTempBeside(path)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	ref, err := archiveReference(tmp, name)
	if err != nil {
		return "", err
	}
	id, err := copyTo(ctx, store, exp, ref, sys)
	if err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return id, os.Rename(tmp.Name(), path)
}

// LoadDocker loads the image that exp holds, read from store, into the Docker daemon at $DOCKER_HOST,
// or the local socket, named name, and returns its image ID.
// It writes a tarball and sends it with the Docker client rather than use the docker-daemon transport,
// which can't reach a daemon over plain HTTP on tcp://, and ignores $DOCKER_CERT_PATH and $DOCKER_TLS_VERIFY.
func LoadDocker(ctx context.Context, store cache.ContentStore, exp api.Exported, name reference.NamedTagged,
	sys *types.SystemContext) (string, error) {
	tmp, err := os.CreateTemp("", "vamos-load-*.tar")
	if err != nil {
		return "", err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	ref, err := archiveReference(tmp, name)
	if err != nil {
		return "", err
	}
	id, err := copyTo(ctx, store, exp, ref, sys)
	if err != nil {
		return "", err
	}
	return id, loadIntoDocker(ctx, tmp)
}

// SavePlatform returns the platform of the image that save writes for an index:
// the host's, whose Docker daemon is likely to load it.
func SavePlatform(context.Context) (imgspecv1.Platform, string, error) {
	return platforms.Normalize(platforms.DefaultSpec()), "the host's platform", nil
}

// archiveReference returns a docker-archive reference for writing an image to f,
// an empty file, recording name in it unless name is nil.
func archiveReference(f *os.File, name reference.NamedTagged) (types.ImageReference, error) {
	path := f.Name()
	// docker-archive rejects paths containing ':', so reach such a file through f instead.
	if strings.Contains(path, ":") {
		path = fmt.Sprintf("/proc/self/fd/%d", f.Fd())
	}
	return archive.NewReference(path, name)
}

// newDockerClient returns a client for the Docker daemon at $DOCKER_HOST,
// or the local socket, set up from the environment as the docker command does.
func newDockerClient() (*dockerclient.Client, error) {
	return dockerclient.New(dockerclient.FromEnv)
}

// DockerPlatform returns the platform of the Docker daemon's machine, which may not be this one's.
func DockerPlatform(ctx context.Context) (imgspecv1.Platform, string, error) {
	client, err := newDockerClient()
	if err != nil {
		return imgspecv1.Platform{}, "", err
	}
	defer client.Close()
	info, err := client.Info(ctx, dockerclient.InfoOptions{})
	if err != nil {
		return imgspecv1.Platform{}, "", err
	}
	p := imgspecv1.Platform{OS: info.Info.OSType, Architecture: info.Info.Architecture}
	// The daemon names its architecture as uname does,
	// which containerd normalizes except for 32-bit ARM's, such as "armv7l".
	if v, ok := strings.CutPrefix(p.Architecture, "armv"); ok && strings.HasSuffix(v, "l") {
		p.Architecture, p.Variant = "arm", "v"+strings.TrimSuffix(v, "l")
	}
	return platforms.Normalize(p), "the Docker daemon's platform", nil
}

// loadIntoDocker sends the image tarball in f to the Docker daemon, like `docker load`.
func loadIntoDocker(ctx context.Context, f *os.File) (err error) {
	_, span := tracing.Start(ctx, "docker load")
	defer func() { span.EndWith(err) }()
	if info, err := f.Stat(); err == nil {
		span.Set("bytes", info.Size())
	}
	client, err := newDockerClient()
	if err != nil {
		return err
	}
	defer client.Close()
	res, err := client.ImageLoad(ctx, f, dockerclient.ImageLoadWithQuiet(true))
	if err != nil {
		return err
	}
	defer res.Close()
	// The daemon reports failures in the response's stream of messages.
	return jsonmessage.DisplayStream(res, io.Discard)
}

// WriteFile writes a file at path with write, through a temporary file beside it, renamed into place once complete,
// so that a failure leaves an existing file as it was.
func WriteFile(path string, write func(io.Writer) error) (err error) {
	tmp, err := createTempBeside(path)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if err := write(tmp); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// CheckSaveDest reports clearly if a file can't be written at path, before anything is built.
func CheckSaveDest(path string) error {
	f, err := createTempBeside(path)
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

// createTempBeside creates and opens an empty temporary file in path's directory,
// reporting clearly if path can't be written there.
// The file gets the mode of an existing file at path, or else the usual mode of a new file.
func createTempBeside(path string) (*os.File, error) {
	existing, err := os.Stat(path)
	if err == nil && existing.IsDir() {
		return nil, fmt.Errorf("cannot write %s: it is a directory", path)
	}
	parent := filepath.Dir(path)
	if info, err := os.Stat(parent); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("cannot write %s: directory %s does not exist", path, parent)
	} else if err != nil {
		return nil, err
	} else if !info.IsDir() {
		return nil, fmt.Errorf("cannot write %s: %s is not a directory", path, parent)
	}
	// Not os.CreateTemp, whose files are private (0600) whatever the umask.
	for {
		name := filepath.Join(parent, fmt.Sprintf(".%s.%d.tmp", filepath.Base(path), rand.Uint32()))
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
		if errors.Is(err, fs.ErrExist) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("cannot write %s: %w", path, err)
		}
		if existing != nil {
			if err := f.Chmod(existing.Mode().Perm()); err != nil {
				f.Close()
				os.Remove(name)
				return nil, fmt.Errorf("cannot write %s: %w", path, err)
			}
		}
		return f, nil
	}
}
