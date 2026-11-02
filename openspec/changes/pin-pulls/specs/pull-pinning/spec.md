# Spec Delta

## Purpose

Lets a project pin each pulled image to a digest recorded in version control, so that every build of a commit uses
the same images, and update those pins in one command that says what changed and which tags have newer releases.

## ADDED Requirements

### Requirement: The lockfile pins each pulled tag

The workspace root's `vamos.lock` SHALL be JSON with a `version` and a `pulls` object keyed by pulled image and tag,
each entry giving the tag it resolved to and the digest of the manifest it named. vamos SHALL write it with sorted
keys, one field per line. A malformed lockfile, or one of a newer version, SHALL fail every command that loads it.

#### Scenario: Updating writes one entry per pulled tag

- **GIVEN** a BUILD file pulling `<registry>/base` at tag `v1` and `<registry>/tools` at tag `2`
- **WHEN** I run `vamos update`
- **THEN** it succeeds and `vamos.lock` holds an entry for each, under sorted keys,
  with the tag and digest each resolved to

#### Scenario: Adding a pin changes only its lines

- **GIVEN** a lockfile pinning `<registry>/a:1` and `<registry>/a-much-longer-repository-name:1`
- **AND** a BUILD file that also pulls `<registry>/an-even-longer-repository-name-than-that:1`
- **WHEN** I run `vamos update`
- **THEN** the lockfile's lines for the two earlier pins are unchanged, and only the new entry's lines are added

#### Scenario: The lockfile lives at the workspace root

- **GIVEN** a workspace whose packages `//backend` and `//ui` both pull `<registry>/base` at tag `v1`
- **WHEN** I run `vamos update` in `ui`
- **THEN** `vamos.lock` beside `WORKSPACE` holds one entry for `<registry>/base:v1`

#### Scenario: A malformed lockfile is refused

- **GIVEN** a `vamos.lock` whose third line is not valid JSON
- **WHEN** I build any target
- **THEN** it fails naming `vamos.lock` with line 3 and its column, and no target ran

#### Scenario: A lockfile of a newer version is refused

- **GIVEN** a `vamos.lock` whose `version` is `2`
- **WHEN** I build any target
- **THEN** it fails naming `vamos.lock` and version 2, and saying to upgrade vamos, and no target ran

### Requirement: A pinned pull uses its pin without asking a registry about its tag

A pull whose image and tag have an entry in the lockfile SHALL use the manifest of that entry's digest,
selecting its platform's image from it if it is an index, and SHALL NOT ask any registry what the tag names.
A digest the registry no longer has SHALL fail, naming the pin and suggesting `vamos update`.

#### Scenario: A moved tag keeps the pinned image

- **GIVEN** a lockfile pinning `<registry>/base:v1` to the index `first`
- **AND** the registry's `v1` now names the index `second`
- **WHEN** I save `app`, a layer on the pull of `<registry>/base:v1`
- **THEN** it succeeds and the image is built on `first`

#### Scenario: Another platform gets the pinned index's image

- **GIVEN** a lockfile pinning `<registry>/base:v1` to an index of images for this machine's platform and another
- **WHEN** I save `app` for the other platform
- **THEN** it succeeds and the image is built on the pinned index's image for that platform

#### Scenario: A pinned digest that is gone says to update

- **GIVEN** a lockfile pinning `<registry>/base:v1` to a digest the registry doesn't have
- **WHEN** I build `app`
- **THEN** it fails naming `<registry>/base:v1` and the digest, and suggesting `vamos update base`

### Requirement: A tag resolves to the newest stable tag it covers

Resolving a tag that reads as a version SHALL choose the newest stable tag the registry lists that the tag covers:
one with the same `v` prefix and suffix whose version's leading numbers are the tag's, as `1.3.2`'s are, and `1.30`'s aren't.
A tag that covers no other, or that isn't a version, SHALL resolve to itself. Coarser tags SHALL be ignored.

#### Scenario: Tags resolve to the newest tag they cover

- **GIVEN** a pull of `<registry>/base` at the example's tag, and a registry listing the example's tags
- **WHEN** I run `vamos update`
- **THEN** the lockfile pins the tag to the example's resolved tag, and its digest

| Tag | Listed | Resolved |
| --- | --- | --- |
| `1.3` | `1.2.9`, `1.3`, `1.3.0`, `1.3.1`, `1.3.2`, `1.4.0` | `1.3.2` |
| `1.3` | `1.3`, `1.4` | `1.3` |
| `1.3.1` | `1`, `1.3`, `1.3.1` | `1.3.1` |
| `24-alpine` | `24-alpine`, `24.11.0-alpine`, `24.11.1-alpine`, `24.11.2-bookworm` | `24.11.1-alpine` |
| `v1.3` | `v1.3`, `v1.3.4`, `1.3.9` | `v1.3.4` |
| `1.3` | `1.3`, `1.3.1`, `1.3.2-rc1` | `1.3.1` |
| `latest` | `latest`, `1.4` | `latest` |

### Requirement: `vamos update` re-resolves pins and reports what changed

`vamos update [pull...]` SHALL resolve the tag of every pull loaded, or only of the pulls named,
rewrite their lockfile entries, and report each entry it added or changed, with the tags and digests before and
after. A full update SHALL remove entries no pull uses. Updating SHALL change no other entry.

#### Scenario: A moved tag is updated and reported

- **GIVEN** a lockfile pinning `<registry>/base:v1` to the digest `first`
- **AND** the registry's `v1` now names `second`
- **WHEN** I run `vamos update`
- **THEN** it succeeds, the lockfile pins `<registry>/base:v1` to `second`, and the report names both digests

#### Scenario: Naming a pull updates only its pin

- **GIVEN** a lockfile pinning the pulls `base` and `tools`, and both their tags moved
- **WHEN** I run `vamos update base`
- **THEN** only the entry for `base` changes

#### Scenario: An entry no pull uses is removed

- **GIVEN** a lockfile pinning `<registry>/old:v1`, which no BUILD file pulls
- **WHEN** I run `vamos update`
- **THEN** the lockfile has no entry for `<registry>/old:v1`, and the report says it was removed

#### Scenario: Naming a target that isn't a pull is refused

- **GIVEN** a BUILD file declaring the layer `app`
- **WHEN** I run `vamos update app`
- **THEN** it fails, saying that `app` is a layer and only pulls are updated, and the lockfile is unchanged

### Requirement: `vamos update --dry-run` writes nothing, and fails if a pin would change

`vamos update --dry-run` SHALL resolve and report exactly as `vamos update` does, SHALL leave the lockfile
unchanged, creating none where there is none, and SHALL fail when `vamos update` would change the lockfile.
Newer releases, prereleases and incomparable tags alone SHALL NOT fail it.

#### Scenario: A dry run fails when a pin would change

- **GIVEN** a lockfile pinning `<registry>/base:v1` to `first`, and the registry's `v1` now names `second`
- **WHEN** I run `vamos update --dry-run`
- **THEN** it reports that `<registry>/base:v1` would change from `first` to `second`
- **AND** it fails, saying `vamos.lock would change; run vamos update`
- **AND** the lockfile is unchanged

#### Scenario: A dry run of current pins succeeds

- **GIVEN** a lockfile pinning `<registry>/base:v1` to what the registry's `v1` names
- **WHEN** I run `vamos update --dry-run`
- **THEN** it succeeds, reporting no change

#### Scenario: Newer releases alone don't fail a dry run

- **GIVEN** a lockfile pinning `<registry>/base:1.3` to what the registry's `1.3` names, and a registry listing `1.4`
- **WHEN** I run `vamos update --dry-run`
- **THEN** it succeeds, reporting the newer minor `1.4`, and the lockfile is unchanged

### Requirement: Updating reports newer releases of a pinned tag

For each tag that reads as a version, updating SHALL report the newest stable tag of a newer minor or patch release
within its major version, and the newest stable tag of a newer major version, each where one exists,
comparing only tags with the same `v` prefix and suffix. A tag it resolved to is not newer.

#### Scenario: Newer releases are reported

- **GIVEN** a pull of `<registry>/base` at the example's tag, and a registry listing the example's tags
- **WHEN** I run `vamos update`
- **THEN** it reports the example's newer releases, or none

| Tag | Listed | Reported |
| --- | --- | --- |
| `1.3` | `1.3`, `1.4`, `1.5` | newer minor `1.5` |
| `1.3.1` | `1.3.1`, `1.3.2` | newer patch `1.3.2` |
| `1.3` | `1.3`, `2.0`, `2.1` | newer major `2.1` |
| `1.3` | `1.3`, `1.4`, `2.0` | newer minor `1.4`, newer major `2.0` |
| `1.3` | `1.3`, `1.3.5` | none: `1.3` resolves to `1.3.5` |
| `1.3.1` | `1`, `1.3`, `1.3.1` | none |
| `24-alpine` | `24-alpine`, `25-alpine`, `26-bookworm` | newer major `25-alpine` |
| `v1.3` | `v1.3`, `v1.4`, `1.9` | newer minor `v1.4` |

### Requirement: Updating notes prereleases and tags it can't compare

Updating SHALL note, without failing, the newest prerelease tag newer than a pull's resolved tag,
such as `1.4-rc1`, and each pulled tag that doesn't read as a version, such as `latest` or `bookworm`,
saying that it can't report newer releases of it.

#### Scenario: A newer prerelease is noted

- **GIVEN** a pull of `<registry>/base` at tag `1.3`, and a registry listing `1.3` and `1.4-rc1`
- **WHEN** I run `vamos update`
- **THEN** it succeeds, notes the prerelease `1.4-rc1`, and reports no newer release

#### Scenario: A tag that isn't a version is noted

- **GIVEN** a pull of `<registry>/base` at tag `latest`
- **WHEN** I run `vamos update`
- **THEN** it succeeds, pins `latest`, and notes that `latest` isn't a version whose newer releases can be reported

### Requirement: Tags are resolved at their upstream registry

Updating, and recording a pin, SHALL list tags and resolve them at the image's own registry, from the client, with
the client's credentials and `--tls-verify`, never through a pull-through cache. A registry that can't be reached
SHALL fail the command, naming the image and saying why; one that won't list tags resolves the tag to itself, noted.

#### Scenario: A pull-through cache is not asked about tags

- **GIVEN** a coordinator mapping `<registry>` to a pull-through cache that holds a stale `v1`
- **WHEN** I run `vamos update`
- **THEN** the lockfile pins what `<registry>` itself says `v1` names

#### Scenario: An unreachable registry fails, saying why

- **GIVEN** a pull of `127.0.0.1:1/app` at tag `v1`
- **WHEN** I run `vamos update`
- **THEN** it fails naming `127.0.0.1:1/app:v1` and saying the connection was refused, and the lockfile is unchanged

#### Scenario: A registry that won't list tags pins the tag itself

- **GIVEN** a pull of `<registry>/base` at tag `1.3`, and a registry that refuses to list its tags
- **WHEN** I run `vamos update`
- **THEN** it succeeds, pins `1.3` to the digest it names, and notes that its tags couldn't be listed

#### Scenario: Every page of a long tag list is read

- **GIVEN** a registry listing the tags of `<registry>/base` over several pages, the newest on the last
- **WHEN** I run `vamos update`
- **THEN** it reports the newest tag as a newer release

### Requirement: `--pins` says what a build does with an unpinned pull

Every command that builds SHALL take `--pins=record|resolve|require`, or else `VAMOS_PINS`, defaulting to `record`,
for a pull without a lockfile entry: `record` resolves it and adds its entry; `resolve` resolves it and keeps it
only in the build cache; `require` fails before anything runs. An unknown policy SHALL be refused.

#### Scenario: Recording adds the missing entry

- **GIVEN** a pull of `<registry>/base` at tag `v1`, and no lockfile
- **WHEN** I build `base`
- **THEN** it succeeds, and `vamos.lock` pins `<registry>/base:v1`

#### Scenario: Resolving keeps the lockfile as it is

- **GIVEN** a pull of `<registry>/base` at tag `v1`, and no lockfile
- **WHEN** I build `base` with `--pins=resolve`
- **THEN** it succeeds, and there is no `vamos.lock`

#### Scenario: Requiring pins fails naming every unpinned pull

- **GIVEN** pulls `base` and `tools`, and a lockfile pinning neither
- **WHEN** I build `app`, which uses both, with `VAMOS_PINS=require`
- **THEN** it fails naming both pulls and suggesting `vamos update`, and no target ran

#### Scenario: The flag outranks the environment

- **GIVEN** a pull of `<registry>/base` at tag `v1`, and no lockfile
- **WHEN** I build `base` with `VAMOS_PINS=require` and `--pins=record`
- **THEN** it succeeds, and `vamos.lock` pins `<registry>/base:v1`

#### Scenario: An unknown policy is refused

- **WHEN** I build `base` with `--pins=strict`
- **THEN** it fails, naming `strict` and the policies there are, and no target ran

### Requirement: Rebuilding a pull resolves nothing

`vamos build <pull> --rebuild` SHALL neither resolve the pull's tag again nor change its pin,
and SHALL say that `vamos update` refreshes a pull.

#### Scenario: Rebuilding a pinned pull points to update

- **GIVEN** a lockfile pinning `<registry>/base:v1` to `first`, and the registry's `v1` now names `second`
- **WHEN** I run `vamos build base --rebuild`
- **THEN** it succeeds, saying `pull "base": pulls are refreshed by vamos update`
- **AND** the lockfile still pins `first`
