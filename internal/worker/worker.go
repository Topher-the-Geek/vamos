package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/remote"
	"github.com/Topher-the-Geek/vamos/internal/run"
	"github.com/Topher-the-Geek/vamos/internal/sched"

	"go.podman.io/storage"
)

// Config says how a worker runs.
type Config struct {
	Coordinator string               // the coordinator's URL
	Store       storage.StoreOptions // the container store that attempts build in
	Capacity    int                  // how many attempts to run at once
	// Child returns the command that runs one attempt as a subprocess: this binary as `vamos attempt`,
	// which Child in child.go serves; tests run their own binary.
	Child func(ctx context.Context) *exec.Cmd
	// CgroupRoot is where attempts' cgroups are made; cgroupRoot if "".
	CgroupRoot string
	// Log receives what the worker does, for its operator; nil discards it.
	Log io.Writer
}

// Worker registers with a coordinator, runs the attempts it is assigned, each in a contained subprocess,
// and reports how they ended.
// Each attempt's lease is renewed by heartbeat; an attempt whose lease can't be renewed is killed before the lease
// would expire at the coordinator, so that the coordinator can treat an expired lease as a stopped attempt.
type Worker struct {
	cfg     Config
	client  *remote.WorkerClient
	contain *containment

	mu       sync.Mutex
	reg      remote.Registration
	attempts map[string]*running
}

// running is an attempt being run.
type running struct {
	id     string
	cancel context.CancelFunc
	done   chan struct{}
}

// New returns a worker configured by cfg, which has checked that it can contain attempts.
func New(cfg Config) (*Worker, error) {
	if cfg.Capacity <= 0 {
		cfg.Capacity = 1
	}
	if cfg.Child == nil && testing.Testing() && os.Getenv(ChildEnv) != "" {
		cfg.Child = TestChild(cfg.Store) // a test binary, which ChildFromEnv makes an attempt's subprocess
	}
	if cfg.Child == nil {
		cfg.Child = func(ctx context.Context) *exec.Cmd {
			exe, err := os.Executable()
			if err != nil {
				exe = os.Args[0]
			}
			// The attempt builds in the store the worker cleans up in, whatever the subprocess would find by default.
			return exec.CommandContext(ctx, exe, "attempt",
				"--graph-root", cfg.Store.GraphRoot, "--run-root", cfg.Store.RunRoot)
		}
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	root := cfg.CgroupRoot
	if root == "" {
		root = cgroupRoot
	}
	contain, err := newContainment(root)
	if err != nil {
		return nil, err
	}
	client, err := remote.NewWorkerClient(cfg.Coordinator)
	if err != nil {
		return nil, err
	}
	return &Worker{cfg: cfg, client: client, contain: contain, attempts: map[string]*running{}}, nil
}

// logf writes to the worker's log.
func (w *Worker) logf(format string, args ...any) {
	fmt.Fprintf(w.cfg.Log, "worker: "+format+"\n", args...)
}

// Run registers and works until ctx ends: it asks for assignments and runs each; when the coordinator can't be
// reached for a lease, or answers that it doesn't know the registration, as one that restarted does, it registers
// again, once what it was running has been killed and cleaned up.
func (w *Worker) Run(ctx context.Context) error {
	defer w.client.Close()
	if err := w.cleanLeftovers(ctx); err != nil {
		w.logf("cleaning up after an earlier run: %v", err)
	}
	backoff := time.Second
	for ctx.Err() == nil {
		reg, err := w.register(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			w.logf("registering with %s: %v; trying again in %s", w.cfg.Coordinator, err, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
			}
			backoff = min(2*backoff, 30*time.Second)
			continue
		}
		backoff = time.Second
		w.logf("registered as %s with coordinator %s", reg.ID, reg.Instance)
		err = w.work(ctx, reg)
		if ctx.Err() == nil {
			w.logf("%v; registering again", err)
		}
		w.killAll()
	}
	w.killAll()
	return nil
}

// info describes this worker.
func (w *Worker) info() remote.WorkerInfo {
	return remote.WorkerInfo{
		Platform:      run.HostPlatform,
		Emulates:      emulatedPlatforms(binfmtDir, run.HostPlatform),
		Isolation:     run.Isolation(),
		Version:       run.Version(),
		ActionVersion: sched.ActionVersion,
		Capacity:      w.cfg.Capacity,
	}
}

// register registers with the coordinator and keeps the registration.
func (w *Worker) register(ctx context.Context) (remote.Registration, error) {
	reg, err := w.client.Register(ctx, w.info())
	if err != nil {
		return remote.Registration{}, err
	}
	w.mu.Lock()
	w.reg = reg
	w.mu.Unlock()
	return reg, nil
}

// lease returns the lease of the current registration, or a default before any.
func (w *Worker) lease() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.reg.Lease > 0 {
		return w.reg.Lease
	}
	return 30 * time.Second
}

// work asks for assignments under reg and runs them, until ctx ends or the coordinator is lost:
// unreachable, or another instance, which knows nothing of this registration.
func (w *Worker) work(ctx context.Context, reg remote.Registration) error {
	failures := 0
	for {
		a, err := w.client.NextAssignment(ctx, reg.ID)
		assigned := time.Now() // the lease began at the coordinator no later than this
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil && remote.IsTransport(err):
			failures++
			if time.Duration(failures)*time.Second > reg.Lease {
				return fmt.Errorf("the coordinator couldn't be reached for %s", reg.Lease)
			}
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
			}
			continue
		case err != nil:
			return fmt.Errorf("asking for an assignment: %w", err) // such as a coordinator that restarted
		}
		failures = 0
		if a == nil {
			continue
		}
		w.start(ctx, reg, a, assigned)
	}
}

// start runs the assignment a in a goroutine of its own.
func (w *Worker) start(ctx context.Context, reg remote.Registration, a *remote.Assignment, assigned time.Time) {
	actx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r := &running{id: a.Attempt, cancel: cancel, done: make(chan struct{})}
	w.mu.Lock()
	w.attempts[a.Attempt] = r
	w.mu.Unlock()
	go func() {
		defer close(r.done)
		defer func() {
			w.mu.Lock()
			delete(w.attempts, a.Attempt)
			w.mu.Unlock()
		}()
		w.attempt(actx, reg, a, assigned)
	}()
}

// killAll kills every attempt still running, as when the coordinator is lost, and waits for their cleanup,
// for at most killWait, or a lease if longer, in all, the deadline common to them so that attempts that don't end
// don't each wait that long: the kill itself waits that long for the processes to exit.
func (w *Worker) killAll() {
	w.killWithin(max(w.lease(), killWait))
}

// killWithin kills every attempt still running and waits for them for at most wait in all.
func (w *Worker) killWithin(wait time.Duration) {
	w.mu.Lock()
	var all []*running
	for _, r := range w.attempts {
		all = append(all, r)
	}
	w.mu.Unlock()
	for _, r := range all {
		r.cancel()
	}
	deadline, stop := context.WithTimeout(context.Background(), wait)
	defer stop()
	for _, r := range all {
		select {
		case <-r.done:
			continue
		default:
		}
		select {
		case <-r.done:
		case <-deadline.Done():
			w.logf("attempt %s didn't end within %s of being killed", r.id, wait)
		}
	}
}

// cleanLeftovers removes what attempts of an earlier run of a worker left behind: their cgroups, and the containers
// named for them in the store, whichever of the two remain.
func (w *Worker) cleanLeftovers(ctx context.Context) error {
	ids, err := w.contain.leftovers()
	if err != nil {
		return err
	}
	containers, err := run.LeftoverAttempts(w.cfg.Store)
	if err != nil {
		return err
	}
	for _, id := range containers {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	var errs []error
	for _, id := range ids {
		w.logf("cleaning up after attempt %s of an earlier run", id)
		if err := w.contain.kill(id, 10*time.Second); err != nil {
			errs = append(errs, err)
		}
		if err := run.RemoveAttempt(ctx, w.cfg.Store, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ChildEnv is the variable that makes a test binary run one attempt, as `vamos attempt` does: its value is the
// container store's graph root and run root, separated by a semicolon. A worker in a test whose environment sets it,
// to anything, runs its attempts as the test binary, in its own store.
const ChildEnv = "VAMOS_TEST_ATTEMPT_STORE"

// ChildFromEnv runs one attempt from standard input if childEnv is set, as a worker's subprocess in tests, and
// reports whether it did, so that the caller exits; a TestMain calls it before running tests, after buildah's own
// re-exec check.
func ChildFromEnv() bool {
	spec := os.Getenv(ChildEnv)
	if spec == "" {
		return false
	}
	graphRoot, runRoot, ok := strings.Cut(spec, ";")
	if !ok {
		return false // set to make a worker use this binary, not to run an attempt
	}
	options := storage.StoreOptions{GraphRoot: graphRoot, RunRoot: runRoot}
	if err := run.Attempt(context.Background(), os.Stdin, os.Stdout, options); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return true
}

// TestChild returns the command that runs this test binary as an attempt's subprocess, in the store options
// describes, for a worker in a test.
func TestChild(options storage.StoreOptions) func(ctx context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd {
		exe, err := os.Executable()
		if err != nil {
			exe = os.Args[0]
		}
		cmd := exec.CommandContext(ctx, exe)
		cmd.Env = append(os.Environ(), ChildEnv+"="+options.GraphRoot+";"+options.RunRoot)
		return cmd
	}
}
