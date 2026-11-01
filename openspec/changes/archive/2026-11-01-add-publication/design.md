# Design

## Context

See proposal.md for why, and specs/publication/spec.md for the behavior.
What shapes the approach:

- `load.Load` turns named targets into a `graph.Graph` whose roots are those targets, and the graph is the
  load/run boundary: no Starlark value, and nothing about destinations, crosses it.
  `load.Request.Targets` and `api.Outcome.Results` are already lists, one result per root in order,
  so building several roots in one run already works, as `vamos build a b c` shows.
- Delivery commands share `buildImage` in `internal/cli/cli.go`, which runs a graph of one root
  and then either submits an `api.PushRequest` job or delivers from the client through `finish`.
- `transfer.Push` copies an image or index to a tag, retrying as `containers.conf` says,
  then reads the tag back from the registry it wrote to.
  The pinned `go.podman.io/image/v5` (v5.41.2) can also push with no tag:
  `docker.NewReferenceUnknownDigest` uploads the manifest under its own digest,
  though for an index only once the destination accepts the empty set of signatures the copy stores.
- On a coordinator, `submitPush` makes one job whose single attempt any worker may take
  (`pool.suits` returns true for a push), and a push is never retried on worker loss, since its write may have
  happened. Every open job counts against `MaxBuilds`, and the journal records each job so that a restart can fail
  a push whose outcome is unknown.

## Goals / Non-Goals

**Goals:**

- A publication loads as ordinary roots, so the graph format, `graph.Version` and `sched.ActionVersion` are
  unchanged, and the cache is shared with `build` and `push`.
- One executor job per publication, on every executor, carrying both phases.
- Phase 1 uploads that fail leave no visible effect, and can therefore be retried freely.
- Reuse the existing copy, retry and read-back code rather than adding raw registry calls.

**Non-Goals:**

- Changing `vamos push`, or letting `load` or `save` take several targets.
- Signing images, or attaching SBOMs or attestations.
- Atomicity across repositories: registries offer none; the two phases only shrink the window to the tag writes.
- Deleting uploads that a failed publication leaves untagged; registries' own garbage collection owns them.

## Decisions

### 1. `publication()` is a declaration kind whose references become roots, and its repositories stay with the client

`publication` joins `glob`, `pull`, `layer`, `task` and `index` as a `declaration.kind` in `internal/load`,
declared through the existing `declarer`, so it gets `params`, functions of `ctx` and position-carrying errors
for free.
Its template, a `publicationSpec`, holds `checks`, a list of target references, and `images`,
a list of (repository, target reference) pairs in the dictionary's order.
References resolve with `resolve` as an index's do, and the kind checks happen there:
checks must be tasks, images pulls, layers or indexes.
Any other declaration's reference that resolves to a publication is refused at the same point,
which makes it a root only.

Load gains a `WantPublication` and a `LoadPublication` entry point that returns the graph together with a
`Publication`: for each image, its root's index in the graph and its repository, as a full reference with no tag.
The roots are the checks followed by the images, each image root once even when published to several repositories,
in the publication's configuration, as a dependency's are configured.
`--set` names the publication's own parameters, as it names a target's.
For `WantBuild`, a named publication expands into the same roots and its plan is dropped, which makes
`vamos build <publication>` the dry run.
`LoadPublication` refuses a target that isn't a publication, naming its kind,
and `WantImage` and `WantFiles` refuse a publication, naming `publish` and `build` as the commands that take it.

*Alternative*: encode the publication as a graph node the executor understands.
Rejected: destinations would cross the load/run boundary, the graph version would change,
and a node that is never cached would be a special case for the scheduler.

*Alternative*: key `images` by target rather than by repository.
Rejected: a target's reference, such as a `dep()`, isn't a natural dictionary key,
and keying by repository makes two images in one repository impossible by construction,
while one image in two repositories, a mirror, stays easy.

### 2. A new `api.PublishRequest` job carries both phases

```
api.PublishRequest  { Images: [{Result, Repository}], Tags: []string, Trace }
api.Outcome.Published = &api.Published{ Destinations: [{Repository, Tag, Image | Index, Written bool, Error}] }
```

The job uploads every (result, repository) pair by digest, keeping each manifest written,
and only if all succeeded writes each tag by putting that manifest to `repository:tag`,
in which every blob, and every manifest an index lists, is already present; it then reads the tag back.
A phase-2 failure ends the job with a `Failure`, and with `Published` listing every destination,
written or not, so that the client reports both.
The client checks every tag with the same reference parsing as `push` (`sched.ParseDestination`'s rules) before
loading, and resolves short repository names, so that both sides mean the same registry.

In `internal/transfer`, `Upload` copies to a `NewReferenceUnknownDigest` reference and returns the manifest
written, and `Tag` puts that manifest to a tag through the registry's image destination and reads it back;
both use `Retries`.
`transfer.Push` is unchanged.

*Alternative*: one `PushRequest` job per destination, as the CLI loop would do.
Rejected: N jobs means N admissions against `MaxBuilds`, refusal can come halfway through,
and there is no point at which the executor knows every upload succeeded before tagging.

*Alternative*: write tags by copying `repository@digest` to `repository:tag`.
Rejected: the copy reads its source through any pull mirror that `registries.conf` configures,
which may not hold the upload, and the system's signature policy, which may refuse it,
and it sends each image's config and manifest again, where putting the manifest through the image destination
reuses the same authentication and sends one request per tag.

### 3. Where each phase runs

```
                 phase 1: upload                        phase 2: tag
 run.Local       one repository at a time               one tag at a time
 --push-from     client, one repository at a time       client, one tag at a time
   =client
 coordinator     one attempt per (result, repository)   one attempt with every tag,
                 on any workers, at once                after all uploads succeeded
```

`run.Local` and the client go one at a time because they share the host's network interface,
and running them concurrently would only contend for it.
The coordinator's publish job fans out phase 1 as attempts, through the same `pool.attempt` path as a push,
and phase 2 as one attempt, since tag writes are small requests and one worker writing them all
keeps the outcome in one report.
`remote.PushWork` becomes a work item with a mode, upload or tag, so `pool.suits` still lets any worker take it,
and the coordinator never opens a connection to a destination registry: only workers, with their credentials, do.

Uploads are retried on worker loss, up to `--retries`, since an upload by digest has no visible effect;
the tag attempt is never retried, as a push isn't, since its writes may have happened.

### 4. The coordinator journals a publication as one job

The journal `record` gains `Publish *api.PublishRequest`, set alone, as `Build`, `Push` and `Prune` are.
On restart, a publication that hadn't ended fails as an unfinished push does, saying that whether its tags were
written is unknown and that publishing again finishes it; nothing is resumed.
A publication is one job against `MaxBuilds`, so a busy coordinator refuses it when it is submitted,
before anything is pushed.
`MaxBuilds` counts the jobs that haven't ended, and the publication's graph job is admitted the same way just before,
so a coordinator that is busy refuses the graph job first; a scenario can't make one busy in between,
and a Go test of the coordinator shows the publication's own refusal.

### 5. The `publish` command

`vamos publish <publication> --tag <tag>... [--push-from executor|client] [--set ...]` sits beside `push`
in `internal/cli`, takes the common build flags as `push` does, and requires `--tag` as Cobra's required flag,
so its absence is a command-line mistake.
It runs one graph job with every root, holds it through publishing as `buildImage` holds a delivery,
then submits one `PublishRequest`, or, with `--push-from=client`, publishes through a new `finish.Publish`
that uploads and tags each in turn from the result's locations or the content store, as `finish.Push` reads them.
Each destination prints `Image built! <repository>:<tag> <id>` or `Index built! ...`, in the publication's order,
so that `ThenItSucceeds` sees one sent result per destination.
In a trace, the client's span of publishing holds the executor's publish job, on its own track as a push job's is,
and the job holds one span per upload and one per tag, each with the copy's per-blob spans in the process that
copies: the executor, the client, or a worker, whose spans reach the trace as a push attempt's do.
Since a tag starts only once every upload has ended, the trace shows the two phases in order.
The span names are fixed by the trace feature's scenarios, `TestTrace_ShowsPublishing`,
beside `TestTrace_ShowsDelivery`, rather than by the spec, which says what the trace shows in a user's terms.
Overlap between workers' uploads isn't specified, since it depends on scheduling and worker capacity.

### 6. Specifications in OpenSpec and in `internal/features`

`specs/publication/spec.md` states the requirements and illustrative scenarios.
The executable form is a `publication` feature, `internal/features/publication_test.go`,
whose rules are its tests, named `TestPublication_<Rule>`, worded as the BUILD and CLI references word them,
as `.agents/kb/testing.md` asks.
Failure modes of the coordinator beyond what a user sees, such as worker loss during an upload,
are Go tests in `internal/coord`, as testing.md allows.

### 7. Documentation is part of done

Each fact lands in the one place that owns it, as `.agents/kb/documentation.md` assigns:
the rule in `docs/reference/build.md`; the command, its flags, its two phases and the garbage-collection window
in `docs/reference/cli.md`; how a publication shares the cache, takes `--trace` and holds off a prune
in `docs/reference/cache.md`; publishing an index in the configuration reference's index delivery table;
the two-phase delivery and its transport owners in `.agents/kb/container-builds.md`;
the phase boundary and that only workers contact destination registries in `.agents/kb/architecture.md`;
and each package's contract in its `doc.go`.
Other pages link to those owners rather than repeating them.

The change is not done until every passage of the user guide and the knowledge base that lists the delivery
commands, or describes delivery, covers `publish` where it applies.
A final task checks this by searching for those lists rather than trusting the per-group tasks to have found them all.

## Risks / Trade-offs

- [Registries that garbage-collect untagged manifests, through ECR lifecycle rules or Harbor GC, could remove an
  upload before its tag is written] → the phases are seconds apart; the CLI reference documents the window,
  and a rerun uploads again.
- [Some registries may refuse a manifest upload without a tag] → the docker distribution API and the OCI
  distribution spec allow it, and the library supports it, for an index with the workaround above;
  the scenario against the in-process registry covers the common path, and the error names the repository if one refuses.
- [Phase 2 can still stop partway, leaving some repositories tagged and others not] → every tag is attempted,
  the report lists what was and wasn't written, and a rerun converges, with every blob already uploaded.
- [Tagging reads back from the destination with the destination's credentials, which need read access as
  well as write] → the same is already true of `push`'s read-back; the CLI reference says so.
- [Concurrent uploads from workers can contend at the destination or its rate limits] → bounded by worker capacity;
  retries back off as `containers.conf` says.
- [A publication changes the coordinator's journal format] → `Publish` is a new optional field;
  an older coordinator reading a newer journal sees a record with none of its known kinds set and reports it as
  corrupt (`errCorruptRecord`), so a downgrade with a publication in the journal needs the journal cleared,
  while a newer coordinator reads older journals unchanged.

## Migration Plan

Additive: no existing command, BUILD rule, graph or cache entry changes.
A coordinator and its workers must be upgraded together before clients use `publish` against them;
an older coordinator refuses an unknown request kind at submission, before anything is pushed.
Rollback is removing the use of `publication()` and `vamos publish`.
