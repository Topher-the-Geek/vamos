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
	// ExecPlatform, if set, is the execution platform that clients configure graphs for unless they ask for
	// another, which Capabilities reports; it must be one of Platforms, so that it is always offered.
	ExecPlatform *imgspecv1.Platform
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
	// State is the directory the coordinator keeps its jobs in, so that a restart resumes them; "" keeps them in
	// memory alone, and a restart loses them.
	State string
	// Warn is told what goes wrong without failing a job, such as a job's record that can't be written; nil drops it.
	Warn func(error)
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
	if cfg.Warn == nil {
		cfg.Warn = func(error) {}
	}
	return cfg
}

// restartPositions is how many event positions each run of a job may give: a run that a restart resumes gives
// positions from its number of restarts times this, so that a watcher resuming from an earlier run's position
// meets a gap that says the coordinator restarted.
const restartPositions = 1 << 40

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
	journal  *journal  // the jobs on disk; nil without Config.State
}

var (
	_ api.Executor         = (*Coordinator)(nil)
	_ remote.Workers       = (*Coordinator)(nil)
	_ remote.SubmissionLog = (*Coordinator)(nil)
)

// New returns a coordinator over the stores content and records, configured by cfg, running until Close.
// With cfg.State, it resumes the jobs that an earlier run kept there, as recover describes.
func New(content cache.ContentStore, records cache.RecordStore, cfg Config) (*Coordinator, error) {
	cfg = cfg.withDefaults()
	if p := cfg.ExecPlatform; p != nil {
		normalized := platforms.Normalize(*p)
		if name := platforms.Format(normalized); !slices.Contains(names(cfg.Platforms), name) {
			return nil, fmt.Errorf("the execution platform %s isn't one of the platforms offered, %v", name,
				names(cfg.Platforms))
		}
		cfg.ExecPlatform = &normalized
	}
	var id [8]byte
	rand.Read(id[:])
	c := &Coordinator{
		cfg:      cfg,
		instance: hex.EncodeToString(id[:]),
		content:  content,
		records:  records,
		jobs:     &sched.Jobs{},
	}
	var kept []*record
	if cfg.State != "" {
		j, instance, recorded, err := openJournal(cfg.State, cfg.Warn)
		if err != nil {
			return nil, fmt.Errorf("opening the coordinator's state in %s: %w", cfg.State, err)
		}
		c.journal, c.instance, kept = j, instance, recorded
		c.jobs.OnEvict(func(id api.JobID) {
			if err := j.evict(id); err != nil {
				cfg.Warn(fmt.Errorf("recording that the job %s expired: %w", id, err))
			}
		})
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
	if err := c.recover(kept); err != nil {
		c.Close()
		return nil, fmt.Errorf("resuming the jobs recorded in %s: %w", cfg.State, err)
	}
	// The jobs' sweep runs apart from the leases', since its evictions write records, so that no write delays the
	// loss of a lease; a sweep asked for while one runs is folded into the next.
	sweep := make(chan struct{}, 1)
	go c.pool.sweep(ctx, func() {
		select {
		case sweep <- struct{}{}:
		default:
		}
	})
	go func() {
		for {
			select {
			case <-sweep:
				c.jobs.Sweep()
			case <-ctx.Done():
				return
			}
		}
	}()
	return c, nil
}

// recover restores the jobs that an earlier run recorded: a build that hadn't ended runs again, from the start,
// and what finished before is a cache hit, while what its attempts were running is lost with them; a finished job's
// outcome is readable again, its terminal event where it was, as if its client had just stopped watching; a
// tombstone is remembered as expired. A push or a prune that hadn't ended fails, since what it wrote before the
// restart is unknown, as does a job that was cancelled, which no restart revives, and a build that was released,
// which no client wants. What recovery changes is recorded before the job runs or is readable, and a failure to
// record it fails the recovery, so that no restart finds a job other than the last one left it.
func (c *Coordinator) recover(kept []*record) error {
	for _, r := range kept {
		var err error
		switch {
		case !r.Evicted.IsZero():
			c.jobs.RememberEvicted(r.ID)
		case r.Outcome != nil:
			err = c.restore(r)
		case r.Cancelled:
			err = c.conclude(r, sched.Failed(context.Canceled))
		case r.Push != nil:
			outcome := sched.Failed(fmt.Errorf("the coordinator restarted while the push ran, so whether the "+
				"destination %s was written is unknown; check it, or push again", r.Push.Destination))
			outcome.Failure.Nodes = []graph.Ref{r.Push.Result.Node}
			err = c.conclude(r, outcome)
		case r.Prune:
			err = c.conclude(r, sched.Failed(errors.New("the coordinator restarted while the prune ran, so the "+
				"build cache may be pruned in part; prune again")))
		case r.Released:
			err = c.conclude(r, sched.Failed(sched.ErrWithdrawn))
		default:
			err = c.resume(r)
		}
		if err != nil {
			return fmt.Errorf("the job %s: %w", r.ID, err)
		}
	}
	return nil
}

// resume runs the build r recorded again, its events past every position its earlier runs gave, once its new number
// of restarts is recorded, so that a later restart never gives the same positions again; a build the jobs table
// refuses, as when fewer builds are allowed than before, fails.
func (c *Coordinator) resume(r *record) error {
	restarts := r.Restarts + 1
	spec := sched.Spec{ID: r.ID, Build: true, First: restarts * restartPositions,
		Resumed: restarts * restartPositions, Ending: c.ending(r.ID)}
	reserved, err := c.jobs.Reserve(spec)
	if err != nil {
		return c.conclude(r, sched.Failed(fmt.Errorf("the coordinator restarted and couldn't resume the build: %w",
			err)))
	}
	// A record from before builds ran natively on several platforms has none: it runs natively on those configured,
	// which a restart keeps, as the version that recorded it would have on the execution platform alone.
	native := r.Native
	if native == nil {
		native = c.configuredNames()
	}
	if err := c.journal.update(r.ID, func(r *record) { r.Restarts, r.Native = restarts, native }); err != nil {
		reserved.Cancel()
		return fmt.Errorf("recording that it resumed: %w", err)
	}
	_, err = reserved.Start(c.build(*r.Build, native))
	return err
}

// conclude ends the job r recorded with outcome, which the restart decided: recorded first, its terminal event past
// every position its earlier runs gave, so that a watcher from one of those meets a gap that says the coordinator
// restarted.
func (c *Coordinator) conclude(r *record, outcome api.Outcome) error {
	restarts := r.Restarts + 1
	done, ended := restarts*restartPositions, time.Now()
	if err := c.journal.update(r.ID, func(r *record) {
		r.Restarts, r.Outcome, r.Done, r.Ended = restarts, &outcome, done, ended
	}); err != nil {
		return fmt.Errorf("recording how it ended: %w", err)
	}
	r.Restarts, r.Outcome, r.Done, r.Ended = restarts, &outcome, done, ended
	return c.restore(r)
}

// restore makes the job r recorded, which has ended, readable: its terminal event where it was, and a gap from a
// position of an earlier run saying that the coordinator restarted.
func (c *Coordinator) restore(r *record) error {
	spec := sched.Spec{ID: r.ID, Build: r.Build != nil, First: r.Done, Resumed: r.Restarts * restartPositions}
	j, err := c.jobs.Restore(spec, *r.Outcome, r.Ended)
	if err != nil {
		return err
	}
	if r.Released {
		j.Release()
	}
	return nil
}

// RetainEvents makes the coordinator retain n events of each job for watchers, rather than the default;
// it is for tests, before any job is started.
func (c *Coordinator) RetainEvents(n int) {
	c.jobs.RetainEvents(n)
}

// Close ends every job and attempt and waits for the jobs to end; a job kept on disk is recorded as it was before,
// so that the next run resumes it rather than finding it ended by the shutdown.
func (c *Coordinator) Close() error {
	c.journal.close()
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

// Capabilities reports the platforms offered, by configuration and by the workers registered, the execution platform
// configured for clients, the graph versions run, the instance and its grace period.
func (c *Coordinator) Capabilities(ctx context.Context) (api.Capabilities, error) {
	generation, err := c.generation(ctx)
	if err != nil {
		return api.Capabilities{}, err
	}
	// The clone keeps every caller from changing the configuration.
	return api.Capabilities{
		Platforms:     c.platforms(),
		ExecPlatform:  c.cfg.ExecPlatform,
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
		return c.submitPush(ctx, *req.Push)
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
	native := c.platformNames()
	return c.begin(ctx, record{Build: g, Native: native}, sched.Spec{Build: true}, c.build(*g, native))
}

// build returns the work of a build of g, whose actions run natively on the platforms native names, as they were
// offered when it was submitted and as its record has them, and on those offered later, as offer says.
func (c *Coordinator) build(g api.GraphRequest, native []string) func(ctx context.Context,
	ev sched.Events) api.Outcome {
	return func(ctx context.Context, ev sched.Events) api.Outcome {
		// The generation is checked once the build is open, since no prune starts then: checked before, a prune
		// could run between the check and the build's start.
		if err := c.checkGeneration(ctx, g.Generation); err != nil {
			return sched.Failed(err)
		}
		results, err := c.run(ctx, g, native, ev)
		if err != nil {
			return sched.Failed(err)
		}
		return api.Outcome{Results: results}
	}
}

// begin starts a job as spec says, which runs work, with r its record, which the journal writes once the job is
// admitted and before it starts, so that a job acknowledged is a job that a restart resumes and a job refused is
// never recorded; the record names the submission that ctx carries.
func (c *Coordinator) begin(ctx context.Context, r record, spec sched.Spec,
	work func(ctx context.Context, ev sched.Events) api.Outcome) (api.JobID, error) {
	spec.ID = sched.NewJobID()
	spec.Ending = c.ending(spec.ID)
	reserved, err := c.jobs.Reserve(spec)
	if err != nil {
		return "", err
	}
	r.ID, r.Submission = spec.ID, remote.Submission(ctx)
	if err := c.journal.put(&r); err != nil {
		reserved.Cancel()
		return "", fmt.Errorf("recording the job: %w", err)
	}
	id, err := reserved.Start(work)
	if err != nil {
		// Only a shutdown since the reservation refuses the start.
		if rerr := c.journal.remove(spec.ID); rerr != nil {
			c.cfg.Warn(fmt.Errorf("forgetting the job %s, which never started: %w", spec.ID, rerr))
		}
		return "", err
	}
	return id, nil
}

// ending returns what records the terminal event of the job id, its outcome and position, before any watcher sees
// it, so that a client that saw the job end finds the same outcome after a restart; nil without a journal.
// A failure to record it is reported to Warn: the job ends in memory regardless, and a restart then runs a build
// again, its finished work cached, and ends a push as one whose write is unknown.
func (c *Coordinator) ending(id api.JobID) func(done api.Event) {
	if c.journal == nil {
		return nil
	}
	return func(done api.Event) {
		ended := time.Now()
		if err := c.journal.update(id, func(r *record) {
			r.Outcome, r.Done, r.Ended = done.Outcome, done.Position, ended
		}); err != nil {
			c.cfg.Warn(fmt.Errorf("recording how the job %s ended: %w", id, err))
		}
	}
}

// Submitted returns the job that the submission id made, if the coordinator holds it, as remote.SubmissionLog asks,
// so that a client retrying a submission that an earlier run accepted gets the job that run started.
func (c *Coordinator) Submitted(id string) (api.JobID, bool) {
	return c.journal.submitted(id)
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

// platforms returns the platforms the deployment offers: by configuration, and those of the workers registered now.
func (c *Coordinator) platforms() []imgspecv1.Platform {
	offered := slices.Clone(c.cfg.Platforms)
	for _, p := range c.pool.platformsOffered() {
		if !slices.ContainsFunc(offered, func(o imgspecv1.Platform) bool {
			return platforms.Format(platforms.Normalize(o)) == platforms.Format(p)
		}) {
			offered = append(offered, p)
		}
	}
	return offered
}

// platformNames returns the platforms offered, as text, normalized and sorted.
func (c *Coordinator) platformNames() []string {
	return names(c.platforms())
}

// configuredNames returns the platforms offered by configuration, as text, normalized and sorted.
func (c *Coordinator) configuredNames() []string {
	return names(c.cfg.Platforms)
}

// names returns ps as text, normalized, sorted and without repeats.
func names(ps []imgspecv1.Platform) []string {
	names := []string{}
	for _, p := range ps {
		names = append(names, platforms.Format(platforms.Normalize(p)))
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// offer returns the sched.Options.Offered of the build id, whose actions run natively on the platforms native
// names: a platform the deployment offers now, by configuration or by a worker registered, joins them, recorded
// before the scheduler gives any action to it, so that a restart resumes the build with it. A platform never leaves
// them, so that what ran natively never becomes emulated: a build that reaches a platform whose workers have gone
// waits for one, saying so, until it is cancelled. A record that can't be written is reported to Warn, as the other
// records of a running build are, and the platform joins them all the same, which the next record carries.
func (c *Coordinator) offer(id api.JobID, native []string) func(platform string) bool {
	native = slices.Clone(native)
	return func(platform string) bool {
		if !slices.Contains(c.platformNames(), platform) {
			return false
		}
		native = append(native, platform)
		slices.Sort(native)
		if err := c.journal.update(id, func(r *record) { r.Native = slices.Clone(native) }); err != nil {
			c.cfg.Warn(fmt.Errorf("recording that the build %s runs commands natively on %s: %w", id, platform, err))
		}
		return true
	}
}

// run runs the graph req names: reads it from the content store, and schedules it across the workers, each action
// on a worker of its image's platform if native names it or the deployment offers it, as offer says.
func (c *Coordinator) run(ctx context.Context, req api.GraphRequest, native []string, ev sched.Events) (
	_ []api.Result, err error) {
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
	// An action whose image's platform the build runs natively waits for a worker of it rather than being emulated,
	// even if none is registered now, as after a restart.
	opts := sched.Options{Rebuild: req.Rebuild, Timestamp: req.Timestamp, Emulate: req.Emulate, OptIn: true,
		Bound: c.cfg.Bound, Native: native, Offered: c.offer(jobOf(ctx), native)}
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
// The cancellation is recorded first, so that a restart before the job ends doesn't resume it; a failure to record
// it is reported to Warn, since the job is cancelled regardless, and a restart then resumes it until it expires.
func (c *Coordinator) Cancel(_ context.Context, id api.JobID) error {
	j, err := c.jobs.Lookup(id)
	if err != nil {
		return err
	}
	if err := c.journal.update(id, func(r *record) { r.Cancelled = true }); err != nil {
		c.cfg.Warn(fmt.Errorf("recording that the job %s was cancelled: %w", id, err))
	}
	j.Cancel()
	return nil
}

// Release lets go of the job id: a finished build is evicted at the next sweep, and a running one's demand is
// withdrawn.
func (c *Coordinator) Release(_ context.Context, id api.JobID) error {
	return c.release(id)
}

// release releases the job id, recording it, so that a restart evicts it rather than holding it for a window;
// a failure to record it is reported to Warn, since the job is released regardless.
func (c *Coordinator) release(id api.JobID) error {
	j, err := c.jobs.Lookup(id)
	if err != nil {
		return err
	}
	if err := c.journal.update(id, func(r *record) { r.Released = true }); err != nil {
		c.cfg.Warn(fmt.Errorf("recording that the job %s was released: %w", id, err))
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
func (c *Coordinator) Prune(ctx context.Context) (api.JobID, error) {
	if since := time.Since(c.started); since < c.cfg.Lease {
		return "", fmt.Errorf("%w: the coordinator started %s ago, and an attempt of an earlier instance may run "+
			"until a lease, %s, has passed", api.ErrSettling, since.Round(time.Second), c.cfg.Lease)
	}
	// Only an open job makes attempts, and Start refuses the prune while one is open, so no attempt starts between
	// this check and the prune.
	if n := c.pool.liveAttempts(); n > 0 {
		return "", fmt.Errorf("%w: %d attempts are still live on the workers, or waiting for one", api.ErrSettling, n)
	}
	return c.begin(ctx, record{Prune: true}, sched.Spec{Exclusive: true}, func(ctx context.Context,
		ev sched.Events) api.Outcome {
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
func (c *Coordinator) submitPush(ctx context.Context, req api.PushRequest) (api.JobID, error) {
	if _, err := sched.ParseDestination(req.Destination); err != nil {
		return "", err
	}
	return c.begin(ctx, record{Push: &req}, sched.Spec{}, func(ctx context.Context, ev sched.Events) api.Outcome {
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
