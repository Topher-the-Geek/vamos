# Proposal

## Why

A `pull()` names a tag, and what that tag resolved to is remembered only in the build cache, where it differs
between a laptop, CI and a fresh coordinator, is lost to a new cache, and appears in no diff:
nothing in the project says which image a build used, and refreshing it with `--rebuild` changes it silently.
Moving the pin into the project, under version control, makes every build of a commit use the same images,
makes each upgrade a deliberate, reviewed change with a history, as compliance may require,
and gives vamos the place to say when a newer release of a tag exists.

## What Changes

- A lockfile, `vamos.lock`, at the workspace root, records for each pulled `image:tag` the tag it resolved to
  and the digest of the manifest that tag named.
  It is JSON, with a version, so that it can gain fields later, and is written one field per line,
  so that a change to one pin changes only that pin's lines.
  vamos writes it; people review it, and don't edit it.
  A pull with an entry uses that digest, without asking any registry.
- A tag is resolved to the newest tag it covers:
  a pull of `1.3` resolves to the newest stable `1.3.x` the registry lists, with the same suffix,
  as `24-alpine` resolves to the newest `24.x.y-alpine`, falling back to the tag itself when it covers no other.
  The lockfile records the tag resolved to beside the one written.
- A new command, `vamos update [pull...]`, resolves every pull's tag, or those named, against its upstream
  registry, rewrites the lockfile, and reports what changed and which tags are superseded.
  `vamos update --dry-run` reports the same, writes nothing, and fails if the lockfile would change,
  so that CI can check that the pins are current.
- Updating reports, for each pull, a newer minor or patch release, and a newer major one,
  each the newest of its kind, among the tags with the same suffix and `v` prefix;
  a newer prerelease, and a tag it can't compare as a version, such as `latest`, are noted.
- A new flag, `--pins=<policy>`, with the environment variable `VAMOS_PINS`, says what a command that builds
  does with a pull that has no entry in the lockfile:
  `record` resolves it and adds the entry, `resolve` resolves it and keeps it only in the build cache,
  as vamos does today, and `require` fails, naming every pull without one, before anything runs.
  CI sets `require`.
- **BREAKING**: `vamos build <pull> --rebuild` no longer resolves the pull's tag again; `vamos update` does.
- Resolving and listing tags go to the upstream registry from the client, with the client's credentials,
  never through a pull-through cache, whose answer about a tag can be stale;
  the images themselves are still fetched by digest, through the pull-through cache where there is one.

## Capabilities

### New Capabilities

- `pull-pinning`: the lockfile and what a pull does with its entry; how a tag is resolved to the tag it covers;
  `vamos update` and its report of changed pins and superseded, prerelease and incomparable tags;
  and the `--pins` policy for a pull without an entry.

### Modified Capabilities

None: the only existing specification is `publication`, whose requirements don't change.
What `pull()` does today is specified only in `internal/features/pull_test.go`, whose `--rebuild` scenarios change.

## Impact

- Command line, in `internal/cli`: the `update` command and its `--dry-run`; the `--pins` flag and `VAMOS_PINS`
  on every command that builds; `cli.Config` gains a registry seam, listing a repository's tags and giving the
  digest a tag names, which the client implements against the upstream registry and scenarios replace.
- Loading, in `internal/load`: reading `vamos.lock` from the workspace root, and the pins of the pulls loaded.
- Pulls, in `internal/sched` and `internal/run`: a pinned pull resolves its digest's index or image for the
  platform, as a tag record's does today, without resolving a tag; `--rebuild` on a pull does nothing.
  The cache's tag records remain for `--pins=resolve`.
- Behavior specifications: a new `update` feature in `internal/features`, whose tag-matching rules are tables of
  examples over the registry seam, with no registry, and a few scenarios over a real one for its failures;
  `pull_test.go`'s rules for reusing resolved tags and `--rebuild` change.
- Documentation: the BUILD, CLI, cache and configuration references; the troubleshooting row for a moved tag;
  `.agents/kb/storage.md` for what the lockfile and tag records each hold; and the `doc.go` of each package touched.
