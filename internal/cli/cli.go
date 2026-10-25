package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/coord"
	"github.com/Topher-the-Geek/vamos/internal/fileset"
	"github.com/Topher-the-Geek/vamos/internal/finish"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/load"
	"github.com/Topher-the-Geek/vamos/internal/remote"
	"github.com/Topher-the-Geek/vamos/internal/run"
	"github.com/Topher-the-Geek/vamos/internal/tracing"
	"github.com/Topher-the-Geek/vamos/internal/worker"

	"github.com/containerd/platforms"
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
	dir           string        // -C
	tlsVerify     tlsVerifyFlag // --tls-verify, which applies to the client's own registry access
	timestamp     timestampFlag // --timestamp
	sets          []string      // --set
	secrets       secretsFlag   // --secret
	platform      string        // --platform
	trace         string        // --trace
	execPlatform  string        // --exec-platform
	emulate       bool          // --emulate
	coordinator   string        // --coordinator, or $VAMOS_COORDINATOR
	rebuild       bool          // build --rebuild
	replace       bool          // export --replace
	tag           string        // save --tag
	pushFrom      pushFromFlag  // push --push-from
	listen        string        // coordinator --listen
	grace         time.Duration // coordinator --grace
	window        time.Duration // coordinator --window
	local         bool          // coordinator --local
	cacheDir      string        // coordinator --cache-dir
	offer         []string      // coordinator --offer
	offerEmulated []string      // coordinator --offer-emulated
	defaultExec   string        // coordinator --default-exec-platform
	lease         time.Duration // coordinator --lease
	retries       int           // coordinator --retries
	bound         int           // coordinator --bound
	maxBuilds     int           // coordinator --max-builds
	cacheRepo     string        // coordinator --cache-repository
	pullThrough   []string      // coordinator --pull-through
	capacity      int           // worker --capacity
	graphRoot     string        // attempt --graph-root
	runRoot       string        // attempt --run-root
}

// defaultOptions returns the options as they are before any flag is given.
func defaultOptions() options {
	return options{
		dir:         ".",
		pushFrom:    pushFromFlag(finish.FromExecutor),
		coordinator: os.Getenv("VAMOS_COORDINATOR"),
		listen:      ":8080",
		grace:       5 * time.Minute,
		cacheDir:    "/var/lib/vamos",
		lease:       30 * time.Second,
		retries:     2,
		bound:       4,
		maxBuilds:   64,
		capacity:    1,
	}
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
	holds          []*hold       // the builds held open on the executor until it is closed
	grace          time.Duration // the executor's grace period, from its capabilities
	generation     uint64        // the executor's cache generation, from its capabilities, read before capture
}

// hold keeps a build open on its executor while the command delivers its result: a watch past the terminal
// event, which delivers nothing, renews the hold every third of the grace period, and closing the executor
// releases the build.
type hold struct {
	id   api.JobID
	stop context.CancelFunc
	done chan struct{}
}

// hold starts holding the build id open on ex, once its terminal event has been seen at position end.
func (inv *invocation) hold(ex api.Executor, id api.JobID, end int64) {
	ctx, stop := context.WithCancel(context.WithoutCancel(inv.ctx))
	h := &hold{id: id, stop: stop, done: make(chan struct{})}
	inv.holds = append(inv.holds, h)
	interval := inv.grace / 3
	go func() {
		defer close(h.done)
		if interval <= 0 {
			return // the executor holds builds as long as it lives
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := ex.Watch(ctx, id, end+1, func(api.Event) error { return nil }); err != nil &&
					ctx.Err() == nil {
					inv.warn(fmt.Errorf("holding the build open on the executor: %w", err))
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

// release lets go of every build held on ex, so that a prune may run and the coordinator may free its state;
// a failure to release is a warning, since the hold lapses on its own.
func (inv *invocation) release(ex api.Executor) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(inv.ctx), 10*time.Second)
	defer cancel()
	for _, h := range inv.holds {
		h.stop()
		<-h.done
		if err := ex.Release(ctx, h.id); err != nil && !errors.Is(err, api.ErrExpired) {
			inv.warn(fmt.Errorf("releasing the build on the executor: %w", err))
		}
	}
	inv.holds = nil
}

// NewRootCommand returns the vamos command line, with its subcommands, as cfg says.
func NewRootCommand(cfg Config) *cobra.Command {
	a := &app{fs: cfg.Fs, store: cfg.Store}
	defaults := defaultOptions()
	opts := &defaults

	root := &cobra.Command{
		Use:     "vamos",
		Short:   "Build container images from Starlark BUILD files",
		Version: cmp.Or(run.Version(), "(devel)"),
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
		"require HTTPS and verify certificates when vamos itself contacts a registry: pushing with "+
			"--push-from=client, and reading a coordinator's images to load, save or push; the executor follows "+
			"its registries.conf, and so does vamos where this is unset")
	root.PersistentFlags().Lookup("tls-verify").NoOptDefVal = "true"
	root.PersistentFlags().Var(&opts.timestamp, "timestamp",
		"stamp the files of targets that run no earlier than this time, in Unix seconds or RFC 3339")
	root.PersistentFlags().StringArrayVar(&opts.sets, "set", nil,
		"give a parameter of the named target a value, as `name=value`; separate a list's strings with commas. "+
			"build gives it to each named target that has the parameter")
	root.PersistentFlags().Var(&opts.secrets, "secret",
		"give run() steps' secret `id=ID,env=VARIABLE` the value of this environment variable, "+
			"or id=ID,src=FILE that of this file; repeat it for each secret. "+
			"Secrets stay out of the build cache and images, and are never sent to a coordinator")
	root.PersistentFlags().StringVar(&opts.platform, "platform", "",
		"build for this `os/arch`, such as linux/arm64, rather than the host's")
	root.PersistentFlags().StringVar(&opts.execPlatform, "exec-platform", "",
		"run commands on this `os/arch`, which the executor must offer, rather than this machine's, "+
			"or the only one offered")
	// Emulation needs no permission from a local build yet; the flag takes effect once an executor requires it.
	root.PersistentFlags().BoolVar(&opts.emulate, "emulate", false,
		"allow commands in images for another platform than the execution platform to run under emulation")
	root.PersistentFlags().MarkHidden("emulate")
	root.PersistentFlags().StringVar(&opts.coordinator, "coordinator", opts.coordinator,
		"build on the coordinator at this `URL`, such as http://coordinator:8080, rather than in this process; "+
			"$VAMOS_COORDINATOR gives the default")

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

	coordinatorCmd := &cobra.Command{
		Use:   "coordinator",
		Short: "Coordinate builds across workers for clients that give --coordinator, until interrupted",
		Long: "Run as a coordinator: schedule what clients on the network submit across the workers that register,\n" +
			"keep the build cache they share, and serve the clients their results. Workers push the layers they\n" +
			"build to --cache-repository and pull bases through the caches that --pull-through maps. There is no\n" +
			"authentication: run it on a trusted network alone. Clients name it with --coordinator or\n" +
			"$VAMOS_COORDINATOR; workers with `vamos worker --coordinator`.",
		Example: "  vamos coordinator --listen :8080 --cache-repository registry:5000/vamos-cache \\\n" +
			"    --pull-through docker.io=hub-proxy:5000",
		Args: noArgs,
		RunE: handle(func(inv *invocation, args []string) error { return inv.coordinator() }),
	}
	coordinatorCmd.Flags().StringVar(&opts.listen, "listen", opts.listen, "the `address` to listen on")
	coordinatorCmd.Flags().DurationVar(&opts.grace, "grace", opts.grace,
		"how long a build goes unwatched before nothing more of it starts, and how long clients keep retrying "+
			"after a drop")
	coordinatorCmd.Flags().DurationVar(&opts.window, "window", 0,
		"how long a finished build stays readable before the coordinator forgets it; --grace if shorter or unset")
	coordinatorCmd.Flags().StringVar(&opts.cacheDir, "cache-dir", opts.cacheDir,
		"the `directory` of the build cache: sources, trees, logs and results")
	coordinatorCmd.Flags().StringArrayVar(&opts.offer, "offer", nil,
		"an `os/arch` the deployment offers workers for, besides those registered now; repeat for several")
	coordinatorCmd.Flags().StringVar(&opts.defaultExec, "default-exec-platform", "",
		"the `os/arch` that clients run commands on unless they give --exec-platform, rather than their own, "+
			"so that clients of every platform share cache keys; one that --offer names")
	coordinatorCmd.Flags().StringArrayVar(&opts.offerEmulated, "offer-emulated", nil,
		"an `os/arch` the deployment has workers with emulators for, besides what registered workers report; "+
			"a build that --emulate lets run commands for it waits for such a worker")
	coordinatorCmd.Flags().DurationVar(&opts.lease, "lease", opts.lease,
		"how long an attempt lives without a heartbeat from its worker")
	coordinatorCmd.Flags().IntVar(&opts.retries, "retries", opts.retries,
		"how many times work whose worker is lost is tried again elsewhere")
	coordinatorCmd.Flags().IntVar(&opts.bound, "bound", opts.bound, "how many targets of one build run at once")
	coordinatorCmd.Flags().IntVar(&opts.maxBuilds, "max-builds", opts.maxBuilds,
		"how many builds may be open at once before submissions are refused as busy")
	coordinatorCmd.Flags().StringVar(&opts.cacheRepo, "cache-repository", "",
		"the registry `repository` that workers push built layers to, tagged by image ID")
	coordinatorCmd.Flags().StringArrayVar(&opts.pullThrough, "pull-through", nil,
		"map an upstream registry to the pull-through cache that mirrors it, as `registry=cache`; repeat for several")
	// --local serves a local executor instead, building in this process: the step before workers existed,
	// kept for a coordinator with no workers at hand, such as a test's.
	coordinatorCmd.Flags().BoolVar(&opts.local, "local", false,
		"build in this process, with its container store, rather than on workers")
	coordinatorCmd.Flags().MarkHidden("local")

	workerCmd := &cobra.Command{
		Use:   "worker",
		Short: "Run builds that a coordinator assigns, until interrupted",
		Long: "Run as a worker of the coordinator that --coordinator names: register, run the attempts it assigns,\n" +
			"each in a contained subprocess of this binary, in this machine's container store, and report what\n" +
			"they made. It needs root, cgroup v2 and buildah's chroot isolation, as building does.",
		Example: "  vamos worker --coordinator http://coordinator:8080 --capacity 2",
		Args:    noArgs,
		RunE:    handle(func(inv *invocation, args []string) error { return inv.worker() }),
	}
	workerCmd.Flags().IntVar(&opts.capacity, "capacity", opts.capacity, "how many attempts to run at once")

	// attempt runs one attempt as the subprocess that a worker starts: hidden, since only workers run it.
	attemptCmd := &cobra.Command{
		Use:    "attempt",
		Short:  "Run one attempt for a worker, from its standard input",
		Hidden: true,
		Args:   noArgs,
		RunE:   handle(func(inv *invocation, args []string) error { return inv.attempt() }),
	}
	attemptCmd.Flags().StringVar(&opts.graphRoot, "graph-root", "", "the container store's graph root")
	attemptCmd.Flags().StringVar(&opts.runRoot, "run-root", "", "the container store's run root")
	attemptCmd.Flags().MarkHidden("graph-root")
	attemptCmd.Flags().MarkHidden("run-root")

	root.AddCommand(buildCmd, pushCmd, loadCmd, saveCmd, writeCmd, exportCmd, pruneCmd, graphCmd, coordinatorCmd,
		workerCmd, attemptCmd)
	return root
}

// UsesContainerStore reports whether the command line args names a command that opens container storage:
// every command but a coordinator, which schedules builds and keeps no images, unless it builds locally,
// and but the root command, help and --version, which print and exit.
// main consults it before the re-exec that container storage needs from a root without capabilities,
// which fails where user namespaces are refused, as in a Dockerfile's RUN step that runs `vamos --version`.
// The command is found as cobra finds it, so that a target or directory named "coordinator" isn't mistaken for it;
// a command line that doesn't parse is left to Execute to refuse, after the re-exec, which is harmless.
func UsesContainerStore(args []string) bool {
	root := NewRootCommand(Config{Fs: afero.NewMemMapFs()})
	cmd, rest, err := root.Traverse(args)
	if err != nil {
		return true
	}
	// Execute adds these flags, and the help and completion commands, which Traverse leaves to the root.
	cmd.InitDefaultHelpFlag()
	root.InitDefaultVersionFlag()
	if err := cmd.ParseFlags(rest); err != nil {
		return true
	}
	if help, err := cmd.Flags().GetBool("help"); cmd == root || err == nil && help {
		return false
	}
	if cmd.Name() != "coordinator" {
		return true
	}
	local, err := cmd.Flags().GetBool("local")
	return err != nil || local
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

// execPlatform returns the execution platform to configure the graph for, among those the executor offers, caps:
// the one --exec-platform names, or else the one the executor says clients use, as a coordinator may, or else this
// machine's, if offered, or else the only one offered. Several offered, none of them this machine's, must be chosen
// between.
func (inv *invocation) execPlatform(caps api.Capabilities) (imgspecv1.Platform, error) {
	var offered []string
	for _, p := range caps.Platforms {
		offered = append(offered, platforms.Format(platforms.Normalize(p)))
	}
	if text := inv.opts.execPlatform; text != "" {
		// platforms.Parse takes an OS alone, filling in this machine's architecture, which isn't what was asked.
		p, err := graph.ParsePlatform(text)
		if err != nil || !strings.Contains(text, "/") {
			return imgspecv1.Platform{}, fmt.Errorf("--exec-platform %s: platform %q: want os/arch or "+
				"os/arch/variant, such as \"linux/arm64\"", text, text)
		}
		switch {
		case len(offered) == 0:
			return imgspecv1.Platform{}, errors.New("the executor offers no platform to run commands on")
		case !caps.Offers(p):
			return imgspecv1.Platform{}, fmt.Errorf("--exec-platform %s: the executor doesn't run commands on %s; "+
				"it offers %s", inv.opts.execPlatform, platforms.Format(p), fileset.QuoteNames(offered))
		}
		return p, nil
	}
	if p := caps.ExecPlatform; p != nil && caps.Offers(*p) {
		return platforms.Normalize(*p), nil
	}
	if caps.Offers(run.HostPlatform) {
		return run.HostPlatform, nil
	}
	switch len(caps.Platforms) {
	case 0:
		return imgspecv1.Platform{}, errors.New("the executor offers no platform to run commands on")
	case 1:
		return platforms.Normalize(caps.Platforms[0]), nil
	}
	return imgspecv1.Platform{}, fmt.Errorf("the executor doesn't run commands on this machine's platform, %s; "+
		"choose among %s with --exec-platform", platforms.Format(run.HostPlatform), fileset.QuoteNames(offered))
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

// executor is what a command builds with: an executor and the content store it reads submitted content from,
// which the client captures sources into and delivers from.
// The local executor's store is the build cache beside its container store; a coordinator's is served over HTTP.
type executor interface {
	api.Executor
	ContentStore() (cache.ContentStore, error)
	Close() error
}

// executor returns the executor to build with, which the caller must close with closeExecutor,
// and the client's own registry settings, for finish; the executor builds its own from its environment.
// With --coordinator, it is the coordinator named, which this process reaches over HTTP and opens no store for.
func (inv *invocation) executor() (executor, *types.SystemContext, error) {
	sys, err := inv.systemContext()
	if err != nil {
		return nil, nil, err
	}
	if inv.opts.coordinator != "" {
		if len(inv.opts.secrets) > 0 {
			return nil, nil, fmt.Errorf("--secret %s can't go to a coordinator, which has no authentication or TLS; "+
				"build in this process, without --coordinator or $VAMOS_COORDINATOR", inv.opts.secrets[0].id)
		}
		client, err := remote.NewClient(inv.opts.coordinator)
		if err != nil {
			return nil, nil, fmt.Errorf("--coordinator: %w", err)
		}
		client.Warn = func(msg string) { inv.warn(errors.New(msg)) }
		return &remoteExecutor{client}, sys, nil
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

// remoteExecutor is a coordinator as an executor: its content store is the coordinator's, over HTTP.
type remoteExecutor struct {
	*remote.Client
}

// ContentStore returns the coordinator's content store.
func (r *remoteExecutor) ContentStore() (cache.ContentStore, error) {
	return r.Client, nil
}

// closeExecutor shuts ex down, warning about what failed: the command's own outcome stands.
func (inv *invocation) closeExecutor(ex executor) {
	inv.release(ex)
	if err := ex.Close(); err != nil {
		inv.warn(fmt.Errorf("shutting the executor down: %w", err))
	}
}

// coordinator coordinates builds for clients on the network, at --listen, until the command is interrupted:
// across the workers that register, or, with --local, in this process with the local executor.
func (inv *invocation) coordinator() error {
	var server *remote.Server
	var closer executor
	if inv.opts.local {
		options, err := inv.storeOptions()
		if err != nil {
			return err
		}
		local, err := run.NewLocal(options)
		if err != nil {
			return err
		}
		content, err := local.ContentStore()
		if err != nil {
			inv.closeExecutor(local)
			return err
		}
		server, closer = &remote.Server{Executor: local, Content: content, GracePeriod: inv.opts.grace}, local
		return inv.serve(server, closer, false)
	}
	c, err := inv.newCoordinator()
	if err != nil {
		return err
	}
	server, closer = &remote.Server{Executor: c, Content: c.content, Workers: c.Workers()}, c
	return inv.serve(server, closer, true)
}

// coordinatorStores is a coordinator with the build cache it keeps, closed together.
type coordinatorStores struct {
	*coord.Coordinator
	directory *cache.Directory
	content   cache.ContentStore
}

// ContentStore returns the coordinator's content store.
func (c *coordinatorStores) ContentStore() (cache.ContentStore, error) {
	return c.content, nil
}

// Close closes the coordinator, then its build cache.
func (c *coordinatorStores) Close() error {
	return errors.Join(c.Coordinator.Close(), c.directory.Close())
}

// newCoordinator returns the coordinator the options describe, over the build cache at --cache-dir.
func (inv *invocation) newCoordinator() (*coordinatorStores, error) {
	offered, err := parsePlatforms("--offer", inv.opts.offer)
	if err != nil {
		return nil, err
	}
	emulated, err := parsePlatforms("--offer-emulated", inv.opts.offerEmulated)
	if err != nil {
		return nil, err
	}
	var execPlatform *imgspecv1.Platform
	if text := inv.opts.defaultExec; text != "" {
		parsed, err := parsePlatforms("--default-exec-platform", []string{text})
		if err != nil {
			return nil, err
		}
		if !(api.Capabilities{Platforms: offered}).Offers(parsed[0]) {
			return nil, fmt.Errorf("--default-exec-platform %s: give --offer %s too, so that it is offered whether "+
				"or not a worker of it is registered", text, text)
		}
		execPlatform = &parsed[0]
	}
	retries := inv.opts.retries
	switch {
	case retries < 0:
		return nil, fmt.Errorf("--retries %d: want zero or more", retries)
	case retries == 0:
		retries = coord.NoRetries // the coordinator's zero is its default, which an explicit zero isn't
	}
	pullThrough := map[string]string{}
	for _, mapping := range inv.opts.pullThrough {
		upstream, through, ok := strings.Cut(mapping, "=")
		if !ok || upstream == "" || through == "" {
			return nil, fmt.Errorf("--pull-through %s: want registry=cache, such as docker.io=hub-proxy:5000", mapping)
		}
		for _, host := range []string{upstream, through} {
			if err := checkRegistryHost(host); err != nil {
				return nil, fmt.Errorf("--pull-through %s: %w", mapping, err)
			}
		}
		pullThrough[upstream] = through
	}
	if inv.opts.cacheRepo == "" {
		inv.warn(errors.New("--cache-repository is unset: workers push no layers, so a build's images stay on " +
			"the worker that made them, where neither the client nor another worker can read them"))
	}
	sys, err := inv.systemContext()
	if err != nil {
		return nil, err
	}
	directory, err := cache.OpenDirectory(inv.opts.cacheDir)
	if err != nil {
		return nil, fmt.Errorf("--cache-dir %s: %w", inv.opts.cacheDir, err)
	}
	c, err := coord.New(directory, directory, coord.Config{
		Platforms:       offered,
		ExecPlatform:    execPlatform,
		Emulated:        emulated,
		Lease:           inv.opts.lease,
		Grace:           inv.opts.grace,
		Window:          inv.opts.window,
		Retries:         retries,
		Bound:           inv.opts.bound,
		MaxBuilds:       inv.opts.maxBuilds,
		CacheRepository: inv.opts.cacheRepo,
		PullThrough:     pullThrough,
		Version:         run.Version(),
		Images:          &coord.Registry{Sys: sys},
		// The jobs are kept beside the build cache, on the volume that keeps it, which a prune leaves alone.
		State: filepath.Join(inv.opts.cacheDir, "coordinator"),
		Warn:  inv.warn,
	})
	if err != nil {
		directory.Close()
		return nil, fmt.Errorf("--cache-dir %s: %w", inv.opts.cacheDir, err)
	}
	return &coordinatorStores{Coordinator: c, directory: directory, content: directory}, nil
}

// checkRegistryHost checks that host names a registry as an image reference would: with a dot or a port, or as
// localhost, so that a bare name isn't taken for a Docker Hub path.
func checkRegistryHost(host string) error {
	named, err := reference.ParseNormalizedNamed(host + "/x")
	if err != nil || (reference.Domain(named) != host && host != "index.docker.io") {
		return fmt.Errorf("%q isn't a registry host; want one with a dot or a port, or localhost, such as docker.io "+
			"or hub-proxy:5000", host)
	}
	return nil
}

// parsePlatforms parses the platforms given to the flag named flag.
func parsePlatforms(flag string, texts []string) ([]imgspecv1.Platform, error) {
	var parsed []imgspecv1.Platform
	for _, text := range texts {
		p, err := graph.ParsePlatform(text)
		if err != nil || !strings.Contains(text, "/") {
			return nil, fmt.Errorf("%s %s: platform %q: want os/arch or os/arch/variant, such as \"linux/arm64\"",
				flag, text, text)
		}
		parsed = append(parsed, p)
	}
	return parsed, nil
}

// serve serves s at --listen until the command is interrupted, then closes closer, the executor it serves,
// so that its jobs are cancelled and cleaned up, which ends the watches that clients hold open, and shuts down.
// An executor that resumes its jobs when it starts again, as a coordinator does, has its event streams and
// assignment polls cut first, so that its clients see a drop, and resume their watches once it is back, rather than
// being told that their builds were cancelled by the shutdown; what else is in flight, such as an upload or a
// worker's report, may finish within the shutdown's wait, and the executor is closed only after it.
func (inv *invocation) serve(s *remote.Server, closer executor, resumes bool) error {
	server := &http.Server{Handler: s.Handler()}
	listener, err := net.Listen("tcp", inv.opts.listen)
	if err != nil {
		inv.closeExecutor(closer)
		return fmt.Errorf("--listen %s: %w", inv.opts.listen, err)
	}
	if err := inv.printf("Coordinating builds at %s\n", listener.Addr()); err != nil {
		inv.closeExecutor(closer)
		return err
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	select {
	case err := <-served:
		inv.closeExecutor(closer)
		return err
	case <-inv.ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if resumes {
		s.EndStreams()
	} else {
		inv.closeExecutor(closer) // which ends the jobs, and so the watches of them
	}
	if err := server.Shutdown(shutdown); err != nil {
		server.Close() // whatever is still connected is cut off
	}
	if resumes {
		inv.closeExecutor(closer)
	}
	return nil
}

// worker runs attempts for the coordinator that --coordinator names, until the command is interrupted.
func (inv *invocation) worker() error {
	if inv.opts.coordinator == "" {
		return errors.New("a worker needs the coordinator to work for: give --coordinator or $VAMOS_COORDINATOR")
	}
	options, err := inv.storeOptions()
	if err != nil {
		return err
	}
	w, err := worker.New(worker.Config{
		Coordinator: inv.opts.coordinator,
		Store:       options,
		Capacity:    inv.opts.capacity,
		Log:         inv.stderr,
	})
	if err != nil {
		return err
	}
	return w.Run(inv.ctx)
}

// attempt runs one attempt as a worker's subprocess: the assignment comes on standard input, and what it does goes
// to standard output, the report last. The worker names the roots of its container store, so that the attempt builds
// where the worker cleans up.
func (inv *invocation) attempt() error {
	options, err := inv.storeOptions()
	if err != nil {
		return err
	}
	if inv.opts.graphRoot != "" || inv.opts.runRoot != "" {
		if !filepath.IsAbs(inv.opts.graphRoot) || !filepath.IsAbs(inv.opts.runRoot) {
			return fmt.Errorf("the container store's graph root %q and run root %q must both be absolute paths",
				inv.opts.graphRoot, inv.opts.runRoot)
		}
		options.GraphRoot, options.RunRoot = inv.opts.graphRoot, inv.opts.runRoot
	}
	return run.Attempt(inv.ctx, os.Stdin, inv.stdout, options)
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

// capabilities asks ex what it offers, before the graph is loaded, refuses an executor that doesn't run this
// vamos's graphs, as Submit would, and returns the execution platform to configure the graph for,
// which EXEC_PLATFORM names.
func (inv *invocation) capabilities(ex api.Executor) (imgspecv1.Platform, error) {
	caps, err := ex.Capabilities(inv.ctx)
	if err != nil {
		return imgspecv1.Platform{}, err
	}
	inv.grace, inv.generation = caps.GracePeriod, caps.Generation
	if !slices.Contains(caps.GraphVersions, graph.Version) {
		who := "the executor"
		if inv.opts.coordinator != "" {
			who = "the coordinator at " + inv.opts.coordinator
		}
		return imgspecv1.Platform{}, fmt.Errorf("%s runs graph versions %v, not this vamos's, %d; "+
			"use a vamos of its version", who, caps.GraphVersions, graph.Version)
	}
	return inv.execPlatform(caps)
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
	secrets, err := inv.opts.secrets.values()
	if err != nil {
		return nil, nil, err
	}
	req.Secrets = map[string]bool{}
	for id := range secrets {
		req.Secrets[id] = true
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
		Graph:      stored,
		Version:    g.Version,
		Platform:   execPlatform,
		Rebuild:    rebuild,
		Timestamp:  time.Time(inv.opts.timestamp),
		Emulate:    inv.opts.emulate,
		Generation: inv.generation, // read before the sources were captured: a prune since refuses the build
		Secrets:    secrets,
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
	outcome, end, err := watchJob(ctx, ex, id, events)
	runSpan.EndWith(err)
	if err == nil {
		inv.hold(ex, id, end) // through delivery, until the executor is closed
	} else {
		inv.holds = append(inv.holds, &hold{id: id, stop: func() {}, done: closedChan})
	}
	return outcome.Results, c, err
}

// closedChan is a channel that is closed, for a hold that renews nothing.
var closedChan = func() chan struct{} { ch := make(chan struct{}); close(ch); return ch }()

// deliver has the executor export res to the content store, and returns what it exported there.
func (inv *invocation) deliver(
	ctx context.Context, ex api.Executor, res api.Result, events *eventPrinter,
) (api.Exported, error) {
	id, err := ex.Submit(ctx, api.Request{Export: &api.ExportRequest{Result: res, Trace: tracing.Call(ctx)}})
	if err != nil {
		return api.Exported{}, err
	}
	outcome, _, err := watchJob(ctx, ex, id, events)
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
		execPlatform, err := inv.capabilities(ex)
		if err != nil {
			return err
		}
		req := inv.request([]string{target}, load.WantImage, execPlatform)
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
				outcome, _, err = watchJob(ctx, ex, id, events)
			}
			if err == nil && outcome.Pushed != nil {
				imageId = outcome.Pushed.Image
				if outcome.Pushed.Index != "" {
					imageId = outcome.Pushed.Index
				}
			}
		} else {
			// An image at a registry, as a coordinator's workers leave it, is read from there; the local executor
			// exports from its store to the content store first, even what it pulled from a registry, so that a
			// local delivery needs no network.
			var img finish.Image
			located := false
			if inv.opts.coordinator != "" {
				img, located, err = finish.FromLocations(res)
			}
			if err == nil && !located {
				var exp api.Exported
				if exp, err = inv.deliver(ctx, ex, res, events); err == nil {
					img = finish.Exported(content, exp)
				}
			}
			if err == nil {
				imageId, err = dest.send(ctx, img, sys)
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
		send: func(ctx context.Context, img finish.Image, sys *types.SystemContext) (string, error) {
			return finish.Push(ctx, img, name, sys)
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
		send: func(ctx context.Context, img finish.Image, sys *types.SystemContext) (string, error) {
			return finish.LoadDocker(ctx, img, name, sys)
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
		send: func(ctx context.Context, img finish.Image, sys *types.SystemContext) (string, error) {
			return finish.Save(ctx, img, path, name, sys)
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
		execPlatform, err := inv.capabilities(ex)
		if err != nil {
			return err
		}
		events := inv.events()
		results, c, err := inv.runGraph(ex, ex.ContentStore, inv.request([]string{task}, load.WantFiles, execPlatform),
			false, events)
		if err != nil {
			return err
		}
		res := results[0]
		ctx, span := tracing.Start(inv.ctx, action, "files", res.Files.Digest.String())
		// A task's files are in the content store already, which the client reads: no executor delivers them,
		// and finish.Export reads every one before it installs any, so a missing or corrupt file fails it there.
		err = finish.Export(ctx, c, api.Exported{Files: res.Files, Epoch: res.Epoch}, dir, inv.opts.replace, inv.warn)
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
		execPlatform, err := inv.capabilities(ex)
		if err != nil {
			return err
		}
		_, _, err = inv.runGraph(ex, ex.ContentStore, inv.request(targets, load.WantBuild, execPlatform),
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
	outcome, _, err := watchJob(inv.ctx, ex, id, inv.events())
	if err != nil {
		return err
	}
	p := outcome.Pruned
	if inv.opts.coordinator != "" {
		return inv.printf("Removed %d results and %d files from the build cache, and %d image tags from the cache "+
			"repository\n", p.Results, p.Blobs, p.Tags)
	}
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
	execPlatform, err := inv.capabilities(ex)
	if err != nil {
		return err
	}
	req := inv.request(targets, load.WantBuild, execPlatform)
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
// and returns its outcome, read back with Result, with the position of its terminal event, or the error its
// failure reports.
// If ctx ends first, as on Ctrl-C, or the events can't be written, it cancels the job and waits for it to stop,
// so that its cleanup runs, and returns ctx's error, or the write's, unless the job had already failed on its own:
// then the job's failure is returned, joined with the write's.
func watchJob(ctx context.Context, ex api.Executor, id api.JobID, events *eventPrinter) (api.Outcome, int64, error) {
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
			return api.Outcome{}, 0, errors.Join(cmp.Or(unwritten, context.Canceled), err)
		}
		err = watch(ctx)
	}
	if err != nil {
		return api.Outcome{}, 0, errors.Join(unwritten, err)
	}
	outcome, err := ex.Result(ctx, id)
	switch {
	case err != nil:
		return api.Outcome{}, 0, errors.Join(unwritten, err)
	case outcome.Failure != nil && !outcome.Cancelled:
		// The job's own failure is the command's error, and a failure to write its events is reported with it.
		return api.Outcome{}, 0, errors.Join(errors.New(outcome.Failure.Describe(events.graph)), unwritten)
	case unwritten != nil:
		return api.Outcome{}, 0, unwritten
	case interrupted:
		return api.Outcome{}, 0, context.Canceled
	case outcome.Failure != nil:
		return api.Outcome{}, 0, errors.New(outcome.Failure.Describe(events.graph))
	case outcome.Cancelled:
		return api.Outcome{}, 0, errors.New("the job was cancelled")
	}
	return outcome, next - 1, nil
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
			p.trace.addGap(e.Position, e.First, e.Restarted)
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
		if e.Restarted {
			fmt.Fprint(p.stderr, "vamos: warning: the coordinator restarted, so the progress it showed before isn't "+
				"shown again; a build it resumed runs again, what finished before cached\n")
			break
		}
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
	// send sends img, the built image as the client reads it, from a registry or the content store, to the
	// destination, as the client's settings sys describe it, returning its image ID there, or an index's digest.
	// If push is set, a full registry reference, the executor pushes there instead, and send isn't called.
	send func(ctx context.Context, img finish.Image, sys *types.SystemContext) (string, error)
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

// secretsFlag is the value of --secret, repeated: the secrets that run() steps are given, each by its ID,
// from an environment variable or a file, which are read only once a build needs them.
type secretsFlag []secretSource

// secretSource is where one --secret's value comes from: the variable env or the file src, one of them.
type secretSource struct {
	id, env, src string
}

func (f *secretsFlag) String() string {
	var given []string
	for _, s := range *f {
		if s.env != "" {
			given = append(given, "id="+s.id+",env="+s.env)
		} else {
			given = append(given, "id="+s.id+",src="+s.src)
		}
	}
	return strings.Join(given, " ")
}

// Set parses value as id=ID,env=VARIABLE or id=ID,src=FILE, refusing an ID given before.
func (f *secretsFlag) Set(value string) error {
	var s secretSource
	for _, field := range strings.Split(value, ",") {
		key, v, ok := strings.Cut(field, "=")
		switch {
		case !ok || v == "":
			return fmt.Errorf("%q isn't key=value; want id=ID,env=VARIABLE or id=ID,src=FILE", field)
		case key == "id":
			s.id = v
		case key == "env":
			s.env = v
		case key == "src":
			s.src = v
		default:
			return fmt.Errorf("unknown key %q; want id=ID,env=VARIABLE or id=ID,src=FILE", key)
		}
	}
	if err := graph.CheckSecret(graph.Secret{ID: s.id, File: "/" + s.id}); err != nil {
		return err
	}
	if (s.env == "") == (s.src == "") {
		return fmt.Errorf("secret %q needs env=VARIABLE or src=FILE, but not both", s.id)
	}
	for _, given := range *f {
		if given.id == s.id {
			return fmt.Errorf("secret %q is given twice", s.id)
		}
	}
	*f = append(*f, s)
	return nil
}

func (f *secretsFlag) Type() string {
	return "id=ID,env=VARIABLE|src=FILE"
}

// values reads each secret's value, by its ID: an unset variable or a file that can't be read is an error,
// naming the secret but never a value.
func (f secretsFlag) values() (map[string][]byte, error) {
	values := map[string][]byte{}
	for _, s := range f {
		if s.env != "" {
			v, ok := os.LookupEnv(s.env)
			if !ok {
				return nil, fmt.Errorf("--secret id=%s,env=%s: $%s isn't set", s.id, s.env, s.env)
			}
			values[s.id] = []byte(v)
			continue
		}
		data, err := os.ReadFile(s.src)
		if err != nil {
			return nil, fmt.Errorf("--secret id=%s,src=%s: %w", s.id, s.src, err)
		}
		values[s.id] = data
	}
	return values, nil
}

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
