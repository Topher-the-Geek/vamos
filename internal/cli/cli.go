// Package cli is vamos's command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/fileset"
	"github.com/Topher-the-Geek/vamos/internal/finish"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/load"
	"github.com/Topher-the-Geek/vamos/internal/run"
	"github.com/Topher-the-Geek/vamos/internal/tracing"

	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/types"
)

// NewRootCommand returns the vamos command line, with its subcommands.
// BUILD files and their sources are read from fsys.
func NewRootCommand(fsys afero.Fs) *cobra.Command {
	var dir string
	var tlsVerify bool
	var timestamp timestampFlag
	var sets []string
	var platform string
	var tracePath string
	var session *traceSession // of the command's trace, if --trace is given

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
	root.PersistentFlags().StringVarP(&dir, "dir", "C", ".",
		"directory of the BUILD file whose targets are named without a label, such as //dir:name")
	root.PersistentFlags().BoolVar(&tlsVerify, "tls-verify", true,
		"require HTTPS and verify certificates when contacting registries")
	root.PersistentFlags().Var(&timestamp, "timestamp",
		"stamp the files of targets that run no earlier than this time, in Unix seconds or RFC 3339")
	root.PersistentFlags().StringArrayVar(&sets, "set", nil,
		"give a parameter of the named target a value, as `name=value`; separate a list's strings with commas. "+
			"build gives it to each named target that has the parameter")
	root.PersistentFlags().StringVar(&platform, "platform", "",
		"build for this `os/arch`, such as linux/arm64, rather than the host's")

	// request returns the request to load the graph of the targets that names name, for want.
	request := func(names []string, want load.Want, execPlatform imgspecv1.Platform) load.Request {
		return load.Request{
			Dir:          dir,
			Targets:      names,
			Want:         want,
			Sets:         sets,
			Platform:     platform,
			ExecPlatform: execPlatform,
		}
	}

	// runGraph loads the graph that req asks for, stores it, and the files of its globs, in the build cache, and has
	// the executor run it, printing its progress, with rebuild for the targets named. It returns the build cache, in
	// which the executor leaves tasks' files.
	runGraph := func(cmd *cobra.Command, ex run.Executor, req load.Request, rebuild bool, events *eventPrinter) (
		[]run.Result, *cache.Cache, error) {
		// The build cache is opened only once the targets are configured, so that a mistake in a BUILD file touches
		// nothing.
		var c *cache.Cache
		openBuildCache := func() (*cache.Cache, error) {
			if c != nil {
				return c, nil
			}
			dir, err := run.CacheDir()
			if err != nil {
				return nil, err
			}
			c, err = cache.Open(dir)
			return c, err
		}
		req.Store = func(fsys afero.Fs, dir string, sel *fileset.Selection) (cache.Blob, fileset.Stats, error) {
			c, err := openBuildCache()
			if err != nil {
				return cache.Blob{}, fileset.Stats{}, err
			}
			return fileset.Store(c, fsys, dir, sel)
		}
		ctx := cmd.Context()
		g, err := load.Load(ctx, fsys, req)
		if err != nil {
			return nil, nil, err
		}
		c, err = openBuildCache()
		if err != nil {
			return nil, nil, err
		}
		_, span := tracing.Start(ctx, "store graph")
		stored, err := c.PutBytes(graph.Encode(g))
		span.Set("graph", stored.Digest.String(), "bytes", stored.Size)
		span.EndWith(err)
		if err != nil {
			return nil, nil, fmt.Errorf("saving to the build cache: %w", err)
		}
		runReq := run.Request{Graph: stored, Rebuild: rebuild, Timestamp: time.Time(timestamp)}
		events.graph = g
		var runSpan *tracing.Active
		if session != nil {
			// Place the executor's spans by this clock, which the executor's may not agree with.
			offset, clockErr := tracing.EstimateOffset(ctx, time.Now, ex.Clock)
			if clockErr != nil {
				return nil, nil, clockErr
			}
			session.setOffset(offset)
			ctx, runSpan = tracing.Start(ctx, "run",
				"clock offset", offset.Offset.String(), "clock uncertainty", offset.Uncertainty.String())
			runReq.Trace = tracing.Call(ctx)
		}
		results, err := ex.Run(ctx, runReq, events)
		runSpan.EndWith(err)
		return results, c, err
	}

	// traced runs the command cmd, given args, with run, tracing it to the file that --trace names, if given, as
	// well as the executor's part, which it sends to the command's events. output is the command's own output,
	// a folder if outputDir, which the trace mustn't overwrite, or "" if it writes no file.
	traced := func(cmd *cobra.Command, args []string, output string, outputDir bool, run func() error) error {
		if tracePath == "" {
			return run()
		}
		var err error
		if session, err = newTraceSession(tracePath, output, outputDir); err != nil {
			return err
		}
		ctx := tracing.WithRecorder(cmd.Context(), session.recorder, "")
		command := strings.Join(append([]string{cmd.CommandPath()}, args...), " ")
		ctx, span := tracing.StartTrack(ctx, clientProcess, command)
		cmd.SetContext(ctx)
		err = run()
		span.EndWith(err)
		return session.finishTrace(err, tracePath, cmd.ErrOrStderr())
	}
	// newEvents returns what prints a run's progress, and passes the executor's spans on to the trace.
	newEvents := func(cmd *cobra.Command) *eventPrinter {
		return &eventPrinter{stdout: cmd.OutOrStdout(), stderr: cmd.ErrOrStderr(), trace: session}
	}
	// addTraceFlag adds --trace to cmd, a command that builds.
	addTraceFlag := func(cmd *cobra.Command) {
		cmd.Flags().StringVar(&tracePath, "trace", "",
			"write a trace of the command, which Perfetto (ui.perfetto.dev) opens, to `file.json`")
	}

	// executor returns the executor to build with, which the caller must close.
	executor := func(cmd *cobra.Command) (*run.Local, *types.SystemContext, error) {
		sys, err := newSystemContext(cmd, tlsVerify)
		if err != nil {
			return nil, nil, err
		}
		ex, err := run.NewLocal(sys)
		return ex, sys, err
	}

	// buildImage builds the image of the target named target and sends it to dest.
	buildImage := func(cmd *cobra.Command, target string, dest destination) error {
		ex, sys, err := executor(cmd)
		if err != nil {
			return err
		}
		defer ex.Close()
		execPlatform, err := ex.ExecPlatform(cmd.Context())
		if err != nil {
			return err
		}
		req := request([]string{target}, load.WantImage, execPlatform)
		if dest.platform != nil {
			req.DestPlatform = func(ctx context.Context) (imgspecv1.Platform, string, error) {
				p, why, err := dest.platform(ctx)
				if err != nil {
					return p, why, fmt.Errorf("%s: %w", dest.action, err)
				}
				return p, why, nil
			}
		}
		events := newEvents(cmd)
		results, _, err := runGraph(cmd, ex, req, false, events)
		if err != nil {
			return err
		}
		res := results[0]
		ctx, span := tracing.Start(cmd.Context(), dest.action)
		imageId, err := dest.send(ctx, ex, res, sys, events)
		span.Set("result", imageId)
		span.EndWith(err)
		if err != nil {
			return fmt.Errorf("%s: %w", dest.action, err)
		}
		if res.Index != nil {
			// An index has no image ID: name it by its manifest's digest, which is what registries show.
			fmt.Fprintf(cmd.OutOrStdout(), "Index built! %s %s\n", dest.label, imageId)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "Image built! %s %s\n", dest.label, imageId)
		}
		return nil
	}

	// build builds the image of the target that args name first and sends it to dest, tracing it if asked.
	build := func(cmd *cobra.Command, args []string, dest destination) error {
		return traced(cmd, args, dest.output, false, func() error {
			return buildImage(cmd, args[0], dest)
		})
	}

	pushFrom := pushFromFlag(finish.FromExecutor)
	pushCmd := &cobra.Command{
		Use:     "push <target> <image>",
		Short:   "Build a target's image and push it to a registry",
		Example: "  vamos push server ghcr.io/me/app:1.0\n  vamos push //ui:site ghcr.io/me/site:1.0",
		Long: "Build a target's image, or an index of images, and push it to a registry. The executor, which\n" +
			"builds the image, pushes it with its own credentials and registry settings, unless --push-from=client\n" +
			"is given: then vamos pushes it with yours.",
		Args: targetAnd("image name", "vamos push server ghcr.io/me/app:1.0"),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := parseImageName(args[1])
			if err != nil {
				return err
			}
			label := reference.FamiliarString(name)
			return build(cmd, args, destination{
				send: func(
					ctx context.Context, ex run.Executor, res run.Result, sys *types.SystemContext, ev run.Events,
				) (string, error) {
					return finish.Push(ctx, ex, res, name, finish.PushFrom(pushFrom), sys, ev)
				},
				action: "pushing to " + label,
				label:  label,
			})
		},
	}

	loadCmd := &cobra.Command{
		Use:     "load <target> <image>",
		Short:   "Build a target's image and load it into the local Docker daemon",
		Long:    "Build a target's image and load it into the Docker daemon at $DOCKER_HOST, or the local socket.",
		Example: "  vamos load server app:dev",
		Args:    targetAnd("image name", "vamos load server app:dev"),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := parseImageName(args[1])
			if err != nil {
				return err
			}
			return build(cmd, args, destination{
				send: func(
					ctx context.Context, ex run.Executor, res run.Result, sys *types.SystemContext, ev run.Events,
				) (string, error) {
					return finish.LoadDocker(ctx, ex, res, name, sys, ev)
				},
				platform: finish.DockerPlatform,
				action:   "loading into Docker",
				label:    reference.FamiliarString(name),
			})
		},
	}

	// saveCommand returns the command use, which writes a target's image to a tarball.
	saveCommand := func(use string) *cobra.Command {
		var tag string
		save := &cobra.Command{
			Use:     use + " <target> <file.tar>",
			Short:   "Build a target's image and save it to a tarball that `docker load` can import",
			Example: "  vamos " + use + " server app.tar --tag app:dev",
			Args:    targetAnd("output file", "vamos "+use+" server app.tar"),
			RunE: func(cmd *cobra.Command, args []string) error {
				path, err := filepath.Abs(args[1])
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
				if err := finish.CheckSaveDest(path); err != nil {
					return err
				}
				return build(cmd, args, destination{
					send: func(
						ctx context.Context, ex run.Executor, res run.Result, sys *types.SystemContext, ev run.Events,
					) (string, error) {
						return finish.Save(ctx, ex, res, path, name, sys, ev)
					},
					output:   path,
					platform: finish.SavePlatform,
					action:   "writing " + args[1],
					label:    args[1],
				})
			},
		}
		save.Flags().StringVarP(&tag, "tag", "t", "", "image name to record in the tarball")
		return save
	}
	saveCmd := saveCommand("save")
	// write is save's earlier name, kept for a release. Cobra hides it, and warns that it is deprecated.
	writeCmd := saveCommand("write")
	writeCmd.Deprecated = `use "vamos save"`

	var replace bool
	// exportFiles builds the task that args name first and writes its files to dir, as action describes.
	exportFiles := func(cmd *cobra.Command, args []string, dir, action string) error {
		ex, _, err := executor(cmd)
		if err != nil {
			return err
		}
		defer ex.Close()
		execPlatform, err := ex.ExecPlatform(cmd.Context())
		if err != nil {
			return err
		}
		results, c, err := runGraph(cmd, ex, request(args[:1], load.WantFiles, execPlatform), false,
			newEvents(cmd))
		if err != nil {
			return err
		}
		res := results[0]
		warn := func(err error) { fmt.Fprintf(cmd.ErrOrStderr(), "vamos: warning: %v\n", err) }
		_, span := tracing.Start(cmd.Context(), action, "files", res.Files.Digest.String())
		err = finish.Export(cmd.Context(), c, res, dir, replace, warn)
		span.EndWith(err)
		if err != nil {
			return fmt.Errorf("%s: %w", action, err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Files exported! %s %s\n", args[1], res.Files.Digest)
		return nil
	}
	exportCmd := &cobra.Command{
		Use:   "export <task> <folder>",
		Short: "Build a task and write its files to a folder",
		Long: "Build a task that is given source, and write the files it keeps to a folder, at their paths within\n" +
			"source. The folder is replaced as a whole, never merged into: it ends up holding the task's files\n" +
			"alone. A folder that holds anything is replaced only with --replace. The files belong to the folder's\n" +
			"parent's owner, and are stamped with the task's time, as an image's are.",
		Example: "  vamos export codegen src/generated --replace",
		Args:    targetAnd("folder", "vamos export codegen out"),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := filepath.Abs(args[1])
			if err != nil {
				return err
			}
			action := "exporting to " + args[1]
			// Report a folder that can't be written now rather than after building.
			if err := finish.CheckExportDest(dir, replace); err != nil {
				return fmt.Errorf("%s: %w", action, err)
			}
			return traced(cmd, args, dir, true, func() error { return exportFiles(cmd, args, dir, action) })
		},
	}
	exportCmd.Flags().BoolVar(&replace, "replace", false, "replace the folder if it holds anything")

	var rebuild bool
	buildCmd := &cobra.Command{
		Use:   "build <target>...",
		Short: "Build targets into the build cache",
		Long: "Build the named targets, and the targets they use, into the build cache, where\n" +
			"push, load, save and export find them. Targets already cached are reused, unless --rebuild is given:\n" +
			"then the named targets run again, though the targets they use are still reused. A pull() named\n" +
			"with --rebuild is pulled again, in case its tag names a newer image.",
		Example: "  vamos build packages //backend:test\n  vamos build compile --rebuild",
		Args:    atLeastOne("target name", "vamos build packages"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return traced(cmd, args, "", false, func() error {
				ex, _, err := executor(cmd)
				if err != nil {
					return err
				}
				defer ex.Close()
				execPlatform, err := ex.ExecPlatform(cmd.Context())
				if err != nil {
					return err
				}
				_, _, err = runGraph(cmd, ex, request(args, load.WantBuild, execPlatform), rebuild, newEvents(cmd))
				return err
			})
		},
	}
	buildCmd.Flags().BoolVar(&rebuild, "rebuild", false,
		"run the named targets again rather than reuse cached results")

	pruneCmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove everything from the build cache",
		Long: "Remove the cached results of every target: the images of cached layers, from the container\n" +
			"store, and the build cache beside it. Pulled images are kept, with what their tags named.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run.Prune(cmd.OutOrStdout())
		},
	}

	graphCmd := &cobra.Command{
		Use:   "graph <target>...",
		Short: "Print the build graph of targets, as JSON, without building them",
		Long: "Print the graph that building the named targets would run, as JSON, without building them or\n" +
			"storing anything. A glob's files are read, and their tree's digest given, as a build would.",
		Hidden: true,
		Args:   atLeastOne("target name", "vamos graph packages"),
		RunE: func(cmd *cobra.Command, args []string) error {
			// The local executor's platform, without opening one, since nothing is built or pulled.
			req := request(args, load.WantBuild, run.HostPlatform)
			req.Store = fileset.Hash
			g, err := load.Load(cmd.Context(), fsys, req)
			if err != nil {
				return err
			}
			data, err := json.MarshalIndent(g, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n", data)
			return nil
		},
	}

	pushCmd.Flags().Var(&pushFrom, "push-from", "which side pushes the image, with its credentials: executor or client")

	for _, cmd := range []*cobra.Command{buildCmd, pushCmd, loadCmd, saveCmd, writeCmd, exportCmd} {
		addTraceFlag(cmd)
	}

	root.AddCommand(buildCmd, pushCmd, loadCmd, saveCmd, writeCmd, exportCmd, pruneCmd, graphCmd)
	return root
}

// eventPrinter prints the progress of a run: each status as a line naming its target, and what targets print, to
// stdout and stderr. It passes the executor's spans on to the command's trace, if it has one.
type eventPrinter struct {
	mu             sync.Mutex
	graph          *graph.Graph
	stdout, stderr io.Writer
	trace          *traceSession
}

func (p *eventPrinter) Spans(spans []tracing.Span) {
	if p.trace != nil {
		p.trace.addRemote(spans)
	}
}

func (p *eventPrinter) Status(node graph.Ref, status string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(p.stdout, "%s: %s\n", p.graph.Node(node).Desc, status)
}

func (p *eventPrinter) Output(node graph.Ref, stream byte, data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if stream == cache.StreamStderr {
		p.stderr.Write(data)
	} else {
		p.stdout.Write(data)
	}
}

// destination says where build sends an image.
type destination struct {
	// send sends res, once it is built, to the destination, returning its image ID there, or an index's digest.
	// ev receives the spans of the executor's part, if traced.
	send func(
		ctx context.Context, ex run.Executor, res run.Result, sys *types.SystemContext, ev run.Events,
	) (string, error)
	// platform, if not nil, says the destination takes one image, and for which platform, with why, so that
	// an index sends only its manifest for that platform. Otherwise the destination takes the whole index.
	platform func(ctx context.Context) (imgspecv1.Platform, string, error)

	action string // describes sending the image, e.g. "pushing to app:latest"
	label  string // names the destination after "Image built!"
	output string // the file it writes on the client, if any, which a trace mustn't overwrite
}

// targetAnd requires a target and one more positional argument, described by what and example.
func targetAnd(what, example string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		switch {
		case len(args) == 0:
			return fmt.Errorf("missing target and %s, e.g. %s", what, example)
		case len(args) == 1:
			return fmt.Errorf("missing %s after target %q, e.g. %s", what, args[0], example)
		case len(args) > 2:
			return fmt.Errorf("expected a target and one %s, got %d arguments: %s",
				what, len(args), strings.Join(args, " "))
		}
		return nil
	}
}

// atLeastOne requires one or more positional arguments, described by what and example.
func atLeastOne(what, example string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return fmt.Errorf("missing %s, e.g. %s", what, example)
		}
		return nil
	}
}

// noArgs refuses positional arguments.
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("%s takes no arguments, got: %s", cmd.CommandPath(), strings.Join(args, " "))
	}
	return nil
}

// pushFromFlag is the value of --push-from: which side pushes an image.
type pushFromFlag finish.PushFrom

func (f *pushFromFlag) String() string { return string(*f) }

// Set accepts "executor" or "client".
func (f *pushFromFlag) Set(value string) error {
	switch finish.PushFrom(value) {
	case finish.FromExecutor, finish.FromClient:
		*f = pushFromFlag(value)
		return nil
	}
	return fmt.Errorf(`want %q or %q`, finish.FromExecutor, finish.FromClient)
}

// Type names the kind of value the flag takes, for help.
func (f *pushFromFlag) Type() string { return "side" }

// timestampFlag is the value of --timestamp: a time given in Unix seconds or in RFC 3339 form.
type timestampFlag time.Time

// String returns the time given, or "" if none was.
func (f *timestampFlag) String() string {
	if time.Time(*f).IsZero() {
		return ""
	}
	return time.Time(*f).UTC().Format(time.RFC3339)
}

// Set parses value as Unix seconds or an RFC 3339 time, no earlier than 1970.
func (f *timestampFlag) Set(value string) error {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		seconds, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr != nil {
			return errors.New("want Unix seconds, such as 1767225600, or a time such as 2026-01-01T00:00:00Z")
		}
		t = time.Unix(seconds, 0)
	}
	if t.Before(time.Unix(0, 0)) {
		return errors.New("must not be before 1970")
	}
	*f = timestampFlag(t)
	return nil
}

// Type names the kind of value the flag takes, for help.
func (f *timestampFlag) Type() string {
	return "time"
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
