# Build cache and traces

vamos caches target results and reuses them when their effective inputs are unchanged.
Delivery commands use the same cache as `build`.

## What changes a result

Inputs include the base image, configured steps and arguments, and the contents, modes, and links of added files.
An added task's timestamp is also an input, even when its file contents are unchanged.
Target names and source modification times are not inputs.
Touching a file or renaming a target therefore does not force it to run again.
Changing a parameter matters when it changes what the target actually does or consumes.

The CLI reports reuse and replays a cached target's previous output:

```text
layer "tools": cached
task "test": cached
task "compile": building
layer "app": building
```

Test and lint successes are cached too.
A failed target does not produce a successful reusable result,
but dependencies that succeeded before its failure remain cached.

## Refreshing results

Cached results are reused even when rerunning would observe a changed external service,
such as a package repository with newer dependencies.
`push`, `load`, `save`, and `export` do not force cached targets to run again.

To refresh a target explicitly:

```sh
vamos build packages --rebuild
```

This rebuilds `packages`, reuses its dependencies when possible, and replaces its cached result.
Consumers run again when the result they consume changes.
To refresh a dependency too, name it explicitly.

Pulls remember what their tag resolved to and reuse it without contacting the registry.
If `node` is a pull target, refresh its tag with:

```sh
vamos build node --rebuild
```

Then run the desired build or delivery command.
`prune` does not refresh tag resolutions.

For checks that must execute every CI run:

```sh
vamos build lint test --rebuild
```

## Storage and pruning

Cached layers are container-store images named `localhost/vamos-cache:<id>`, by their image ID;
pulled images carry that name too, beside the name of the digest they were pulled by.
Other cached data is beside the container store, normally in `/var/lib/containers/vamos`.
A volume on `/var/lib/containers` preserves both when vamos runs in a disposable container.
See the [tutorial](../tutorial.md#2-set-up-vamos) for a Docker setup with a named volume.

Cache results remain until explicitly pruned:

```sh
vamos prune
```

This removes cached layer images and other build results, while keeping pulled images and recorded tag resolutions.
The next build recreates the needed results.
A prune is refused while another vamos is using the build cache, and a build is refused while a prune runs;
run it again when the other finishes.

With `--coordinator`, the build cache is the coordinator's: sources, trees, logs and results live in its
`--cache-dir`, and built layers in the cache registry repository its workers push to, tagged by image ID.
The registry must have deletion enabled, the coordinator's credentials must allow deleting there and the workers'
pushing, and the space of deleted layers comes back only when the registry's own garbage collection runs, which is
the operator's to schedule.
The workers' own container stores only speed things up; a cache hit is checked against the registry, so a worker can
be replaced at any time.
`vamos prune --coordinator URL` empties the coordinator's results, files and the cache repository's tags, keeping
recorded tag resolutions and the pull-through cache's contents.
It is refused while any build or push is open, including a finished build whose client is still delivering it,
while attempts are still ending on the workers, and for one lease after the coordinator started; run it again
when they have finished.
A build whose sources were captured before a prune is refused when submitted, with a message to run it again.

## Timestamps

Files a target adds, and the resulting image, use the latest timestamp from its base and any tasks whose files it adds.
Pulled images supply their creation times; built bases and added tasks supply their result timestamps.
This helps the same inputs produce the same image despite different build times.

`--timestamp` sets a minimum time for newly executed targets:

```sh
vamos build app --timestamp 2026-01-01T00:00:00Z
vamos build app --timestamp 1767225600
```

Both examples name the same UTC instant.
The flag is not a cache input, so a cached result keeps its previous timestamp.
To apply a new timestamp to a cached target, explicitly rebuild it:

```sh
vamos build app --rebuild --timestamp 2026-01-01T00:00:00Z
```

Dependencies may still be reused with their earlier timestamps.
Rebuilding a task with a changed timestamp invalidates targets that add its files,
even when the files' contents are unchanged.
Exported task files receive the task's timestamp.

## Traces

`--trace <file.json>` is available on `build`, `push`, `load`, `save` (including its `write` alias), and `export`:

```sh
vamos build lint test app --trace build-trace.json
vamos load app app:dev --trace load-trace.json
```

Open the trace in [Perfetto](https://ui.perfetto.dev).
The viewer reads the file in your browser without uploading it.

The trace shows BUILD loading and glob capture, each pull and target, execution steps, cache activity,
and image delivery.
Arrows connect the results of globs, pulls, and targets to the steps that consume them.
Spans include details such as file counts, byte sizes, digests, and image IDs.

Once tracing starts, vamos attempts to write the trace even if BUILD loading, execution, or delivery fails.
Errors caught before tracing starts, such as an invalid image name or a failed destination check, produce no trace.
Writing the trace replaces an existing file at its path.
The trace path must be different from a `save` archive and outside an `export` destination folder.
