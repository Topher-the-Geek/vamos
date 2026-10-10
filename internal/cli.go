package internal

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

	dockerclient "github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/jsonmessage"
	"github.com/sirupsen/logrus"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/docker/archive"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/types"
)

// NewRootCommand returns the vamos command line, with its subcommands.
// BUILD files and their sources are read from fsys.
func NewRootCommand(fsys afero.Fs) *cobra.Command {
	var dir string
	var tlsVerify bool

	root := &cobra.Command{
		Use:   "vamos",
		Short: "Build container images from Starlark BUILD files",
		// Arguments are valid by the time this runs, so later errors shouldn't print usage.
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			cmd.SilenceUsage = true
			// buildah logs routine details at info level, which would bury our own output.
			logrus.SetLevel(logrus.WarnLevel)
		},
		// Report unknown commands ourselves, since SilenceErrors hides Cobra's hints.
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return nil
			}
			msg := fmt.Sprintf("unknown command %q", args[0])
			if suggestions := cmd.SuggestionsFor(args[0]); len(suggestions) > 0 {
				msg += fmt.Sprintf("; did you mean %q?", suggestions[0])
			}
			return errors.New(msg)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
		SilenceErrors:              true,
		SuggestionsMinimumDistance: 2,
	}
	root.PersistentFlags().StringVarP(&dir, "dir", "C", ".", "directory containing the BUILD file")
	root.PersistentFlags().BoolVar(&tlsVerify, "tls-verify", true,
		"require HTTPS and verify certificates when contacting registries")

	// build builds the image described by dir/BUILD and sends it to dest.
	build := func(cmd *cobra.Command, dest destination) error {
		systemContext, err := newSystemContext(cmd, tlsVerify)
		if err != nil {
			return err
		}
		absDir, err := filepath.Abs(dir)
		if err != nil {
			return err
		}
		spec, err := loadBuildFile(fsys, absDir)
		if err != nil {
			return err
		}

		imageId, err := buildImage(cmd.Context(), buildOptions{
			fs:            fsys,
			dir:           absDir,
			spec:          spec,
			dest:          dest.ref,
			action:        dest.action,
			systemContext: systemContext,
			stdout:        cmd.OutOrStdout(),
			stderr:        cmd.ErrOrStderr(),
		})
		if err != nil {
			return err
		}
		if dest.deliver != nil {
			if err := dest.deliver(cmd.Context()); err != nil {
				return fmt.Errorf("%s: %w", dest.action, err)
			}
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Image built! %s %s\n", dest.label, imageId)
		return nil
	}

	push := &cobra.Command{
		Use:     "push <image>",
		Short:   "Build the image and push it to a registry",
		Example: "  vamos push ghcr.io/me/app:1.0",
		Args:    exactlyOne("image name", "vamos push ghcr.io/me/app:1.0"),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := parseImageName(args[0])
			if err != nil {
				return err
			}
			label := reference.FamiliarString(name)
			return build(cmd, destination{
				ref:    func() (types.ImageReference, error) { return docker.NewReference(name) },
				action: "pushing to " + label,
				label:  label,
			})
		},
	}

	load := &cobra.Command{
		Use:     "load <image>",
		Short:   "Build the image and load it into the local Docker daemon",
		Long:    "Build the image and load it into the Docker daemon at $DOCKER_HOST, or the local socket.",
		Example: "  vamos load app:dev",
		Args:    exactlyOne("image name", "vamos load app:dev"),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := parseImageName(args[0])
			if err != nil {
				return err
			}
			// Write a tarball and send it with the Docker client rather than use the
			// docker-daemon transport, which can't reach a daemon over plain
			// HTTP on tcp://, and ignores $DOCKER_CERT_PATH and $DOCKER_TLS_VERIFY.
			var tmp *os.File
			defer func() {
				if tmp != nil {
					tmp.Close()
					os.Remove(tmp.Name())
				}
			}()
			return build(cmd, destination{
				ref: func() (types.ImageReference, error) {
					var err error
					if tmp, err = os.CreateTemp("", "vamos-load-*.tar"); err != nil {
						return nil, err
					}
					return archiveReference(tmp, name)
				},
				deliver: func(ctx context.Context) error { return loadIntoDocker(ctx, tmp) },
				action:  "loading into Docker",
				label:   reference.FamiliarString(name),
			})
		},
	}

	var tag string
	write := &cobra.Command{
		Use:     "write <file.tar>",
		Short:   "Build the image and write it to a tarball that `docker load` can import",
		Example: "  vamos write app.tar --tag app:dev",
		Args:    exactlyOne("output file", "vamos write app.tar"),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			path, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			var name reference.NamedTagged
			if tag != "" {
				if name, err = parseImageName(tag); err != nil {
					return err
				}
			}
			// Report an unwritable path now rather than after building.
			if err := checkWritable(path); err != nil {
				return err
			}

			// Write to a temporary file beside path and rename it into place, so an existing
			// file is replaced only by a complete image.
			var tmp *os.File
			defer func() {
				if err != nil && tmp != nil {
					tmp.Close()
					os.Remove(tmp.Name())
				}
			}()
			return build(cmd, destination{
				ref: func() (types.ImageReference, error) {
					var err error
					if tmp, err = createTempBeside(path); err != nil {
						return nil, err
					}
					return archiveReference(tmp, name)
				},
				deliver: func(ctx context.Context) error {
					if err := tmp.Close(); err != nil {
						return err
					}
					return os.Rename(tmp.Name(), path)
				},
				action: "writing " + args[0],
				label:  args[0],
			})
		},
	}
	write.Flags().StringVarP(&tag, "tag", "t", "", "image name to record in the tarball")

	root.AddCommand(push, load, write)
	return root
}

// destination says where build sends an image.
type destination struct {
	// ref returns the reference to commit the image to. It is called only once the image
	// is ready to commit, so that a file it creates isn't copied in as a source.
	ref func() (types.ImageReference, error)
	// deliver, if not nil, finishes sending the committed image.
	deliver func(ctx context.Context) error

	action string // describes sending the image, e.g. "pushing to app:latest"
	label  string // names the destination after "Image built!"
}

// exactlyOne requires a single positional argument, described by what and example.
func exactlyOne(what, example string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		switch {
		case len(args) == 0:
			return fmt.Errorf("missing %s, e.g. %s", what, example)
		case len(args) > 1:
			return fmt.Errorf("expected one %s, got %d arguments: %s", what, len(args), strings.Join(args, " "))
		}
		return nil
	}
}

// parseImageName parses a name such as "app" or "ghcr.io/me/app:1.0", adding
// ":latest" when no tag is given.
func parseImageName(name string) (reference.NamedTagged, error) {
	named, err := reference.ParseNormalizedNamed(name)
	if err != nil {
		return nil, fmt.Errorf("invalid image name %q: %w", name, err)
	}
	if _, ok := named.(reference.Digested); ok {
		return nil, fmt.Errorf("image name %q must not include a digest", name)
	}
	return reference.TagNameOnly(named).(reference.NamedTagged), nil
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

// newSystemContext returns the settings for reaching registries and the Docker daemon.
func newSystemContext(cmd *cobra.Command, tlsVerify bool) (*types.SystemContext, error) {
	sys := &types.SystemContext{}
	// Unless --tls-verify is given, leave it to registries.conf, so that "insecure = true"
	// entries there still apply.
	if cmd.Flags().Changed("tls-verify") {
		sys.DockerInsecureSkipTLSVerify = types.NewOptionalBool(!tlsVerify)
	}
	// containers/image doesn't read $REGISTRY_AUTH_FILE itself; podman and buildah pass it on.
	if path := os.Getenv("REGISTRY_AUTH_FILE"); path != "" {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("REGISTRY_AUTH_FILE: %w", err)
		}
		sys.AuthFilePath = path
	}
	return sys, nil
}

// newDockerClient returns a client for the Docker daemon at $DOCKER_HOST, or the local
// socket, set up from the environment as the docker command does.
func newDockerClient() (*dockerclient.Client, error) {
	return dockerclient.New(dockerclient.FromEnv)
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

// checkWritable reports clearly if a file can't be written at path.
func checkWritable(path string) error {
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
