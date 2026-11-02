# Design

## Context

See proposal.md for why, and specs/pull-pinning/spec.md for the behavior.
What shapes the approach:

- `graph.Pull` is `{Image, Platform}`, with `Image` a name and tag; the graph is the load/run boundary and
  carries no lockfile.
- A pull runs as a `KindResolve` action. The scheduler already pins every platform of one tag to the index the
  first resolved in a job, through `Action.Index` (`s.pinned` in `internal/sched/scheduler.go`), and the runner
  (`internal/run/pull.go`) resolves a given index's image for a platform, from the store by digest when it holds it.
  A digest from the lockfile is the same thing, known before the job starts.
- Tag records (`cache.TagRecord`) are the executor's memory of what a tag named. They stay, for `--pins=resolve`.
- Workers reach upstream registries only through pull-through caches, by digest; the client reaches registries with
  its own credentials and `--tls-verify` already, for `--push-from=client`.
- `go.podman.io/image/v5` v5.41.2 provides `docker.GetRepositoryTags`, which follows every page of the tag list,
  and `docker.GetDigest`, which asks with HEAD, which Docker Hub doesn't count as a pull.
  Neither applies `registries.conf` mirrors, so both go to the image's own registry.
- `cli.Config` already injects what scenarios replace: `Fs` and `Store`.

## Goals / Non-Goals

**Goals:**

- A pinned build asks no registry anything but the bytes of a digest, on every executor.
- The tag heuristic is specified by tables of examples that run with no registry, no buildah and no network.
- A change to one pin touches only that pin's lines of the lockfile, so its review diff is that pin.

**Non-Goals:**

- A `digest=` argument to `pull()`: the lockfile is the only place a pin is written, so that `vamos update` can
  rewrite every pin however its pull was declared, by a macro or in a loop.
- Changing a BUILD file's tag, as moving `1.3` to `1.4` would: newer releases are reported, and a person edits
  the tag; a later `--bump` could do it.
- Resolving tags through a pull-through cache, for networks that reach only it.
- A lockfile per package, or a setting in `WORKSPACE`.

## Decisions

### 1. The lockfile is JSON, one field per line

```json
{
  "version": 1,
  "pulls": {
    "docker.io/library/node:24-alpine": {
      "tag": "24.11.1-alpine",
      "digest": "sha256:9f3c..."
    },
    "ghcr.io/me/base:1.3": {
      "tag": "1.3.2",
      "digest": "sha256:1a2b..."
    }
  }
}
```

It is written by `encoding/json`'s `MarshalIndent` with two spaces and a trailing newline;
Go sorts map keys, so the same pins always give the same bytes, and parsing needs no parser of vamos's own.

Keyed by the full image name and tag as the BUILD file writes them, after normalization, as `graph.Pull.Image`
gives it, so that two pulls of one tag share an entry and renaming a target changes nothing.
The digest is of the manifest the resolved tag named, an index or an image; selecting a platform's image from it
stays in the runner, as for a tag record, so one entry serves every platform.
Alternative considered: an aligned text table of three columns, one line per pin.
Aligning columns invites reformatting the whole file when a long name comes or goes, which makes its diff useless
for review; JSON's field per line never re-aligns another entry, it extends by adding fields, and it needs no new
dependency. A syntax error's byte offset is turned into a line and column for the message.

`version` is 1. A reader ignores fields it doesn't know, so a field can be added within a version;
a lockfile of a greater version is refused, so that an older vamos never rewrites it and drops what it can't read.
It is written whole, to a temporary file renamed over it, after re-reading it, so that two builds recording
different pins at once both keep theirs.

### 2. Load attaches the pin; the graph carries the digest

`internal/load` finds `vamos.lock` beside the workspace root it already locates, parses it once, and gives each pull
its entry. `graph.Pull` gains `Digest`, and `graph.Version` becomes 4.
The scheduler seeds `Action.Index` from `Pull.Digest`, so a pinned pull resolves as the second platform of a tag does
today: the runner selects the platform's image from that index, or checks the image's platform, and never reads or
writes a tag record. What it finds out about the pinned manifest, the manifest itself and each platform's image,
it keeps in a record of the pin, named by the image's name and the digest, in the record store beside tag records,
so that a cached pinned build asks no registry anything; a digest names the same manifest forever, so that record is
never stale. The resolve's action is marked pinned, so that it never shares an attempt with a tag's resolve of the
same index. Workers carry out resolves differently, a pinned one never touching a tag record, and `Again` goes,
so `sched.ActionVersion` becomes 11, which a coordinator requires its workers to run, so that a worker of 10 never
takes a pin's index for its tag's.

Alternative considered: keeping the lockfile client-side and rewriting tag records in the cache before the job.
That leaves the cache as the source of truth and, on a coordinator, means the client writes the coordinator's records.

### 3. Resolving happens on the client, through a seam

`cli.Config` gains `Registry`:

```go
type Registry interface {
    Tags(ctx context.Context, repo reference.Named) ([]string, error)
    Digest(ctx context.Context, ref reference.NamedTagged) (digest.Digest, error)
}
```

Its default wraps `docker.GetRepositoryTags` and `docker.GetDigest` with the client's `types.SystemContext`.
`vamos update` and `--pins=record` both call it before loading builds the graph's digests:
list the tags, choose the tag resolved to (decision 4), ask its digest.
`--pins=resolve` leaves the pull's `Digest` empty, and the executor resolves the tag as today.
`--pins=require` checks the pins after loading, naming every pull without one, before submitting anything.

The `update` feature's scenarios set `Registry` to a world-owned fake filled by
`GivenTheRegistryListsTags(repo, tags...)`, and start from `newWorld`.
Scenarios over `GivenARegistry` use the default and cover what the fake can't: pagination, an unreachable registry,
and that a pinned build fetches by digest.

### 4. Tags are parsed as versions by a small parser of vamos's own

Tags such as `24-alpine` and `1.3.2-bookworm` aren't semver, so no semver library applies.
A tag reads as `[v]N[.N[.N]][-suffix]`, with up to three numbers; otherwise it isn't a version.
A suffix that begins `alpha`, `beta`, `rc` or `pre`, with an optional `.` and number, marks a prerelease,
and the rest of the suffix is its variant, as `1.4-rc1-alpine` is a prerelease of `1.4` with the suffix `alpine`.
Tags compare only within the same `v` prefix and suffix, number by number.

- **Resolved tag**: the newest stable tag whose leading numbers are the pulled tag's; the tag itself if none is.
- **Newer minor or patch**: the newest stable tag with the same major number, newer than the resolved tag,
  that the pulled tag doesn't cover.
- **Newer major**: the newest stable tag with a greater major number.
- **Prerelease**: the newest prerelease newer than the resolved tag.

This is a pure function from a tag and a list to a result, kept in one file of `internal/cli` or a small package,
and specified only through the scenarios' tables, as `testing.md` requires.

### 5. `--pins` is a persistent flag of the commands that build

Declared once, beside `--tls-verify`, on `build`, `push`, `publish`, `load`, `save` and `export`;
its default is `VAMOS_PINS` where set, else `record`, so the flag outranks the environment.
`update` doesn't take it.

### 6. `--rebuild` stops resolving pulls

The scheduler no longer passes `again` for a named pull; a rebuilt pull reports
`pulls are refreshed by vamos update` and completes from its pin or record.
`againTags` and the runner's `Again` go away.

## Risks / Trade-offs

- [`record` writes a file during a build, which users may not expect.] → It is the default because the first build
  of a project then pins it; the report says when it adds an entry, and CI's `require` never writes.
- [The prerelease markers are a heuristic: a variant named `rc-builder` reads as a prerelease.] → Notes are only
  advisory, and the markers are one list, extended by adding a row to a table.
- [A large tag list, like `node`'s, costs pages of requests on every update and every recorded pin.] → Tags are
  listed once per repository per command, and pinned builds list nothing.
- [A coarse tag resolves to a fine tag that its own maintainers haven't moved to yet.] → That is the intent;
  the lockfile shows the fine tag, so the review sees it.
- [A registry that lists no tags, or forbids listing, prevents resolving to finer tags.] → Resolving falls back to
  the digest the tag names, and notes that the tags couldn't be listed, rather than failing.

## Migration Plan

Existing projects have no lockfile; under the default `record`, their next build resolves each pull from the
registry, as a fresh cache does today, and writes `vamos.lock`, whose commit then pins them.
Tag records already in caches are no longer read for pinned pulls; `prune` handles them as before.
A project that wants today's behavior sets `VAMOS_PINS=resolve`.

### 7. A dry run that would change the lockfile fails

`vamos update --dry-run` computes the lockfile `vamos update` would write and compares it with the one on disk;
if they differ, it prints its report and returns an error, so vamos exits 1, as for any failure.
A distinct exit status for "would change" was considered and left out: vamos has no exit-status convention but 1,
and the message says which it was.
