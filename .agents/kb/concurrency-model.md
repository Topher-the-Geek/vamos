# Concurrency, callbacks and ownership

`run.Local` walks each job's pulls and targets sequentially, but runs jobs at once.
The scheduler's `walk`, in [sched](../../internal/sched/doc.go), dispatches ready nodes up to `Options.Bound`,
which the local executor sets to one,
so that a bound of one keeps node order; with a larger bound, which only tests use so far, every pull still resolves
before any target, pulls of one tag resolve in order, and once a target fails nothing more is dispatched,
what runs finishes and is recorded, and the first failure is the job's.
`walk` alone writes the scheduler's maps, between dispatches; what it dispatches reads only its request and the stores.
A coordinator, [coord](../../internal/coord/doc.go), runs the same scheduler with a bound of several, its runner a
pool of workers: an attempt is live from assignment until its report, its lease expiring or its kill, and only a
live attempt's report is recorded, while a prune waits for a killed attempt's report or lease, since its worker may
have missed the kill; `pool.mu` guards workers, attempts and the pending queue, and nothing is held
while a worker's events are relayed. A worker, [worker](../../internal/worker/doc.go), renews leases by heartbeat
and fences itself by its monotonic clock, at a fixed fraction of the lease since the last renewing request was sent
whatever request is still in flight, so that the attempt's processes are killed before the coordinator's view of
the lease expires, and the coordinator refuses a request that meets an expired lease without waiting for its sweep;
a heartbeat that times out is a transport failure, which only the fence decides, while an answer that the attempt is
stale kills it at once. `TestWorkerFencesItself` holds the fence.
A build's demand is the client's attachment: `sched.Jobs` counts the watches open on each build and, with a grace
period set, as a coordinator sets it, withdraws the demand of a build unwatched for that long, by closing the channel
`sched.Withdrawn(ctx)` that `walk` checks before each dispatch, so that what runs finishes and publishes while
nothing more starts; cancellation, by contrast, ends the job's context, which detaches it from its attempts in
`Sharing`, and an attempt no job is attached to is cancelled. `Jobs.Sweep`, which the coordinator's pool ticker
runs, applies the grace period and evicts finished jobs a window after their end or their last watch, or at the next
sweep once released, remembering them so that a later call says the job expired.
The pool's `await` asks `sched.Demanded` and `sched.Permitted` every heartbeat interval while an attempt waits for a
worker, and takes the attempt back once no attached job wants it, or no permitting job needs an emulated one, so
that work nobody wants never starts.
`TestJobsExpire`, `TestBuildExpires` and `TestExpiredPendingAttemptEnds` hold this.
Each job runs in a goroutine of its own, with a context that only `Cancel` ends,
and appends its events to a `sched.Job`, which retains a bounded number of them, `defaultRetention`;
watchers read them from any position they hold without blocking the job, a position no longer retained gets
a gap event, and a watcher that falls behind as it reads is dropped with `api.ErrFellBehind`.
Cancellation takes effect between commands, which buildah can't interrupt, and between the files of a checkout,
the records of a replayed log, the directories of a capture and the statements of a BUILD file.
Container copies and stdout/stderr delivery can still involve multiple goroutines.
Do not infer that event consumers are single-threaded from the target scheduler.
The local executor runs several jobs at once and deduplicates the actions in flight by key, in `sched.Sharing`:
an ordinary job that misses the cache attaches to the attempt of that key already running,
including one a forced rebuild started; a forced rebuild, or a fresh resolution of a tag, never attaches
and consults no cache, and its attempt replaces any in flight as the one later jobs attach to;
each attempt returns its own result to every job attached, and the latest write wins the record,
which `sharing.publish` writes once the attempt's artifact is published and before any job gets the result.
The starting job gets the attempt's output live; a job that attached later gets the result's log replayed.
Cancelling a job detaches it; an attempt runs while any job is attached and is cancelled when none is.
The attempt's spans go to the starting job's trace while it is attached;
once it detaches before others, they are dropped, so that no job produces events after its final flush.
`Local.Close` cancels the jobs still running and waits for them, so that their containers and pending files
are removed, and reports what failed.
Parallel targets within one job are still sequential; that is a separate decision.

Concurrency contracts belong with their symbols, not in a parallel list of locks here.
The relevant owners are `sched.Events`, `spanBatch`, `blobTracker`, `cache.LogRecorder`,
`tracing.Recorder`, `tracing.Active`, `cli.eventPrinter` and `cli.traceSession`.
Their files are routed by the corresponding package `doc.go` files.
Changes to callbacks must inspect both caller and consumer: data may be borrowed, callbacks may arrive in parallel,
and invoking user code while holding an internal lock changes the deadlock and latency behavior.

The directory store publishes with filesystem operations rather than a process-wide build lock.
It does not provide a transaction over a graph, all its blobs and container images.
A prune and every other use of the directory exclude each other, across processes as within one:
an open `cache.Directory` holds an advisory lock on a file in it shared, from the client's source capture through
finish, since the client and the local executor share one handle, and `Directory.Prune` converts that handle's
lock to exclusive for the wipe, removing the cached images from the container store under it too.
Both take the lock without waiting, so a prune that meets an open store and a use that meets a prune each fail
at once, naming the other; the executor also refuses a prune while another of its jobs runs.
The lock is one host's kernel lock, not a lease: it survives nothing, and a shared network store wouldn't honour it.
Missing cached content is still handled by reuse checks, not coordinated reclamation.
The commit/discard contracts are on `cache.pendingFile` and `cache.LogRecorder`.

Trace transport crosses several owners: the client attaches `tracing.CallContext` to executor requests,
the executor emits completed spans through `sched.Events`, which a job turns into `api.EventSpans` events,
and `cli.traceSession` aligns clocks, by `api.Capabilities.Clock`, before writing the trace.
Each job must finish producing events before its final flush and terminal event.
Inspect `forCall` and `transfer.Image` when adding asynchronous work;
a context passed to a local method is not a remote trace protocol.

Tracing should do no trace-only computation when disabled; guard argument preparation with `tracing.Enabled`.
Track nesting, flow endpoints and timestamp encoding are owned by [tracing](../../internal/tracing/doc.go).
`blobTracker` in [transfer.go](../../internal/transfer/transfer.go) adapts incomplete library progress events;
those events are not a reliable byte-level network audit (a config may be reported copied even when already present).
`TestTracePerfetto` in [trace_test.go](../../internal/cli/trace_test.go) verifies the importer's view,
beyond checking the JSON ourselves.
