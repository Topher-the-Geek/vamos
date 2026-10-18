package coord

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/remote"
	"github.com/Topher-the-Geek/vamos/internal/sched"
	"github.com/Topher-the-Geek/vamos/internal/tracing"

	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// Config says how a coordinator runs.
type Config struct {
	// Platforms are the execution platforms the deployment offers, beyond those of the workers registered now:
	// a build for one waits for a worker; one for a platform offered by neither is refused.
	Platforms []imgspecv1.Platform
	// Emulated are the platforms the deployment has workers with emulators for, beyond those the registered
	// workers report: an emulated target for one waits for such a worker; one for a platform offered by neither
	// fails at once.
	Emulated []imgspecv1.Platform
	// Lease is how long an attempt lives without a heartbeat; Heartbeat how often workers are told to renew.
	Lease, Heartbeat time.Duration
	// Grace is how long a build goes without a watcher before it expires: its demand is withdrawn, and what runs
	// finishes and publishes.
	Grace time.Duration
	// Window is how long a finished build stays readable before it is evicted; at least Grace.
	Window time.Duration
	// Retries is how many times work is tried again when the worker running it is lost; zero is the default,
	// two, and NoRetries, or any negative number, is none.
	Retries int
	// Bound is how many targets of one build are dispatched at once.
	Bound int
	// MaxBuilds is how many jobs, builds and pushes, may be open at once; beyond it, submissions are refused as busy.
	MaxBuilds int
	// CacheRepository is the registry repository that workers push built layers to, tagged by image ID;
	// "" for none.
	CacheRepository string
	// PullThrough maps each upstream registry to the pull-through cache that mirrors it, by host.
	PullThrough map[string]string
	// Version is the vamos version, for capabilities.
	Version string
	// Isolation is how workers run commands, for capabilities.
	Isolation string
	// Images checks whether an image is where a result says it is; nil accepts every registry location.
	Images ImageChecker
}

// NoRetries is the Config.Retries that tries no work again when its worker is lost.
const NoRetries = -1

// Defaults fills in what cfg leaves zero.
func (cfg Config) withDefaults() Config {
	if cfg.Lease == 0 {
		cfg.Lease = 30 * time.Second
	}
	if cfg.Heartbeat == 0 {
		cfg.Heartbeat = cfg.Lease / 3
	}
	if cfg.Grace == 0 {
		cfg.Grace = 5 * time.Minute
	}
	if cfg.Window < cfg.Grace {
		cfg.Window = cfg.Grace
	}
	switch {
	case cfg.Retries == 0:
		cfg.Retries = 2
	case cfg.Retries < 0:
		cfg.Retries = 0
	}
	if cfg.Bound == 0 {
		cfg.Bound = 4
	}
	if cfg.MaxBuilds == 0 {
		cfg.MaxBuilds = 64
	}
	return cfg
}

// Coordinator runs builds across a pool of workers: it schedules them, shares the actions in flight and their
// results among builds, and keeps the content and record stores; workers execute, and only they do.
// It implements api.Executor, which remote.Server serves to clients, and remote.Workers, which it serves to workers.
type Coordinator struct {
	cfg      Config
	instance string
	content  cache.ContentStore
	records  cache.RecordStore
	jobs     *sched.Jobs
	pool     *pool
	sharing  *sched.Sharing
	checker  *checker
	stop     context.CancelFunc
	started  time.Time // when this instance started, since a prune is refused for a lease after it
}

var (
	_ api.Executor   = (*Coordinator)(nil)
	_ remote.Workers = (*Coordinator)(nil)
)

// New returns a coordinator over the stores content and records, configured by cfg, running until Close.
func New(content cache.ContentStore, records cache.RecordStore, cfg Config) *Coordinator {
	cfg = cfg.withDefaults()
	var id [8]byte
	rand.Read(id[:])
	c := &Coordinator{
		cfg:      cfg,
		instance: hex.EncodeToString(id[:]),
		content:  content,
		records:  records,
		jobs:     &sched.Jobs{},
	}
	c.jobs.Limit(cfg.MaxBuilds)
	c.jobs.Expire(cfg.Grace, cfg.Window)
	c.pool = newPool(cfg, c.instance, func() (uint64, error) {
		return c.generation(context.Background())
	})
	c.sharing = sched.NewSharing(c.pool, content, records)
	c.checker = &checker{content: content, images: cfg.Images}
	ctx, cancel := context.WithCancel(context.Background())
	c.stop = cancel
	c.started = time.Now()
	go c.pool.sweep(ctx, c.jobs.Sweep)
	return c
}

// RetainEvents makes the coordinator retain n events of each job for watchers, rather than the default;
// it is for tests, before any job is started.
func (c *Coordinator) RetainEvents(n int) {
	c.jobs.RetainEvents(n)
}

// Close ends every job and attempt and waits for the jobs to end.
func (c *Coordinator) Close() error {
	c.stop()
	c.pool.close()
	c.jobs.Stop()
	return nil
}

// Workers returns the coordinator's side of the worker-facing API.
func (c *Coordinator) Workers() remote.Workers {
	return c
}

// Register, NextAssignment, Heartbeat, Events and Report are the pool's.
func (c *Coordinator) Register(ctx context.Context, info remote.WorkerInfo) (remote.Registration, error) {
	return c.pool.Register(ctx, info)
}

func (c *Coordinator) NextAssignment(ctx context.Context, worker string) (*remote.Assignment, error) {
	return c.pool.NextAssignment(ctx, worker)
}

func (c *Coordinator) Heartbeat(ctx context.Context, attempt string, beat remote.Heartbeat) (
	remote.HeartbeatAnswer, error) {
	return c.pool.Heartbeat(ctx, attempt, beat)
}

func (c *Coordinator) Events(ctx context.Context, attempt string, events remote.AttemptEvents) error {
	return c.pool.Events(ctx, attempt, events)
}

func (c *Coordinator) Report(ctx context.Context, attempt string, report remote.AttemptReport) error {
	return c.pool.Report(ctx, attempt, report)
}

// Capabilities reports the platforms offered, by configuration and by the workers registered, the graph versions
// run, the instance and its grace period.
func (c *Coordinator) Capabilities(ctx context.Context) (api.Capabilities, error) {
	offered := slices.Clone(c.cfg.Platforms)
	for _, p := range c.pool.platformsOffered() {
		if !slices.ContainsFunc(offered, func(o imgspecv1.Platform) bool {
			return platforms.Format(platforms.Normalize(o)) == platforms.Format(p)
		}) {
			offered = append(offered, p)
		}
	}
	generation, err := c.generation(ctx)
	if err != nil {
		return api.Capabilities{}, err
	}
	return api.Capabilities{
		Platforms:     offered,
		GraphVersions: []int{graph.Version},
		Isolation:     c.cfg.Isolation,
		Version:       c.cfg.Version,
		Clock:         time.Now(),
		Instance:      c.instance,
		GracePeriod:   c.cfg.Grace,
		Generation:    generation,
	}.Clone(), nil
}

// generation returns the cache generation the coordinator advertises: one more than the record store's count of
// prunes, so that a fresh store's is 1 and a graph's zero means a client that read none, never the first generation.
func (c *Coordinator) generation(ctx context.Context) (uint64, error) {
	generation, err := c.records.Generation(ctx)
	if err != nil {
		return 0, err
	}
	return generation + 1, nil
}

// offered reports whether the deployment offers platform: by configuration, or by a worker registered now.
func (c *Coordinator) offered(platform imgspecv1.Platform) bool {
	want := platforms.Format(platforms.Normalize(platform))
	for _, p := range c.cfg.Platforms {
		if platforms.Format(platforms.Normalize(p)) == want {
			return true
		}
	}
	return c.pool.offers(platform)
}

// maxGraphSize is the most bytes a graph may have, so that a corrupt or hostile one is refused rather than decoded.
const maxGraphSize = 64 << 20

// Submit starts a job for req: a graph run, a push or an export.
// A graph of a version it can't run, or for a platform the deployment doesn't offer, is refused before anything else.
func (c *Coordinator) Submit(ctx context.Context, req api.Request) (api.JobID, error) {
	req = req.Clone()
	kinds := 0
	for _, set := range []bool{req.Graph != nil, req.Push != nil, req.Export != nil} {
		if set {
			kinds++
		}
	}
	if kinds != 1 {
		return "", fmt.Errorf("the request asks for %d jobs, want 1", kinds)
	}
	switch {
	case req.Push != nil:
		if err := checkResult(req.Push.Result); err != nil {
			return "", err
		}
		return c.submitPush(*req.Push)
	case req.Export != nil:
		if err := checkResult(req.Export.Result); err != nil {
			return "", err
		}
		return "", errors.New("a coordinator exports nothing: the client delivers from the result's locations")
	}
	g := req.Graph
	if g.Version != graph.Version {
		return "", fmt.Errorf("graph version %d isn't supported; this coordinator runs version %d", g.Version,
			graph.Version)
	}
	if !c.offered(g.Platform) {
		return "", fmt.Errorf("the graph was configured for an executor on %s, which this coordinator doesn't offer; "+
			"it offers %v", platforms.Format(platforms.Normalize(g.Platform)), c.platformNames())
	}
	return c.jobs.StartBuild(func(ctx context.Context, ev sched.Events) api.Outcome {
		// The generation is checked once the build is open, since no prune starts then: checked before, a prune
		// could run between the check and the build's start.
		if err := c.checkGeneration(ctx, g.Generation); err != nil {
			return sched.Failed(err)
		}
		results, err := c.run(ctx, *g, ev)
		if err != nil {
			return sched.Failed(err)
		}
		return api.Outcome{Results: results}
	})
}

// checkGeneration refuses a graph captured against a generation other than the current one; zero, from a client
// that read none, is not checked.
func (c *Coordinator) checkGeneration(ctx context.Context, captured uint64) error {
	if captured == 0 {
		return nil
	}
	current, err := c.generation(ctx)
	if err != nil {
		return err
	}
	if captured != current {
		return errors.New("the build cache was pruned while the build's sources were being captured, so what " +
			"was captured may be gone; run the command again")
	}
	return nil
}

// platformNames returns the platforms offered, as text.
func (c *Coordinator) platformNames() []string {
	caps, _ := c.Capabilities(context.Background())
	var names []string
	for _, p := range caps.Platforms {
		names = append(names, platforms.Format(platforms.Normalize(p)))
	}
	slices.Sort(names)
	return names
}

// run runs the graph req names: reads it from the content store, and schedules it across the workers.
func (c *Coordinator) run(ctx context.Context, req api.GraphRequest, ev sched.Events) (_ []api.Result, err error) {
	ctx, flush := sched.ForCall(ctx, req.Trace, ev)
	defer flush()
	ctx, span := tracing.StartTrack(ctx, "run", "run", "graph", req.Graph.Digest.String(), "rebuild", req.Rebuild)
	defer func() { span.EndWith(err) }()
	data, err := c.content.Read(ctx, req.Graph, maxGraphSize)
	if err != nil {
		return nil, fmt.Errorf("reading the build graph: %w", err)
	}
	g, err := graph.Decode(data)
	if err != nil {
		return nil, err
	}
	platform, err := graph.ParsePlatform(g.ExecPlatform)
	if err != nil {
		return nil, err
	}
	// Submit checked that the request's platform is offered, and the build is scheduled for the graph's.
	if got, want := platforms.Format(platform), platforms.Format(platforms.Normalize(req.Platform)); got != want {
		return nil, fmt.Errorf("the build graph was configured for an executor on %s, but its request names %s", got,
			want)
	}
	// A worker that left meanwhile is waited for, as the pool says.
	ctx = withBuild(ctx, buildInfo{job: jobOf(ctx), platform: platform})
	b := sched.Backend{Content: c.content, Records: c.records, Checker: c.checker, Runner: c.sharing}
	opts := sched.Options{Rebuild: req.Rebuild, Timestamp: req.Timestamp, Emulate: req.Emulate, OptIn: true,
		Bound: c.cfg.Bound}
	return sched.New(g, b, ev, opts).Run(ctx)
}

// jobOf returns the job that ctx, a job's, belongs to.
func jobOf(ctx context.Context) api.JobID {
	return sched.JobOf(ctx)
}

// Watch delivers the events of the job id from position from to fn.
func (c *Coordinator) Watch(ctx context.Context, id api.JobID, from int64, fn func(api.Event) error) error {
	j, err := c.jobs.Lookup(id)
	if err != nil {
		return err
	}
	return j.Watch(ctx, from, fn)
}

// Cancel asks the job id to stop: its demand is withdrawn, and attempts no build needs are killed.
func (c *Coordinator) Cancel(_ context.Context, id api.JobID) error {
	j, err := c.jobs.Lookup(id)
	if err != nil {
		return err
	}
	j.Cancel()
	return nil
}

// Release lets go of the job id: a finished build is evicted at the next sweep, and a running one's demand is
// withdrawn.
func (c *Coordinator) Release(_ context.Context, id api.JobID) error {
	j, err := c.jobs.Lookup(id)
	if err != nil {
		return err
	}
	j.Release()
	return nil
}

// Result returns the outcome of the job id, or ErrRunning.
func (c *Coordinator) Result(_ context.Context, id api.JobID) (api.Outcome, error) {
	j, err := c.jobs.Lookup(id)
	if err != nil {
		return api.Outcome{}, err
	}
	return j.Result()
}

// Prune removes every cached result from the coordinator's stores, as a job refused while any other job is open,
// a finished build its client still holds included, while any attempt is live, such as one of an expired build that
// is finishing, and for a lease after this instance started, since a worker partitioned from the last instance may
// still run an attempt of it until it fences itself.
// The content goes first and the records after, since clearing the records advances the generation: a client that
// reads the new generation then uploads to a content store that nothing clears any more.
func (c *Coordinator) Prune(context.Context) (api.JobID, error) {
	if since := time.Since(c.started); since < c.cfg.Lease {
		return "", fmt.Errorf("%w: the coordinator started %s ago, and an attempt of an earlier instance may run "+
			"until a lease, %s, has passed", api.ErrSettling, since.Round(time.Second), c.cfg.Lease)
	}
	// Only an open job makes attempts, and Start refuses the prune while one is open, so no attempt starts between
	// this check and the prune.
	if n := c.pool.liveAttempts(); n > 0 {
		return "", fmt.Errorf("%w: %d attempts are still live on the workers, or waiting for one", api.ErrSettling, n)
	}
	return c.jobs.Start(true, func(ctx context.Context, ev sched.Events) api.Outcome {
		var pruned api.Pruned
		var err error
		if pruned.Blobs, err = c.content.ClearContent(ctx); err != nil {
			return sched.Failed(err)
		}
		if pruned.Results, err = c.records.ClearResults(ctx); err != nil {
			return sched.Failed(fmt.Errorf("the content is cleared, but the results aren't, so cached layers name "+
				"logs that are gone: prune again; %w", err))
		}
		// The cache repository's tags go last: a record that named one is gone already, so a failure here leaves
		// the registry holding images nothing names, which the next prune removes.
		if c.cfg.CacheRepository != "" && c.cfg.Images != nil {
			if pruned.Tags, err = c.cfg.Images.ClearRepository(ctx, c.cfg.CacheRepository); err != nil {
				return sched.Failed(fmt.Errorf("the build cache is pruned, but the cache repository %s isn't: %w",
					c.cfg.CacheRepository, err))
			}
		}
		return api.Outcome{Pruned: &pruned}
	})
}

// submitPush starts a job that has a worker push req.Result's image or index to req.Destination.
func (c *Coordinator) submitPush(req api.PushRequest) (api.JobID, error) {
	if _, err := sched.ParseDestination(req.Destination); err != nil {
		return "", err
	}
	return c.jobs.Start(false, func(ctx context.Context, ev sched.Events) api.Outcome {
		ctx, flush := sched.ForCall(ctx, req.Trace, ev)
		defer flush()
		ctx, span := tracing.StartTrack(ctx, "push", "push to "+req.Destination)
		ctx = withBuild(ctx, buildInfo{job: jobOf(ctx)})
		rep, err := c.pool.attempt(ctx, nil, &remote.PushWork{Result: req.Result, Destination: req.Destination},
			sched.OutputFunc(func(byte, []byte) {}))
		span.EndWith(err)
		switch {
		case errors.Is(err, context.Canceled):
			err = fmt.Errorf("%w: the push was cancelled, and the destination %s may or may not have been written; "+
				"check it, or push again", err, req.Destination)
		case err != nil:
		case rep.Failure != nil:
			err = rep.Failure.Err()
		case rep.Pushed == nil:
			err = errors.New("the worker pushed nothing")
		}
		if err != nil {
			outcome := sched.Failed(err)
			outcome.Failure.Nodes = []graph.Ref{req.Result.Node}
			return outcome
		}
		return api.Outcome{Pushed: rep.Pushed}
	})
}

// checkResult refuses a result to push whose image IDs aren't image IDs.
func checkResult(res api.Result) error {
	if res.Image != "" {
		if err := digest.SHA256.Validate(res.Image); err != nil {
			return fmt.Errorf("the result's image ID %q: %w", res.Image, err)
		}
	}
	for i, m := range res.Index {
		if err := digest.SHA256.Validate(m.Image); err != nil {
			return fmt.Errorf("the image ID %q of the result's manifest %d: %w", m.Image, i, err)
		}
	}
	return nil
}
