// Package finish sends what a build made where a command says: to a registry, the Docker daemon, or a tarball.
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

	"github.com/Topher-the-Geek/vamos/internal/run"

	"github.com/containerd/platforms"
	dockerclient "github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/jsonmessage"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/docker/archive"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/types"
)

// PushFrom says which side pushes an image: the executor, which holds it, with its credentials and registry
// settings, or the client, with its own.
type PushFrom string

const (
	FromExecutor PushFrom = "executor"
	FromClient   PushFrom = "client"
)

// Push pushes the image or index of res to the registry name, from the side from says, returning the image's ID
// there, or the index's digest. sys describes the client's registry settings.
func Push(ctx context.Context, ex run.Executor, res run.Result, name reference.Named, from PushFrom,
	sys *types.SystemContext) (string, error) {
	if from == FromClient {
		ref, err := docker.NewReference(name)
		if err != nil {
			return "", err
		}
		return ex.Export(ctx, res, ref, sys)
	}
	return ex.Push(ctx, res, name)
}

// Save writes the image of res to a tarball at path, which `docker load` imports, recording name in it unless
// name is nil, and returns its image ID there. It writes a temporary file beside path, created only once res is
// built, and renames it into place, so that an existing file is replaced only by a complete image.
func Save(ctx context.Context, ex run.Executor, res run.Result, path string, name reference.NamedTagged,
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
	id, err := ex.Export(ctx, res, ref, sys)
	if err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return id, os.Rename(tmp.Name(), path)
}

// LoadDocker loads the image of res into the Docker daemon at $DOCKER_HOST, or the local socket, named name, and
// returns its image ID. It writes a tarball and sends it with the Docker client rather than use the
// docker-daemon transport, which can't reach a daemon over plain HTTP on tcp://, and ignores $DOCKER_CERT_PATH
// and $DOCKER_TLS_VERIFY.
func LoadDocker(ctx context.Context, ex run.Executor, res run.Result, name reference.NamedTagged,
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
	id, err := ex.Export(ctx, res, ref, sys)
	if err != nil {
		return "", err
	}
	return id, loadIntoDocker(ctx, tmp)
}

// SavePlatform returns the platform of the image that save writes for an index: the host's, whose Docker
// daemon is likely to load it.
func SavePlatform(context.Context) (imgspecv1.Platform, string, error) {
	return run.HostPlatform, "the host's platform", nil
}

// archiveReference returns a docker-archive reference for writing an image to f, an empty
// file, recording name in it unless name is nil.
func archiveReference(f *os.File, name reference.NamedTagged) (types.ImageReference, error) {
	path := f.Name()
	// docker-archive rejects paths containing ':', so reach such a file through f instead.
	if strings.Contains(path, ":") {
		path = fmt.Sprintf("/proc/self/fd/%d", f.Fd())
	}
	return archive.NewReference(path, name)
}

// newDockerClient returns a client for the Docker daemon at $DOCKER_HOST, or the local
// socket, set up from the environment as the docker command does.
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
	// The daemon names its architecture as uname does, which containerd normalizes except for 32-bit ARM's,
	// such as "armv7l".
	if v, ok := strings.CutPrefix(p.Architecture, "armv"); ok && strings.HasSuffix(v, "l") {
		p.Architecture, p.Variant = "arm", "v"+strings.TrimSuffix(v, "l")
	}
	return platforms.Normalize(p), "the Docker daemon's platform", nil
}

// loadIntoDocker sends the image tarball in f to the Docker daemon, like `docker load`.
func loadIntoDocker(ctx context.Context, f *os.File) error {
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
// reporting clearly if path can't be written there. The file gets the mode of an
// existing file at path, or else the usual mode of a new file.
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
