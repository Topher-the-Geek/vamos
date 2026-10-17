package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"
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
	"go.podman.io/storage"
)

// Config says what the command line works on, beyond what its flags say.
type Config struct {
	Fs afero.Fs // BUILD files and their sources are read from it
	// Store, if set, is the container store that the local executor builds in, with the build cache beside it,
	// and must give its graph root and run root as absolute paths;
	// otherwise it is the one that buildah and podman use here.
	// Tests set it, so that they never touch the developer's.
	Store *storage.StoreOptions
}

// app is the vamos command line: what every invocation shares.
// Each command is a method of invocation, which holds the state of one run, so that a handler can be tested
// without constructing the root command.
type app struct {
	fs    afero.Fs
	store *storage.StoreOptions
}

// options are the flags of one invocation; each command reads the ones it takes.
// defaultOptions gives their defaults, which the root command's flags show.
type options struct {
	dir       string        // -C
	tlsVerify tlsVerifyFlag // --tls-verify, which applies to the client's own registry access
	timestamp timestampFlag // --timestamp
	sets      []string      // --set
	platform  string        // --platform
	trace     string        // --trace
	rebuild   bool          // build --rebuild
	replace   bool          // export --replace
	tag       string        // save --tag
	pushFrom  pushFromFlag  // push --push-from
}

// defaultOptions returns the options as they are before any flag is given.
func defaultOptions() options {
	return options{dir: ".", pushFrom: pushFromFlag(finish.FromExecutor)}
}

// invocation is one run of a command: its context, its writers, its options and, if --trace is given,
// its trace session, which the executor's spans join.
type invocation struct {
	app            *app
	ctx            context.Context
	stdout, stderr io.Writer
	opts           options
	command        string        // as the trace names it: "vamos build app"
	session        *traceSession // of the trace, if --trace is given, once started
}

// NewRootCommand returns the vamos command line, with its subcommands, as cfg says.
func NewRootCommand(cfg Config) *cobra.Command {
	a := &app{fs: cfg.Fs, store: cfg.Store}
	defaults := defaultOptions()
	opts := &defaults

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
	root.PersistentFlags().StringVarP(&opts.dir, "dir", "C", opts.dir,
		"directory of the BUILD file whose targets are named without a label, such as //dir:name")
	root.PersistentFlags().Var(&opts.tlsVerify, "tls-verify",
		"require HTTPS and verify certificates when vamos itself contacts a registry, as --push-from=client does; "+
			"the executor follows its registries.conf")
	root.PersistentFlags().Lookup("tls-verify").NoOptDefVal = "true"
	root.PersistentFlags().Var(&opts.timestamp, "timestamp",
		"stamp the files of targets that run no earlier than this time, in Unix seconds or RFC 3339")
	root.PersistentFlags().StringArrayVar(&opts.sets, "set", nil,
		"give a parameter of the named target a value, as `name=value`; separate a list's strings with commas. "+
			"build gives it to each named target that has the parameter")
	root.PersistentFlags().StringVar(&opts.platform, "platform", "",
		"build for this `os/arch`, such as linux/arm64, rather than the host's")

	// handle returns a RunE that runs the command through an invocation made from cmd and opts.
	handle := func(h func(inv *invocation, args []string) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			inv := &invocation{
				app:     a,
				ctx:     cmd.Context(),
				stdout:  cmd.OutOrStdout(),
				stderr:  cmd.ErrOrStderr(),
				opts:    *opts,
				command: strings.Join(append([]string{cmd.CommandPath()}, args...), " "),
			}
			return h(inv, args)
		}
	}

	pushCmd := &cobra.Command{
		Use:     "push <target> <image>",
		Short:   "Build a target's image and push it to a registry",
		Example: "  vamos push server ghcr.io/me/app:1.0\n  vamos push //ui:site ghcr.io/me/site:1.0",
		Long: "Build a target's image, or an index of images, and push it to a registry. The executor, which\n" +
			"builds the image, pushes it with its own credentials and registry settings, unless --push-from=client\n" +
			"is given: then vamos pushes it with yours.",
		Args: targetAnd("image name", "vamos push server ghcr.io/me/app:1.0"),
		RunE: handle(func(inv *invocation, args []string) error { return inv.push(args[0], args[1]) }),
	}
	pushCmd.Flags().Var(&opts.pushFrom, "push-from",
		"which side pushes the image, with its credentials: executor or client")

	loadCmd := &cobra.Command{
		Use:     "load <target> <image>",
		Short:   "Build a target's image and load it into the local Docker daemon",
		Long:    "Build a target's image and load it into the Docker daemon at $DOCKER_HOST, or the local socket.",
		Example: "  vamos load server app:dev",
		Args:    targetAnd("image name", "vamos load server app:dev"),
		RunE:    handle(func(inv *invocation, args []string) error { return inv.load(args[0], args[1]) }),
	}

	// saveCommand returns the command use, which writes a target's image to a tarball.
	saveCommand := func(use string) *cobra.Command {
		save := &cobra.Command{
			Use:     use + " <target> <file.tar>",
			Short:   "Build a target's image and save it to a tarball that `docker load` can import",
			Example: "  vamos " + use + " server app.tar --tag app:dev",
			Args:    targetAnd("output file", "vamos "+use+" server app.tar"),
			RunE:    handle(func(inv *invocation, args []string) error { return inv.save(args[0], args[1]) }),
		}
		save.Flags().StringVarP(&opts.tag, "tag", "t", "", "image name to record in the tarball")
		return save
	}
	saveCmd := saveCommand("save")
	// write is save's earlier name, kept for a release.
	// Cobra hides it, and warns that it is deprecated.
	writeCmd := saveCommand("write")
	writeCmd.Deprecated = `use "vamos save"`

	exportCmd := &cobra.Command{
		Use:   "export <task> <folder>",
		Short: "Build a task and write its files to a folder",
		Long: "Build a task that is given source, and write the files it keeps to a folder, at their paths within\n" +
			"source. The folder is replaced as a whole, never merged into: it ends up holding the task's files\n" +
			"alone. A folder that holds anything is replaced only with --replace. The files belong to the folder's\n" +
			"parent's owner, and are stamped with the task's time, as an image's are.",
		Example: "  vamos export codegen src/generated --replace",
		Args:    targetAnd("folder", "vamos export codegen out"),
		RunE:    handle(func(inv *invocation, args []string) error { return inv.export(args[0], args[1]) }),
	}
	exportCmd.Flags().BoolVar(&opts.replace, "replace", false, "replace the folder if it holds anything")

	buildCmd := &cobra.Command{
		Use:   "build <target>...",
		Short: "Build targets into the build cache",
		Long: "Build the named targets, and the targets they use, into the build cache, where\n" +
			"push, load, save and export find them. Targets already cached are reused, unless --rebuild is given:\n" +
			"then the named targets run again, though the targets they use are still reused. A pull() named\n" +
			"with --rebuild is pulled again, in case its tag names a newer image.",
		Example: "  vamos build packages //backend:test\n  vamos build compile --rebuild",
		Args:    atLeastOne("target name", "vamos build packages"),
		RunE:    handle(func(inv *invocation, args []string) error { return inv.build(args) }),
	}
	buildCmd.Flags().BoolVar(&opts.rebuild, "rebuild", false,
		"run the named targets again rather than reuse cached results")

	pruneCmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove everything from the build cache",
		Long: "Remove the cached results of every target: the images of cached layers, from the container\n" +
			"store, and the build cache beside it. Pulled images are kept, with what their tags named.",
		Args: noArgs,
		RunE: handle(func(inv *invocation, args []string) error { return inv.prune() }),
	}

	graphCmd := &cobra.Command{
		Use:   "graph <target>...",
		Short: "Print the build graph of targets, as JSON, without building them",
		Long: "Print the graph that building the named targets would run, as JSON, without building them or\n" +
			"storing anything. A glob's files are read, and their tree's digest given, as a build would.",
		Hidden: true,
		Args:   atLeastOne("target name", "vamos graph packages"),
		RunE:   handle(func(inv *invocation, args []string) error { return inv.graph(args) }),
	}

	for _, cmd := range []*cobra.Command{buildCmd, pushCmd, loadCmd, saveCmd, writeCmd, exportCmd} {
		cmd.Flags().StringVar(&opts.trace, "trace", "",
			"write a trace of the command, which Perfetto (ui.perfetto.dev) opens, to `file.json`")
	}

	root.AddCommand(buildCmd, pushCmd, loadCmd, saveCmd, writeCmd, exportCmd, pruneCmd, graphCmd)
	return root
}

// printf writes to the command's output, returning a failure to write, which the command reports.
func (inv *invocation) printf(format string, args ...any) error {
	_, err := fmt.Fprintf(inv.stdout, format, args...)
	return err
}

// warn writes a warning to the command's error output; a failure to write a warning is itself ignored.
func (inv *invocation) warn(err error) {
	fmt.Fprintf(inv.stderr, "vamos: warning: %v\n", err)
}

// request returns the request to load the graph of the targets names, for want, configured for an executor
// on execPlatform.
func (inv *invocation) request(names []string, want load.Want, execPlatform imgspecv1.Platform) load.Request {
	return load.Request{
		Dir:          inv.opts.dir,
		Targets:      names,
		Want:         want,
		Sets:         inv.opts.sets,
		Platform:     inv.opts.platform,
		ExecPlatform: execPlatform,
	}
}

// storeOptions returns the container store to build in.
func (inv *invocation) storeOptions() (storage.StoreOptions, error) {
	if store := inv.app.store; store != nil {
		// The build cache's place is derived from the graph root, and containers/storage fills in
		// missing roots with the default store's, so a store given in part would reach the developer's.
		if !filepath.IsAbs(store.GraphRoot) || !filepath.IsAbs(store.RunRoot) {
			return storage.StoreOptions{}, fmt.Errorf(
				"the container store's graph root %q and run root %q must both be absolute paths",
				store.GraphRoot, store.RunRoot)
		}
		return *store, nil
	}
	return run.DefaultStoreOptions()
}

// executor returns the executor to build with, which the caller must close with closeExecutor,
// and the client's own registry settings, for finish; the executor builds its own from its environment.
func (inv *invocation) executor() (*run.Local, *types.SystemContext, error) {
	sys, err := inv.systemContext()
	if err != nil {
		return nil, nil, err
	}
	options, err := inv.storeOptions()
	if err != nil {
		return nil, nil, err
	}
	ex, err := run.NewLocal(options)
	if err != nil {
		return nil, nil, err
	}
	return ex, sys, nil
}

// closeExecutor shuts ex down, warning about what failed: the command's own outcome stands.
func (inv *invocation) closeExecutor(ex *run.Local) {
	if err := ex.Close(); err != nil {
		inv.warn(fmt.Errorf("shutting the executor down: %w", err))
	}
}

// systemContext returns the client's settings for reaching registries, for a push from the client,
// and the Docker daemon: the environment's, as the executor builds too, and --tls-verify, which is the client's alone.
func (inv *invocation) systemContext() (*types.SystemContext, error) {
	sys, err := run.SystemContext()
	if err != nil {
		return nil, err
	}
	// Unless --tls-verify is given, leave it to registries.conf, so that "insecure = true" entries there still apply.
	if inv.opts.tlsVerify.set {
		sys.DockerInsecureSkipTLSVerify = types.NewOptionalBool(!inv.opts.tlsVerify.verify)
	}
	return sys, nil
}

// events returns what prints a run's progress, and passes the executor's spans on to the trace.
func (inv *invocation) events() *eventPrinter {
	return &eventPrinter{stdout: inv.stdout, stderr: inv.stderr, trace: inv.session}
}

// traced runs the command with run, tracing it to the file that --trace names, if given,
// as well as the executor's part, which comes as events.
// output is the command's own output, a folder if outputDir,
// which the trace mustn't overwrite, or "" if it writes no file.
func (inv *invocation) traced(output string, outputDir bool, run func() error) error {
	if inv.opts.trace == "" {
		return run()
	}
	session, err := newTraceSession(inv.opts.trace, output, outputDir)
	if err != nil {
		return err
	}
	inv.session = session
	ctx := tracing.WithRecorder(inv.ctx, session.recorder, "")
	command := inv.command
	if command == "" {
		command = clientProcess // a handler called without the root command, as a test does
	}
	ctx, span := tracing.StartTrack(ctx, clientProcess, command)
	inv.ctx = ctx
	err = run()
	span.EndWith(err)
	return session.finishTrace(err, inv.opts.trace, inv.stderr)
}

// runGraph loads the graph that req asks for, stores it, and the files of its globs, in the content store
// that content opens, and has the executor run it as a job, printing its progress through events,
// with rebuild for the targets named.
// It returns the content store, in which the executor leaves tasks' files.
func (inv *invocation) runGraph(
	ex api.Executor, content func() (cache.ContentStore, error), req load.Request, rebuild bool,
	events *eventPrinter,
) ([]api.Result, cache.ContentStore, error) {
	// The content store is opened only once the targets are configured,
	// so that a mistake in a BUILD file touches nothing.
	var c cache.ContentStore
	openContent := func() (cache.ContentStore, error) {
		if c != nil {
			return c, nil
		}
		var err error
		c, err = content()
		return c, err
	}
	req.Store = func(ctx context.Context, fsys afero.Fs, dir string, sel *fileset.Selection) (
		cache.Blob, fileset.Stats, error) {
		c, err := openContent()
		if err != nil {
			return cache.Blob{}, fileset.Stats{}, err
		}
		return fileset.Store(ctx, c, fsys, dir, sel)
	}
	ctx := inv.ctx
	g, err := load.Load(ctx, inv.app.fs, req)
	if err != nil {
		return nil, nil, err
	}
	c, err = openContent()
	if err != nil {
		return nil, nil, err
	}
	_, span := tracing.Start(ctx, "store graph")
	stored, err := c.PutBytes(ctx, graph.Encode(g))
	span.Set("graph", stored.Digest.String(), "bytes", stored.Size)
	span.EndWith(err)
	if err != nil {
		return nil, nil, fmt.Errorf("saving to the build cache: %w", err)
	}
	execPlatform, err := graph.ParsePlatform(g.ExecPlatform)
	if err != nil {
		return nil, nil, err
	}
	runReq := api.GraphRequest{
		Graph:     stored,
		Version:   g.Version,
		Platform:  execPlatform,
		Rebuild:   rebuild,
		Timestamp: time.Time(inv.opts.timestamp),
	}
	events.graph = g
	var runSpan *tracing.Active
	if inv.session != nil {
		// Place the executor's spans by this clock, which the executor's may not agree with.
		offset, clockErr := tracing.EstimateOffset(ctx, time.Now, func(ctx context.Context) (time.Time, error) {
			caps, err := ex.Capabilities(ctx)
			return caps.Clock, err
		})
		if clockErr != nil {
			return nil, nil, clockErr
		}
		inv.session.setOffset(offset)
		ctx, runSpan = tracing.Start(ctx, "run",
			"clock offset", offset.Offset.String(), "clock uncertainty", offset.Uncertainty.String())
		runReq.Trace = tracing.Call(ctx)
	}
	id, err := ex.Submit(ctx, api.Request{Graph: &runReq})
	if err != nil {
		runSpan.EndWith(err)
		return nil, nil, err
	}
	outcome, err := watchJob(ctx, ex, id, events)
	runSpan.EndWith(err)
	return outcome.Results, c, err
}

// deliver has the executor export res to the content store, and returns what it exported there.
func (inv *invocation) deliver(
	ctx context.Context, ex api.Executor, res api.Result, events *eventPrinter,
) (api.Exported, error) {
	id, err := ex.Submit(ctx, api.Request{Export: &api.ExportRequest{Result: res, Trace: tracing.Call(ctx)}})
	if err != nil {
		return api.Exported{}, err
	}
	outcome, err := watchJob(ctx, ex, id, events)
	if err != nil {
		return api.Exported{}, err
	}
	if outcome.Exported == nil {
		return api.Exported{}, errors.New("the executor exported nothing")
	}
	return *outcome.Exported, nil
}

// buildImage builds the image of the target named target and sends it to dest, tracing it if asked.
func (inv *invocation) buildImage(target string, dest destination) error {
	return inv.traced(dest.output, false, func() error {
		ex, sys, err := inv.executor()
		if err != nil {
			return err
		}
		defer inv.closeExecutor(ex)
		caps, err := ex.Capabilities(inv.ctx)
		if err != nil {
			return err
		}
		req := inv.request([]string{target}, load.WantImage, caps.Platform)
		if dest.platform != nil {
			req.DestPlatform = func(ctx context.Context) (imgspecv1.Platform, string, error) {
				p, why, err := dest.platform(ctx)
				if err != nil {
					return p, why, fmt.Errorf("%s: %w", dest.action, err)
				}
				return p, why, nil
			}
		}
		events := inv.events()
		results, content, err := inv.runGraph(ex, ex.ContentStore, req, false, events)
		if err != nil {
			return err
		}
		res := results[0]
		ctx, span := tracing.Start(inv.ctx, dest.action)
		var imageId string
		if dest.push != "" {
			// The executor pushes, with its own credentials and registry settings.
			var id api.JobID
			id, err = ex.Submit(ctx, api.Request{Push: &api.PushRequest{
				Result: res, Destination: dest.push, Trace: tracing.Call(ctx),
			}})
			var outcome api.Outcome
			if err == nil {
				outcome, err = watchJob(ctx, ex, id, events)
			}
			if err == nil && outcome.Pushed != nil {
				imageId = outcome.Pushed.Image
				if outcome.Pushed.Index != "" {
					imageId = outcome.Pushed.Index
				}
			}
		} else {
			var exp api.Exported
			if exp, err = inv.deliver(ctx, ex, res, events); err == nil {
				imageId, err = dest.send(ctx, content, exp, sys)
			}
		}
		span.Set("result", imageId)
		span.EndWith(err)
		if err != nil {
			return fmt.Errorf("%s: %w", dest.action, err)
		}
		if res.Index != nil {
			// An index has no image ID: name it by its manifest's digest, which is what registries show.
			return inv.printf("Index built! %s %s\n", dest.label, imageId)
		}
		return inv.printf("Image built! %s %s\n", dest.label, imageId)
	})
}

// push builds target's image and pushes it to the registry image names, from the side --push-from says.
func (inv *invocation) push(target, image string) error {
	name, err := parseImageName(image)
	if err != nil {
		return err
	}
	label := reference.FamiliarString(name)
	dest := destination{
		send: func(
			ctx context.Context, content cache.ContentStore, exp api.Exported, sys *types.SystemContext,
		) (string, error) {
			return finish.Push(ctx, content, exp, name, sys)
		},
		action: "pushing to " + label,
		label:  label,
	}
	if finish.PushFrom(inv.opts.pushFrom) == finish.FromExecutor {
		dest.push = name.String() // the full reference, so that the executor means the same registry
	}
	return inv.buildImage(target, dest)
}

// load builds target's image and loads it into the Docker daemon, named image.
func (inv *invocation) load(target, image string) error {
	name, err := parseImageName(image)
	if err != nil {
		return err
	}
	return inv.buildImage(target, destination{
		send: func(
			ctx context.Context, content cache.ContentStore, exp api.Exported, sys *types.SystemContext,
		) (string, error) {
			return finish.LoadDocker(ctx, content, exp, name, sys)
		},
		platform: finish.DockerPlatform,
		action:   "loading into Docker",
		label:    reference.FamiliarString(name),
	})
}

// save builds target's image and writes it to a tarball at file, recording --tag in it if given.
func (inv *invocation) save(target, file string) error {
	path, err := filepath.Abs(file)
	if err != nil {
		return err
	}
	var name reference.NamedTagged
	if inv.opts.tag != "" {
		if name, err = parseImageName(inv.opts.tag); err != nil {
			return err
		}
	}
	// Report an unwritable path now rather than after building.
	if err := finish.CheckSaveDest(path); err != nil {
		return err
	}
	return inv.buildImage(target, destination{
		send: func(
			ctx context.Context, content cache.ContentStore, exp api.Exported, sys *types.SystemContext,
		) (string, error) {
			return finish.Save(ctx, content, exp, path, name, sys)
		},
		output:   path,
		platform: finish.SavePlatform,
		action:   "writing " + file,
		label:    file,
	})
}

// export builds the task named task and writes its files to folder, replacing it if --replace is given.
func (inv *invocation) export(task, folder string) error {
	dir, err := filepath.Abs(folder)
	if err != nil {
		return err
	}
	action := "exporting to " + folder
	// Report a folder that can't be written now rather than after building.
	if err := finish.CheckExportDest(dir, inv.opts.replace); err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	return inv.traced(dir, true, func() error {
		ex, _, err := inv.executor()
		if err != nil {
			return err
		}
		defer inv.closeExecutor(ex)
		caps, err := ex.Capabilities(inv.ctx)
		if err != nil {
			return err
		}
		events := inv.events()
		results, c, err := inv.runGraph(ex, ex.ContentStore, inv.request([]string{task}, load.WantFiles, caps.Platform),
			false, events)
		if err != nil {
			return err
		}
		res := results[0]
		ctx, span := tracing.Start(inv.ctx, action, "files", res.Files.Digest.String())
		exp, err := inv.deliver(ctx, ex, res, events)
		if err == nil {
			err = finish.Export(ctx, c, exp, dir, inv.opts.replace, inv.warn)
		}
		span.EndWith(err)
		if err != nil {
			return fmt.Errorf("%s: %w", action, err)
		}
		return inv.printf("Files exported! %s %s\n", folder, res.Files.Digest)
	})
}

// build builds the targets named into the build cache, running them again if --rebuild is given.
func (inv *invocation) build(targets []string) error {
	return inv.traced("", false, func() error {
		ex, _, err := inv.executor()
		if err != nil {
			return err
		}
		defer inv.closeExecutor(ex)
		caps, err := ex.Capabilities(inv.ctx)
		if err != nil {
			return err
		}
		_, _, err = inv.runGraph(ex, ex.ContentStore, inv.request(targets, load.WantBuild, caps.Platform),
			inv.opts.rebuild, inv.events())
		return err
	})
}

// prune removes every cached result, and reports what was removed.
func (inv *invocation) prune() error {
	ex, _, err := inv.executor()
	if err != nil {
		return err
	}
	defer inv.closeExecutor(ex)
	id, err := ex.Prune(inv.ctx)
	if err != nil {
		return err
	}
	outcome, err := watchJob(inv.ctx, ex, id, inv.events())
	if err != nil {
		return err
	}
	p := outcome.Pruned
	return inv.printf("Removed %d images from the container store, and %d results and %d files "+
		"from the build cache\n", p.Images, p.Results, p.Blobs)
}

// graph prints the graph of the targets named, as JSON, without building or storing anything.
func (inv *invocation) graph(targets []string) error {
	// The executor's platform configures the graph, as a build's would; nothing is built or pulled.
	ex, _, err := inv.executor()
	if err != nil {
		return err
	}
	defer inv.closeExecutor(ex)
	caps, err := ex.Capabilities(inv.ctx)
	if err != nil {
		return err
	}
	req := inv.request(targets, load.WantBuild, caps.Platform)
	req.Store = func(ctx context.Context, fsys afero.Fs, dir string, sel *fileset.Selection) (
		cache.Blob, fileset.Stats, error) {
		return fileset.Hash(ctx, fsys, dir, sel)
	}
	g, err := load.Load(inv.ctx, inv.app.fs, req)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	return inv.printf("%s\n", data)
}

// watchJob watches the job id on ex to its end, printing its events through events,
// and returns its outcome, read back with Result, or the error its failure reports.
// If ctx ends first, as on Ctrl-C, or the events can't be written, it cancels the job and waits for it to stop,
// so that its cleanup runs, and returns ctx's error, or the write's, unless the job had already failed on its own:
// then the job's failure is returned, joined with the write's.
func watchJob(ctx context.Context, ex api.Executor, id api.JobID, events *eventPrinter) (api.Outcome, error) {
	next := int64(0)
	var unwritten error // the first failure to write an event, after which events are only counted
	handle := func(e api.Event) error {
		if unwritten != nil {
			// Output is no longer written, but the trace still takes the job's spans and gaps.
			events.record(e)
		} else if err := events.handle(e); err != nil {
			unwritten = fmt.Errorf("writing the build's progress: %w", err)
			return unwritten
		}
		if e.Kind != api.EventGap {
			next = e.Position + 1
		}
		return nil
	}
	// A watcher that fell behind watches again from where it was, and meets a gap there.
	watch := func(ctx context.Context) error {
		for {
			err := ex.Watch(ctx, id, next, handle)
			if !errors.Is(err, api.ErrFellBehind) {
				return err
			}
		}
	}
	err := watch(ctx)
	interrupted := err != nil && ctx.Err() != nil
	if interrupted || unwritten != nil {
		// The job is stopped deliberately, and watched to its end so that its cleanup has run.
		ctx = context.WithoutCancel(ctx)
		if err := ex.Cancel(ctx, id); err != nil {
			return api.Outcome{}, errors.Join(cmp.Or(unwritten, context.Canceled), err)
		}
		err = watch(ctx)
	}
	if err != nil {
		return api.Outcome{}, errors.Join(unwritten, err)
	}
	outcome, err := ex.Result(ctx, id)
	switch {
	case err != nil:
		return api.Outcome{}, errors.Join(unwritten, err)
	case outcome.Failure != nil && !outcome.Cancelled:
		// The job's own failure is the command's error, and a failure to write its events is reported with it.
		return api.Outcome{}, errors.Join(errors.New(outcome.Failure.Describe(events.graph)), unwritten)
	case unwritten != nil:
		return api.Outcome{}, unwritten
	case interrupted:
		return api.Outcome{}, context.Canceled
	case outcome.Failure != nil:
		return api.Outcome{}, errors.New(outcome.Failure.Describe(events.graph))
	case outcome.Cancelled:
		return api.Outcome{}, errors.New("the job was cancelled")
	}
	return outcome, nil
}

// eventPrinter prints the progress of a run: each status as a line naming its target,
// and what targets print, to stdout and stderr.
// It passes the executor's spans on to the command's trace, if it has one.
// Set graph and trace before starting executor callbacks and keep them unchanged until callbacks stop.
type eventPrinter struct {
	mu             sync.Mutex
	graph          *graph.Graph
	stdout, stderr io.Writer
	trace          *traceSession
}

// record passes the event e of a job to the command's trace, if it has one, without writing anything.
func (p *eventPrinter) record(e api.Event) {
	switch e.Kind {
	case api.EventSpans:
		p.spans(e.Spans)
	case api.EventGap:
		if p.trace != nil {
			p.trace.addGap(e.Position, e.First)
		}
	}
}

// handle prints the event e of a job, returning a failure to write it.
func (p *eventPrinter) handle(e api.Event) error {
	switch e.Kind {
	case api.EventStatus:
		for _, node := range e.Nodes {
			if err := p.status(node, e.Status); err != nil {
				return err
			}
		}
	case api.EventOutput:
		for _, node := range e.Nodes {
			if err := p.output(node, e.Stream, e.Data); err != nil {
				return err
			}
		}
	case api.EventSpans:
		p.record(e)
	case api.EventGap:
		p.record(e)
		p.mu.Lock()
		defer p.mu.Unlock()
		// A warning that can't be written is dropped, as every warning is: the command's outcome matters more.
		fmt.Fprintf(p.stderr, "vamos: warning: the executor no longer had the events from %d to %d, "+
			"so some progress wasn't shown; the targets' logs are whole\n", e.Position, e.First-1)
	}
	return nil
}

// spans forwards executor spans without taking the output lock; traceSession synchronizes its own state.
func (p *eventPrinter) spans(spans []tracing.Span) {
	if p.trace != nil {
		p.trace.addRemote(spans)
	}
}

// status serializes progress with step output, even when the configured writers are not concurrency-safe.
// A job without a graph, such as a prune, names its nodes by number.
func (p *eventPrinter) status(node graph.Ref, status string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.graph == nil {
		_, err := fmt.Fprintf(p.stdout, "node %d: %s\n", node, status)
		return err
	}
	_, err := fmt.Fprintf(p.stdout, "%s: %s\n", p.graph.Node(node).Desc, status)
	return err
}

// output consumes the borrowed data before returning, under the same lock as status.
func (p *eventPrinter) output(node graph.Ref, stream byte, data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	w := p.stdout
	if stream == cache.StreamStderr {
		w = p.stderr
	}
	_, err := w.Write(data)
	return err
}

// destination says where build sends an image.
type destination struct {
	// send sends exp, the built image as the executor exported it to content, to the destination,
	// as the client's settings sys describe it, returning its image ID there, or an index's digest.
	// If push is set, a full registry reference, the executor pushes there instead, and send isn't called.
	send func(ctx context.Context, content cache.ContentStore, exp api.Exported, sys *types.SystemContext) (
		string, error)
	push string
	// platform, if not nil, says the destination takes one image, and for which platform, with why,
	// so that an index sends only its manifest for that platform.
	// Otherwise the destination takes the whole index.
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

// String returns the side.
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

// tlsVerifyFlag is the value of --tls-verify: whether the client verifies registries' certificates,
// and whether the flag was given at all, since registries.conf decides otherwise.
type tlsVerifyFlag struct {
	set    bool
	verify bool
}

// String shows the flag's default, true, in help.
func (f *tlsVerifyFlag) String() string {
	if !f.set {
		return "true"
	}
	return strconv.FormatBool(f.verify)
}

// Set parses value as a bool.
func (f *tlsVerifyFlag) Set(value string) error {
	verify, err := strconv.ParseBool(value)
	if err != nil {
		return err
	}
	f.set, f.verify = true, verify
	return nil
}

// Type names the kind of value the flag takes, for help.
func (f *tlsVerifyFlag) Type() string { return "bool" }

// IsBoolFlag lets the flag be given without a value, as a bool flag is.
func (f *tlsVerifyFlag) IsBoolFlag() bool { return true }

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

// parseImageName parses a name such as "app" or "ghcr.io/me/app:1.0", adding ":latest" when no tag is given.
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
