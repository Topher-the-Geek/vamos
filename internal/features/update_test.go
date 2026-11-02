package features

import (
	"fmt"
	"strings"
	"testing"
)

// pin is an entry of a lockfile, as lockfilePinning writes it: the pulled image and tag, the tag it resolved to, and
// the digest of what that named, in which placeholders stand for what atPlaceholders says.
type pin struct {
	image, tag, digest string
}

// lockfilePinning returns a lockfile, as vamos writes it, that pins each of pins, which the caller gives in the order
// of their images.
func lockfilePinning(pins ...pin) string {
	var entries []string
	for _, p := range pins {
		entries = append(entries, fmt.Sprintf(`    %q: {
      "tag": %q,
      "digest": %q
    }`, p.image, p.tag, p.digest))
	}
	if len(entries) == 0 {
		return "{\n  \"version\": 1,\n  \"pulls\": {}\n}\n"
	}
	return "{\n  \"version\": 1,\n  \"pulls\": {\n" + strings.Join(entries, ",\n") + "\n  }\n}\n"
}

// pullOfBase declares the pull "base" of the scenario's registry's base, at tag.
func pullOfBase(tag string) string {
	return fmt.Sprintf(`pull(name="base", image="<registry>/base", tag=%q)`, tag)
}

// TestUpdate_TheLockfilePinsEachPulledTag specifies that the workspace root's vamos.lock pins each pulled image and
// tag to the tag it resolved to and the digest of the manifest that tag named, and that a lockfile vamos can't read
// fails every command that loads it.
func TestUpdate_TheLockfilePinsEachPulledTag(t *testing.T) {
	t.Run("A malformed lockfile is refused", func(t *testing.T) {
		w := newWorld(t)
		w.GivenAProjectThatPullsBusybox()
		w.GivenTheLockfileHolds(`
			{
			  "version": 1,
			  "pulls" {}
			}
		`)
		w.WhenIBuild("busybox")
		w.ThenItFailsWith(`/project/vamos.lock:3:11: invalid character '{' after object key`)
		w.ThenNoTargetRan()
	})

	t.Run("A lockfile of a newer version is refused", func(t *testing.T) {
		w := newWorld(t)
		w.GivenAProjectThatPullsBusybox()
		w.GivenTheLockfileHolds(`
			{
			  "version": 2,
			  "Pulls": [{"image": "busybox", "tag": "1.36"}]
			}
		`)
		w.WhenIBuild("busybox")
		w.ThenItFailsWith(`/project/vamos.lock is of version 2, but this vamos reads only version 1 or older; ` +
			`upgrade vamos`)
		w.ThenNoTargetRan()
	})
	for _, example := range []struct {
		name     string
		lockfile string
		error    string
	}{
		{
			name:     "no version",
			lockfile: `{"pulls": {}}`,
			error:    `/project/vamos.lock has no version`,
		},
		{
			name:     "a version that no vamos writes",
			lockfile: `{"version": 0, "pulls": {}}`,
			error:    `/project/vamos.lock is of version 0, which no vamos writes`,
		},
		{
			name:     "a known field written in another case",
			lockfile: `{"version": 1, "Pulls": {}}`,
			error:    `/project/vamos.lock: field "Pulls" is written "pulls"`,
		},
		{
			name: "a version that isn't a number",
			lockfile: `
				{
				  "version": "1",
				  "pulls": {}
				}
			`,
			error: `/project/vamos.lock:2:16: "version" holds a string, want a number`,
		},
		{
			name: "a pin that isn't an object",
			lockfile: `
				{
				  "version": 1,
				  "pulls": {"docker.io/library/busybox:1.36": "1.36.1"}
				}
			`,
			error: `/project/vamos.lock:3:54: "pulls" holds a string, want an object`,
		},
		{
			name: "an image not written as vamos writes it",
			lockfile: `
				{
				  "version": 1,
				  "pulls": {"busybox:1.36": {"tag": "1.36.1", "digest": "sha256:` + strings.Repeat("0", 64) + `"}}
				}
			`,
			error: `/project/vamos.lock: pull "busybox:1.36": it is written "docker.io/library/busybox:1.36"`,
		},
		{
			name: "an image without a tag",
			lockfile: `
				{
				  "version": 1,
				  "pulls": {"docker.io/library/busybox": {"tag": "1.36.1", "digest": "sha256:` +
				strings.Repeat("0", 64) + `"}}
				}
			`,
			error: `/project/vamos.lock: pull "docker.io/library/busybox": it has no tag`,
		},
		{
			name: "a pin without the tag it resolved to",
			lockfile: `
				{
				  "version": 1,
				  "pulls": {"docker.io/library/busybox:1.36": {"digest": "sha256:` + strings.Repeat("0", 64) + `"}}
				}
			`,
			error: `/project/vamos.lock: pull "docker.io/library/busybox:1.36": it has no tag resolved to`,
		},
		{
			name: "a tag resolved to that isn't a tag",
			lockfile: `
				{
				  "version": 1,
				  "pulls": {"docker.io/library/busybox:1.36": {"tag": "1.36 1", "digest": "sha256:` +
				strings.Repeat("0", 64) + `"}}
				}
			`,
			error: `/project/vamos.lock: pull "docker.io/library/busybox:1.36": tag resolved to "1.36 1": `,
		},
		{
			name: "a digest that isn't one",
			lockfile: `
				{
				  "version": 1,
				  "pulls": {"docker.io/library/busybox:1.36": {"tag": "1.36.1", "digest": "sha256:1234"}}
				}
			`,
			error: `/project/vamos.lock: pull "docker.io/library/busybox:1.36": digest "sha256:1234": ` +
				`invalid checksum digest length`,
		},
		{
			name: "a digest of another algorithm than sha256",
			lockfile: `
				{
				  "version": 1,
				  "pulls": {"docker.io/library/busybox:1.36": {"tag": "1.36.1", "digest": "sha512:` +
				strings.Repeat("0", 128) + `"}}
				}
			`,
			error: `/project/vamos.lock: pull "docker.io/library/busybox:1.36": digest "sha512:` +
				strings.Repeat("0", 128) + `": want a sha256 digest, which pulling verifies the manifest by`,
		},
	} {
		t.Run("A lockfile with a mistake is refused, saying what it is: "+example.name, func(t *testing.T) {
			w := newWorld(t)
			w.GivenAProjectThatPullsBusybox()
			w.GivenTheLockfileHolds(example.lockfile)
			w.WhenIBuild("busybox")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
		})
	}

	t.Run("Updating writes one entry per pulled tag", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheRegistryListsTags("<registry>/tools", "2")
		w.GivenTheBuildFileDeclares(`
			pull(name="tools", image="<registry>/tools", tag="2")
			pull(name="base", image="<registry>/base", tag="v1")
		`)
		w.WhenIUpdate()
		w.ThenItSucceeds()
		w.ThenTheLockfileIs(lockfilePinning(
			pin{"<registry>/base:v1", "v1", "<digest of <registry>/base:v1>"},
			pin{"<registry>/tools:2", "2", "<digest of <registry>/tools:2>"},
		))
	})

	t.Run("Adding a pin changes only its lines", func(t *testing.T) {
		w := newWorld(t)
		short := pin{"<registry>/a:1", "1", "<digest of <registry>/a:1>"}
		long := pin{"<registry>/a-much-longer-repository-name:1", "1",
			"<digest of <registry>/a-much-longer-repository-name:1>"}
		longer := pin{"<registry>/an-even-longer-repository-name-than-that:1", "1",
			"<digest of <registry>/an-even-longer-repository-name-than-that:1>"}
		w.GivenTheRegistryListsTags("<registry>/a", "1")
		w.GivenTheRegistryListsTags("<registry>/a-much-longer-repository-name", "1")
		w.GivenTheRegistryListsTags("<registry>/an-even-longer-repository-name-than-that", "1")
		w.GivenTheLockfileHolds(lockfilePinning(long, short)) // "-" sorts before ":"

		w.GivenTheBuildFileDeclares(`
			pull(name="a", image="<registry>/a", tag="1")
			pull(name="long", image="<registry>/a-much-longer-repository-name", tag="1")
			pull(name="longer", image="<registry>/an-even-longer-repository-name-than-that", tag="1")
		`)
		w.WhenIUpdate()
		w.ThenItSucceeds()
		w.ThenTheLockfileIs(lockfilePinning(long, short, longer))
	})

	t.Run("The lockfile lives at the workspace root", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheProjectHolds(files{"WORKSPACE": ""})
		w.GivenThePackageDeclares("backend", pullOfBase("v1"))
		w.GivenThePackageDeclares("ui", pullOfBase("v1"))
		w.GivenIWorkIn("ui")
		w.WhenIUpdate()
		w.ThenItSucceeds()
		w.ThenTheLockfileIs(lockfilePinning(pin{"<registry>/base:v1", "v1", "<digest of <registry>/base:v1>"}))
	})
}

// TestUpdate_TagsResolveToTheNewestTagTheyCover specifies that updating resolves a tag that reads as a version to the
// newest stable tag the registry lists that the tag covers, with the same v prefix and suffix, whose leading numbers
// are the tag's, and a tag that covers no other, or isn't a version, to itself.
func TestUpdate_TagsResolveToTheNewestTagTheyCover(t *testing.T) {
	for _, example := range []struct {
		tag      string
		listed   []string
		resolved string
	}{
		{
			tag:      "1.3",
			listed:   []string{"1.2.9", "1.3", "1.3.0", "1.3.1", "1.3.2", "1.4.0"},
			resolved: "1.3.2",
		},
		{
			tag:      "1.3",
			listed:   []string{"1.3", "1.4"},
			resolved: "1.3",
		},
		{
			tag:      "1.3.1",
			listed:   []string{"1", "1.3", "1.3.1"},
			resolved: "1.3.1",
		},
		{
			tag:      "24-alpine",
			listed:   []string{"24-alpine", "24.11.0-alpine", "24.11.1-alpine", "24.11.2-bookworm"},
			resolved: "24.11.1-alpine",
		},
		{
			tag:      "v1.3",
			listed:   []string{"v1.3", "v1.3.4", "1.3.9"},
			resolved: "v1.3.4",
		},
		{
			tag:      "1.3",
			listed:   []string{"1.3", "1.3.1", "1.3.2-rc1"},
			resolved: "1.3.1",
		},
		{
			tag:      "latest",
			listed:   []string{"latest", "1.4"},
			resolved: "latest",
		},
	} {
		t.Run(fmt.Sprintf("%s among %s resolves to %s", example.tag, strings.Join(example.listed, ", "),
			example.resolved), func(t *testing.T) {
			w := newWorld(t)
			w.GivenTheRegistryListsTags("<registry>/base", example.listed...)
			w.GivenTheBuildFileDeclares(pullOfBase(example.tag))
			w.WhenIUpdate()
			w.ThenItSucceeds()
			w.ThenTheLockfileIs(lockfilePinning(pin{"<registry>/base:" + example.tag, example.resolved,
				"<digest of <registry>/base:" + example.resolved + ">"}))
		})
	}
}

// TestUpdate_ReResolvesPinsAndReportsWhatChanged specifies that vamos update resolves the tag of every pull, or of
// the pulls named, rewrites their pins, and reports each pin it added, changed or removed, with the tags and digests
// before and after; that updating every pull removes the pins that no pull uses; and that it changes no other pin.
func TestUpdate_ReResolvesPinsAndReportsWhatChanged(t *testing.T) {
	t.Run("A moved tag is updated and reported", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheTagNames("<registry>/base:v1", "first")
		w.GivenTheLockfileHolds(lockfilePinning(pin{"<registry>/base:v1", "v1", "<digest of first>"}))
		w.GivenTheBuildFileDeclares(pullOfBase("v1"))
		w.GivenTheTagNames("<registry>/base:v1", "second")
		w.WhenIUpdate()
		w.ThenItSucceeds()
		w.ThenTheLockfileIs(lockfilePinning(pin{"<registry>/base:v1", "v1", "<digest of second>"}))
		w.ThenItReports("updated <registry>/base:v1 from v1@<digest of first> to v1@<digest of second>")
	})

	t.Run("A pull without a pin is pinned and reported", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheBuildFileDeclares(pullOfBase("v1"))
		w.WhenIUpdate()
		w.ThenItSucceeds()
		w.ThenItReports("pinned <registry>/base:v1 to v1@<digest of <registry>/base:v1>")
	})

	t.Run("Pins that are current are reported as such", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheLockfileHolds(lockfilePinning(pin{"<registry>/base:v1", "v1", "<digest of <registry>/base:v1>"}))
		w.GivenTheBuildFileDeclares(pullOfBase("v1"))
		w.WhenIUpdate()
		w.ThenItSucceeds()
		w.ThenItReports("vamos.lock is up to date")
		w.ThenTheLockfileIsUnchanged()
	})

	t.Run("A lockfile of current pins not written as vamos writes it is rewritten and reported", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheLockfileHolds(`{"version": 1, "pulls": {"<registry>/base:v1": ` +
			`{"tag": "v1", "digest": "<digest of <registry>/base:v1>"}}}`)
		w.GivenTheBuildFileDeclares(pullOfBase("v1"))
		w.WhenIUpdate()
		w.ThenItSucceeds()
		w.ThenItReports("vamos.lock pins the same, rewritten as this vamos writes it")
		w.ThenTheLockfileIs(lockfilePinning(pin{"<registry>/base:v1", "v1", "<digest of <registry>/base:v1>"}))
	})

	t.Run("Naming a pull updates only its pin", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheTagNames("<registry>/base:v1", "first base")
		w.GivenTheTagNames("<registry>/tools:v1", "first tools")
		w.GivenTheLockfileHolds(lockfilePinning(
			pin{"<registry>/base:v1", "v1", "<digest of first base>"},
			pin{"<registry>/tools:v1", "v1", "<digest of first tools>"},
		))
		w.GivenTheBuildFileDeclares(`
			pull(name="base", image="<registry>/base", tag="v1")
			pull(name="tools", image="<registry>/tools", tag="v1")
		`)
		w.GivenTheTagNames("<registry>/base:v1", "second base")
		w.GivenTheTagNames("<registry>/tools:v1", "second tools")
		w.WhenIUpdate(onlyThePulls("base"))
		w.ThenItSucceeds()
		w.ThenTheLockfileIs(lockfilePinning(
			pin{"<registry>/base:v1", "v1", "<digest of second base>"},
			pin{"<registry>/tools:v1", "v1", "<digest of first tools>"},
		))
	})

	t.Run("An entry no pull uses is removed", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheLockfileHolds(lockfilePinning(
			pin{"<registry>/base:v1", "v1", "<digest of <registry>/base:v1>"},
			pin{"<registry>/old:v1", "v1", "sha256:" + strings.Repeat("0", 64)},
		))
		w.GivenTheBuildFileDeclares(pullOfBase("v1"))
		w.WhenIUpdate()
		w.ThenItSucceeds()
		w.ThenTheLockfileIs(lockfilePinning(pin{"<registry>/base:v1", "v1", "<digest of <registry>/base:v1>"}))
		w.ThenItReports("removed <registry>/old:v1, which no pull uses, from v1@sha256:" + strings.Repeat("0", 64))
	})

	t.Run("Updating every pull finds every package, but not hidden folders or other workspaces", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheRegistryListsTags("<registry>/hidden", "v1")
		w.GivenTheRegistryListsTags("<registry>/other", "v1")
		w.GivenTheProjectHolds(files{"WORKSPACE": "", "other/WORKSPACE": ""})
		w.GivenThePackageDeclares("ui/web", pullOfBase("v1"))
		w.GivenThePackageDeclares(".cache", `pull(name="hidden", image="<registry>/hidden", tag="v1")`)
		w.GivenThePackageDeclares("other", `pull(name="other", image="<registry>/other", tag="v1")`)
		w.WhenIUpdate()
		w.ThenItSucceeds()
		w.ThenTheLockfileIs(lockfilePinning(pin{"<registry>/base:v1", "v1", "<digest of <registry>/base:v1>"}))
	})

	t.Run("Updating a project reached through a link finds its pulls", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheProjectIsReachedThroughALink()
		w.GivenTheLockfileHolds(lockfilePinning(pin{"<registry>/base:v1", "v1", "<digest of <registry>/base:v1>"}))
		w.GivenTheBuildFileDeclares(pullOfBase("v1"))
		w.WhenIUpdate()
		w.ThenItSucceeds()
		w.ThenItReports("vamos.lock is up to date")
		w.ThenTheLockfileIs(lockfilePinning(pin{"<registry>/base:v1", "v1", "<digest of <registry>/base:v1>"}))
	})

	t.Run("A pull whose tag is a function of ctx keeps the pins that no pull uses here", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1", "v2")
		w.GivenTheLockfileHolds(lockfilePinning(pin{"<registry>/base:v2", "v2", "<digest of <registry>/base:v2>"}))
		w.GivenTheBuildFileDeclares(`pull(name="base", image="<registry>/base", tag=lambda ctx: "v1")`)
		w.WhenIUpdate()
		w.ThenItSucceeds()
		w.ThenTheLockfileIs(lockfilePinning(
			pin{"<registry>/base:v1", "v1", "<digest of <registry>/base:v1>"},
			pin{"<registry>/base:v2", "v2", "<digest of <registry>/base:v2>"},
		))
		w.ThenItReports(
			"pinned <registry>/base:v1 to v1@<digest of <registry>/base:v1>",
			`kept <registry>/base:v2, which no pull uses here, since pull "base" may in another configuration`,
			"<registry>/base:v1 has a newer major release, v2",
		)
	})

	t.Run("A pull in a hidden folder that a package uses is pinned", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheProjectHolds(files{"WORKSPACE": ""})
		w.GivenThePackageDeclares(".shared", pullOfBase("v1"))
		w.GivenThePackageDeclares("ui", `layer(name="app", base="//.shared:base")`)
		w.WhenIUpdate()
		w.ThenItSucceeds()
		w.ThenTheLockfileIs(lockfilePinning(pin{"<registry>/base:v1", "v1", "<digest of <registry>/base:v1>"}))
	})

	t.Run("A package whose BUILD file is a link is found", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheProjectHolds(files{"WORKSPACE": ""})
		w.GivenThePackageDeclares(".shared", pullOfBase("v1")) // which a hidden folder hides from updating
		w.GivenTheProjectLinks(links{"ui/BUILD": "../.shared/BUILD"})
		w.WhenIUpdate()
		w.ThenItSucceeds()
		w.ThenTheLockfileIs(lockfilePinning(pin{"<registry>/base:v1", "v1", "<digest of <registry>/base:v1>"}))
	})

	t.Run("A pull in a hidden package that a function of ctx uses is found, and its pin kept", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheProjectHolds(files{"WORKSPACE": ""})
		w.GivenThePackageDeclares(".shared", pullOfBase("v1"))
		w.GivenThePackageDeclares("ui", `layer(name="app", base=lambda ctx: "//.shared:base")`)
		w.GivenTheLockfileHolds(lockfilePinning(pin{"<registry>/base:v1", "v1", "<digest of <registry>/base:v1>"}))
		w.WhenIUpdate(dryRun())
		w.ThenItSucceeds()
		w.ThenTheOutputIncludes("vamos.lock is up to date\n")
	})

	t.Run("A target that can't be configured keeps the pins that no pull found uses", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheProjectHolds(files{"WORKSPACE": ""})
		w.GivenThePackageDeclares(".shared", pullOfBase("v1"))
		w.GivenThePackageDeclares("ui", `
			layer(name="app", params={"version": param(type="string")},
			      base=lambda ctx: "//.shared:base")
		`)
		old := pin{"<registry>/old:v1", "v1", "sha256:" + strings.Repeat("0", 64)}
		w.GivenTheLockfileHolds(lockfilePinning(old))
		w.WhenIUpdate()
		w.ThenItSucceeds()
		w.ThenTheLockfileIs(lockfilePinning(old))
		w.ThenTheOutputIncludes(`kept <registry>/old:v1, which no pull uses here, since "//ui:app", which ` +
			"couldn't be configured as building it would, may use a pull that does\n")
	})

	t.Run("A pull whose tag is a function of ctx is pinned for the platform the command gives", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "amd64", "arm64")
		w.GivenTheBuildFileDeclares(`pull(name="base", image="<registry>/base", tag=lambda ctx: ctx.arch)`)
		w.WhenIUpdate(forPlatform("linux/arm64"))
		w.ThenItSucceeds()
		w.ThenTheLockfileIs(lockfilePinning(pin{"<registry>/base:arm64", "arm64", "<digest of <registry>/base:arm64>"}))
	})

	t.Run("A pull whose tag is a function of ctx is pinned for the execution platform the coordinator says to use",
		func(t *testing.T) {
			w := newWorld(t)
			arch := w.platform(other).Architecture
			w.GivenACoordinatorThatOffers([]string{thisPlatform(), other}, other)
			w.GivenTheRegistryListsTags("<registry>/base", arch, w.platform(thisPlatform()).Architecture)
			w.GivenTheBuildFileDeclares(
				`pull(name="base", image="<registry>/base", tag=lambda ctx: ctx.exec_platform.split("/")[1])`)
			w.WhenIUpdate(onTheCoordinator("<coordinator>"))
			w.ThenItSucceeds()
			w.ThenTheLockfileIs(lockfilePinning(
				pin{"<registry>/base:" + arch, arch, "<digest of <registry>/base:" + arch + ">"}))
		})

	t.Run("A coordinator that runs other graph versions still says which execution platform to pin for",
		func(t *testing.T) {
			w := newWorld(t)
			arch := w.platform(other).Architecture
			w.GivenACoordinatorOfAnotherGraphVersionThatOffers(other)
			w.GivenTheRegistryListsTags("<registry>/base", arch)
			w.GivenTheBuildFileDeclares(
				`pull(name="base", image="<registry>/base", tag=lambda ctx: ctx.exec_platform.split("/")[1])`)
			w.WhenIUpdate(onTheCoordinator("<coordinator>"))
			w.ThenItSucceeds()
			w.ThenTheLockfileIs(lockfilePinning(
				pin{"<registry>/base:" + arch, arch, "<digest of <registry>/base:" + arch + ">"}))
		})

	t.Run("A pull that can't be resolved leaves the lockfile as it was", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheRegistryListsTags("<registry>/missing")
		w.GivenTheLockfileHolds(lockfilePinning(pin{"<registry>/old:v1", "v1", "sha256:" + strings.Repeat("0", 64)}))
		w.GivenTheBuildFileDeclares(pullOfBase("v1") + "\n" + `pull(name="missing", image="<registry>/missing")`)
		w.WhenIUpdate()
		w.ThenItFailsWith(`resolving <registry>/missing:latest for pull "missing"`, "manifest unknown")
		w.ThenTheLockfileIsUnchanged()
	})

	t.Run("Naming a target that isn't a pull is refused", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheLockfileHolds(lockfilePinning(pin{"<registry>/base:v1", "v1", "<digest of <registry>/base:v1>"}))
		w.GivenTheBuildFileDeclares(pullOfBase("v1") + "\n" + `layer(name="app", base="base")`)
		w.WhenIUpdate(onlyThePulls("app"))
		w.ThenItFailsWith(`"app" is a layer(), and only pulls are updated`)
		w.ThenTheLockfileIsUnchanged()
	})
}

// TestUpdate_NewerReleasesAreReported specifies that updating reports, for each tag that reads as a version, the
// newest stable tag of a newer minor or patch release within its major version, and the newest stable tag of a newer
// major version, each where one exists, among the tags with the same v prefix and suffix; a tag it resolved to isn't
// newer.
func TestUpdate_NewerReleasesAreReported(t *testing.T) {
	for _, example := range []struct {
		tag      string
		listed   []string
		resolved string
		reported []string
	}{
		{
			tag:      "1.3",
			listed:   []string{"1.3", "1.4", "1.5"},
			resolved: "1.3",
			reported: []string{"<registry>/base:1.3 has a newer minor release, 1.5"},
		},
		{
			tag:      "1.3.1",
			listed:   []string{"1.3.1", "1.3.2"},
			resolved: "1.3.1",
			reported: []string{"<registry>/base:1.3.1 has a newer patch release, 1.3.2"},
		},
		{
			tag:      "1.3",
			listed:   []string{"1.3", "2.0", "2.1"},
			resolved: "1.3",
			reported: []string{"<registry>/base:1.3 has a newer major release, 2.1"},
		},
		{
			tag:      "1.3",
			listed:   []string{"1.3", "1.4", "2.0"},
			resolved: "1.3",
			reported: []string{"<registry>/base:1.3 has a newer minor release, 1.4, and a newer major release, 2.0"},
		},
		{
			tag:      "1.3",
			listed:   []string{"1.3", "1.3.5"},
			resolved: "1.3.5",
		},
		{
			tag:      "1.3.1",
			listed:   []string{"1", "1.3", "1.3.1"},
			resolved: "1.3.1",
		},
		{
			tag:      "24-alpine",
			listed:   []string{"24-alpine", "25-alpine", "26-bookworm"},
			resolved: "24-alpine",
			reported: []string{"<registry>/base:24-alpine has a newer major release, 25-alpine"},
		},
		{
			tag:      "v1.3",
			listed:   []string{"v1.3", "v1.4", "1.9"},
			resolved: "v1.3",
			reported: []string{"<registry>/base:v1.3 has a newer minor release, v1.4"},
		},
		{
			tag:      "1.3",
			listed:   []string{"1.3", "1.3.4", "1.4.1"},
			resolved: "1.3.4",
			reported: []string{"<registry>/base:1.3 has a newer minor release, 1.4.1"},
		},
		{
			tag:      "1.4-rc1",
			listed:   []string{"1.4-rc1", "1.4"},
			resolved: "1.4-rc1",
			reported: []string{"<registry>/base:1.4-rc1 has its final release, 1.4"},
		},
	} {
		name := fmt.Sprintf("%s among %s reports %d newer releases", example.tag, strings.Join(example.listed, ", "),
			len(example.reported))
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.GivenTheRegistryListsTags("<registry>/base", example.listed...)
			w.GivenTheBuildFileDeclares(pullOfBase(example.tag))
			w.WhenIUpdate()
			w.ThenItSucceeds()
			pinned := fmt.Sprintf("pinned <registry>/base:%s to %s@<digest of <registry>/base:%s>", example.tag,
				example.resolved, example.resolved)
			w.ThenItReports(append([]string{pinned}, example.reported...)...)
		})
	}
}

// TestUpdate_PrereleasesAndTagsThatArentVersionsAreNoted specifies that updating notes, without failing, the newest
// prerelease tag newer than a pull's resolved tag, and each pulled tag that doesn't read as a version, saying that it
// can't report newer releases of it.
func TestUpdate_PrereleasesAndTagsThatArentVersionsAreNoted(t *testing.T) {
	t.Run("A newer prerelease is noted", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "1.3", "1.4-rc1")
		w.GivenTheBuildFileDeclares(pullOfBase("1.3"))
		w.WhenIUpdate()
		w.ThenItSucceeds()
		w.ThenItReports(
			"pinned <registry>/base:1.3 to 1.3@<digest of <registry>/base:1.3>",
			"<registry>/base:1.3 has a newer prerelease, 1.4-rc1",
		)
	})

	t.Run("Only the newest prerelease is noted", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "1.3", "1.3.5", "1.3.4-rc1", "1.4-rc9", "1.4-rc10",
			"1.4-beta20", "1.4-rc9223372036854775808", "1.4-rc10000000000000000000")
		w.GivenTheBuildFileDeclares(pullOfBase("1.3"))
		w.WhenIUpdate()
		w.ThenItSucceeds()
		w.ThenItReports(
			"pinned <registry>/base:1.3 to 1.3.5@<digest of <registry>/base:1.3.5>",
			"<registry>/base:1.3 has a newer prerelease, 1.4-rc10000000000000000000",
		)
	})

	t.Run("A tag that isn't a version is noted", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "latest")
		w.GivenTheBuildFileDeclares(pullOfBase("latest"))
		w.WhenIUpdate()
		w.ThenItSucceeds()
		w.ThenTheLockfileIs(lockfilePinning(pin{"<registry>/base:latest", "latest",
			"<digest of <registry>/base:latest>"}))
		w.ThenItReports(
			"pinned <registry>/base:latest to latest@<digest of <registry>/base:latest>",
			"<registry>/base:latest isn't tagged with a version, so newer releases of it can't be reported",
		)
	})
}

// TestUpdate_ADryRunWritesNothingAndFailsIfAPinWouldChange specifies that vamos update --dry-run resolves and reports
// as vamos update does, leaves the lockfile as it is, creating none where there is none, and fails when vamos update
// would change it, while newer releases, prereleases and tags that aren't versions alone don't fail it.
func TestUpdate_ADryRunWritesNothingAndFailsIfAPinWouldChange(t *testing.T) {
	t.Run("A dry run fails when a pin would change", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheTagNames("<registry>/base:v1", "first")
		w.GivenTheLockfileHolds(lockfilePinning(pin{"<registry>/base:v1", "v1", "<digest of first>"}))
		w.GivenTheBuildFileDeclares(pullOfBase("v1"))
		w.GivenTheTagNames("<registry>/base:v1", "second")
		w.WhenIUpdate(dryRun())
		w.ThenItReports("would update <registry>/base:v1 from v1@<digest of first> to v1@<digest of second>")
		w.ThenItFailsWith("vamos.lock would change; run vamos update")
		w.ThenTheLockfileIsUnchanged()
	})

	t.Run("A dry run of current pins succeeds", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheLockfileHolds(lockfilePinning(pin{"<registry>/base:v1", "v1", "<digest of <registry>/base:v1>"}))
		w.GivenTheBuildFileDeclares(pullOfBase("v1"))
		w.WhenIUpdate(dryRun())
		w.ThenItSucceeds()
		w.ThenItReports("vamos.lock is up to date")
		w.ThenTheLockfileIsUnchanged()
	})

	t.Run("Newer releases alone don't fail a dry run", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "1.3", "1.4")
		w.GivenTheLockfileHolds(lockfilePinning(pin{"<registry>/base:1.3", "1.3", "<digest of <registry>/base:1.3>"}))
		w.GivenTheBuildFileDeclares(pullOfBase("1.3"))
		w.WhenIUpdate(dryRun())
		w.ThenItSucceeds()
		w.ThenItReports("vamos.lock is up to date", "<registry>/base:1.3 has a newer minor release, 1.4")
		w.ThenTheLockfileIsUnchanged()
	})

	t.Run("A dry run without a lockfile creates none, and fails if a pull would be pinned", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheBuildFileDeclares(pullOfBase("v1"))
		w.WhenIUpdate(dryRun())
		w.ThenItReports("would pin <registry>/base:v1 to v1@<digest of <registry>/base:v1>")
		w.ThenItFailsWith("vamos.lock would change; run vamos update")
		w.ThenThereIsNoLockfile()
	})

	t.Run("A dry run fails when a pin would be removed", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheLockfileHolds(lockfilePinning(
			pin{"<registry>/base:v1", "v1", "<digest of <registry>/base:v1>"},
			pin{"<registry>/old:v1", "v1", "sha256:" + strings.Repeat("0", 64)},
		))
		w.GivenTheBuildFileDeclares(pullOfBase("v1"))
		w.WhenIUpdate(dryRun())
		w.ThenItReports("would remove <registry>/old:v1, which no pull uses, from v1@sha256:" + strings.Repeat("0", 64))
		w.ThenItFailsWith("vamos.lock would change; run vamos update")
		w.ThenTheLockfileIsUnchanged()
	})

	t.Run("A dry run of a lockfile not written as vamos writes it fails", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheLockfileHolds(`{"version": 1, "pulls": {"<registry>/base:v1": ` +
			`{"tag": "v1", "digest": "<digest of <registry>/base:v1>"}}}`)
		w.GivenTheBuildFileDeclares(pullOfBase("v1"))
		w.WhenIUpdate(dryRun())
		w.ThenItReports("vamos.lock pins the same, but would be rewritten as this vamos writes it")
		w.ThenItFailsWith("vamos.lock would change; run vamos update")
		w.ThenTheLockfileIsUnchanged()
	})

	t.Run("A dry run of a project without pulls or a lockfile succeeds, creating none", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheBuildFileDeclares(`layer(name="app", base="scratch")`)
		w.WhenIUpdate(dryRun())
		w.ThenItSucceeds()
		w.ThenItReports("vamos.lock is up to date")
		w.ThenThereIsNoLockfile()
	})

	t.Run("A dry run of the pulls named ignores other pins", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheTagNames("<registry>/tools:v1", "first tools")
		w.GivenTheLockfileHolds(lockfilePinning(
			pin{"<registry>/base:v1", "v1", "<digest of <registry>/base:v1>"},
			pin{"<registry>/tools:v1", "v1", "<digest of first tools>"},
		))
		w.GivenTheBuildFileDeclares(pullOfBase("v1") + "\n" + `pull(name="tools", image="<registry>/tools", tag="v1")`)
		w.GivenTheTagNames("<registry>/tools:v1", "second tools")
		w.WhenIUpdate(dryRun(), onlyThePulls("base"))
		w.ThenItSucceeds()
		w.ThenItReports("vamos.lock is up to date")
		w.ThenTheLockfileIsUnchanged()
	})
}

// TestUpdate_TagsAreResolvedAtTheirUpstreamRegistry specifies that updating lists tags and resolves them at each
// image's own registry, from the client, never a pull-through cache or mirror; that a registry that can't be reached
// fails the command, naming the image and saying why; and that one that won't list tags resolves the tag to itself,
// noted.
func TestUpdate_TagsAreResolvedAtTheirUpstreamRegistry(t *testing.T) {
	t.Run("A pull-through cache is not asked about tags", func(t *testing.T) {
		w := newWorld(t)
		w.GivenARegistry()
		w.GivenTheRegistryHoldsAnIndex("<registry>/base:1.3", "stale", thisPlatform())
		w.GivenTheRegistryHoldsAnIndex("<registry>/base:1.3.1", "fresh", thisPlatform())
		w.GivenAPullThroughCacheMirrorsTheRegistryWithAnIndex("<registry>/base:1.3", "cached", thisPlatform())
		w.GivenTheBuildFileDeclares(pullOfBase("1.3"))
		w.WhenIUpdate()
		w.ThenItSucceeds()
		w.ThenTheLockfileIs(lockfilePinning(pin{"<registry>/base:1.3", "1.3.1", "<digest of fresh>"}))
		w.ThenThePullThroughCacheWasNotAsked()
	})

	t.Run("An unreachable registry fails, saying why", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheBuildFileDeclares(`pull(name="app", image="127.0.0.1:1/app", tag="v1")`)
		w.WhenIUpdate(withoutTLSVerification())
		w.ThenItFailsWith(`resolving 127.0.0.1:1/app:v1 for pull "app": listing its tags:`, "connection refused")
		w.ThenThereIsNoLockfile()
	})

	t.Run("A registry that won't list tags pins the tag itself", func(t *testing.T) {
		w := newWorld(t)
		w.GivenARegistry()
		w.GivenTheRegistryHoldsAnIndex("<registry>/base:1.3", "base 1.3", thisPlatform())
		w.GivenTheRegistryRefusesToListTags("<registry>/base")
		w.GivenTheBuildFileDeclares(pullOfBase("1.3"))
		w.WhenIUpdate()
		w.ThenItSucceeds()
		w.ThenTheLockfileIs(lockfilePinning(pin{"<registry>/base:1.3", "1.3", "<digest of base 1.3>"}))
		w.ThenTheOutputIncludes(
			"<registry>/base:1.3 is pinned to what its tag names, since its tags couldn't be listed")
	})

	t.Run("Every page of a long tag list is read", func(t *testing.T) {
		w := newWorld(t)
		w.GivenARegistry()
		w.GivenTheRegistryHoldsAnImage("<registry>/base:1.0", "base", thisPlatform())
		w.GivenTheRegistryAlsoTags("<registry>/base:1.0", "1.1", "1.2", "1.3", "2.0")
		w.GivenTheRegistryListsTagsInPagesOf(2)
		w.GivenTheBuildFileDeclares(pullOfBase("1.0"))
		w.WhenIUpdate()
		w.ThenItSucceeds()
		w.ThenItReports(
			"pinned <registry>/base:1.0 to 1.0@<digest of base>",
			"<registry>/base:1.0 has a newer minor release, 1.3, and a newer major release, 2.0",
		)
	})

	for _, example := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "a 403 with a code", status: 403, body: `{"errors": [{"code": "DENIED", "message": "no"}]}`},
		{name: "a 403 with a code alone", status: 403, body: `{"errors": [{"code": "DENIED"}]}`},
		{name: "a bare 403", status: 403},
		{name: "a 404 in HTML", status: 404, body: "<html>not here</html>"},
		{name: "a bare 405", status: 405},
		{name: "a bare 401", status: 401},
	} {
		t.Run("A refusal to list tags pins the tag itself: "+example.name, func(t *testing.T) {
			w := newWorld(t)
			w.GivenARegistry()
			w.GivenTheRegistryHoldsAnIndex("<registry>/base:1.3", "base 1.3", thisPlatform())
			w.GivenTheRegistryAnswersTagListsOf("<registry>/base", example.status, example.body)
			w.GivenTheBuildFileDeclares(pullOfBase("1.3"))
			w.WhenIUpdate()
			w.ThenItSucceeds()
			w.ThenTheLockfileIs(lockfilePinning(pin{"<registry>/base:1.3", "1.3", "<digest of base 1.3>"}))
		})
	}

	for _, example := range []struct {
		name   string
		status int
		body   string
		error  string
	}{
		{name: "a bare 500", status: 500, error: "500"},
		{name: "a code of the registry's own", status: 400,
			body: `{"errors": [{"code": "QUOTA_EXCEEDED", "message": "slow down"}]}`, error: "slow down"},
		{name: "a 503 in HTML", status: 503, body: "<html>busy</html>", error: "503"},
	} {
		t.Run("A failure to list tags that may pass fails, changing nothing: "+example.name, func(t *testing.T) {
			w := newWorld(t)
			w.GivenARegistry()
			w.GivenTheRegistryHoldsAnIndex("<registry>/base:1.3", "base 1.3", thisPlatform())
			w.GivenTheRegistryAnswersTagListsOf("<registry>/base", example.status, example.body)
			w.GivenTheBuildFileDeclares(pullOfBase("1.3"))
			w.WhenIUpdate()
			w.ThenItFailsWith(`resolving <registry>/base:1.3 for pull "base": listing its tags:`, example.error)
			w.ThenThereIsNoLockfile()
		})
	}
}

// TestUpdate_PinsSaysWhatABuildDoesWithAPullWithoutAPin specifies that every command that builds takes
// --pins=record|resolve|require, or else VAMOS_PINS, defaulting to record, for a pull without a pin in the lockfile:
// record resolves it and adds its pin, resolve resolves it and keeps it only in the build cache, and require fails
// before anything runs; and that an unknown policy is refused.
func TestUpdate_PinsSaysWhatABuildDoesWithAPullWithoutAPin(t *testing.T) {
	t.Run("Recording adds the missing entry", func(t *testing.T) {
		w := newBuildingWorld(t)
		w.GivenTheEnvironmentDoesNotSet("VAMOS_PINS")
		w.GivenARegistry()
		w.GivenTheRegistryHoldsAnIndex("<registry>/base:v1", "first", thisPlatform())
		w.GivenTheBuildFileDeclares(pullOfBase("v1"))
		w.WhenIBuild("base")
		w.ThenItSucceeds()
		w.ThenTheLockfileIs(lockfilePinning(pin{"<registry>/base:v1", "v1", "<digest of first>"}))
		w.ThenTheOutputIncludes("pinned <registry>/base:v1 to v1@<digest of first> in vamos.lock\n")
	})

	t.Run("Resolving keeps the lockfile as it is", func(t *testing.T) {
		w := newBuildingWorld(t)
		w.GivenARegistry()
		w.GivenTheRegistryHoldsAnIndex("<registry>/base:v1", "first", thisPlatform())
		w.GivenTheBuildFileDeclares(pullOfBase("v1"))
		w.WhenIBuild("base", pinning("resolve"))
		w.ThenItSucceeds()
		w.ThenThereIsNoLockfile()
	})

	t.Run("Requiring pins fails naming every unpinned pull", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheLockfileHolds(lockfilePinning())
		w.GivenTheBuildFileDeclares(`
			pull(name="base", image="<registry>/base", tag="v1")
			pull(name="tools", image="<registry>/tools", tag="v1")
		`)
		w.GivenTheEnvironmentSets("VAMOS_PINS", "require")
		w.WhenIBuild("base", alongWith("tools"))
		w.ThenItFailsWith(`VAMOS_PINS=require: pulls without a pin in vamos.lock: "base" (<registry>/base:v1), ` +
			`"tools" (<registry>/tools:v1); run vamos update to pin them`)
		w.ThenNoTargetRan()
		w.ThenTheLockfileIsUnchanged()
	})

	t.Run("The flag outranks the environment", func(t *testing.T) {
		w := newBuildingWorld(t)
		w.GivenARegistry()
		w.GivenTheRegistryHoldsAnIndex("<registry>/base:v1", "first", thisPlatform())
		w.GivenTheBuildFileDeclares(pullOfBase("v1"))
		w.GivenTheEnvironmentSets("VAMOS_PINS", "require")
		w.WhenIBuild("base", pinning("record"))
		w.ThenItSucceeds()
		w.ThenTheLockfileIs(lockfilePinning(pin{"<registry>/base:v1", "v1", "<digest of first>"}))
	})

	t.Run("An unknown policy is refused", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheBuildFileDeclares(pullOfBase("v1"))
		w.WhenIBuild("base", pinning("strict"))
		w.ThenItFailsShowingUsage(`invalid argument "strict" for "--pins" flag: "strict" isn't a policy for pulls ` +
			"without a pin; want record, resolve or require")
		w.ThenThereIsNoLockfile()
	})

	t.Run("An unknown policy in the environment is refused", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheBuildFileDeclares(pullOfBase("v1"))
		w.GivenTheEnvironmentSets("VAMOS_PINS", "strict")
		w.WhenIBuild("base")
		w.ThenItFailsWith(`VAMOS_PINS: "strict" isn't a policy for pulls without a pin; ` +
			"want record, resolve or require")
		w.ThenNoTargetRan()
	})

	t.Run("A glob reads the lockfile as the build records it", func(t *testing.T) {
		w := newBuildingWorld(t)
		w.GivenARegistry()
		w.GivenTheRegistryHoldsAnIndex("<registry>/base:v1", "first", thisPlatform())
		w.GivenTheBuildFileDeclares(pullOfBase("v1"))
		w.GivenTheBuildFileDeclares(`
			glob(name="lockfile", include=["vamos.lock"], allow_no_files=True)
			layer(name="app", base="base", steps=[add("lockfile", "/app")])
		`)
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): reused})
	})

	t.Run("Recording a pull that its registry doesn't have fails, suggesting to check the name", func(t *testing.T) {
		w := newWorld(t)
		w.GivenTheRegistryListsTags("<registry>/base", "v1")
		w.GivenTheBuildFileDeclares(pullOfBase("latest"))
		w.WhenIBuild("base")
		w.ThenItFailsWith(`resolving <registry>/base:latest for pull "base"`,
			"check the name, and log in if the image is private")
		w.ThenThereIsNoLockfile()
	})

	t.Run("Recording keeps a pin that another command wrote meanwhile", func(t *testing.T) {
		w := newBuildingWorld(t)
		w.GivenARegistry()
		w.GivenTheRegistryHoldsAnIndex("<registry>/base:v1", "first", thisPlatform())
		w.GivenTheRegistryHoldsAnIndex("<registry>/base:other", "second", thisPlatform())
		w.GivenTheBuildFileDeclares(pullOfBase("v1"))
		w.GivenTheBuildFileDeclares(`layer(name="app", base="base")`)
		pinned := lockfilePinning(
			pin{"<registry>/base:v1", "v1", "<digest of second>"},
			pin{"<registry>/old:v1", "v1", "sha256:" + strings.Repeat("0", 64)})
		w.GivenAnotherCommandPinsWhileTheTagsAreListed(pinned)
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageIsBuiltOn("second", thisPlatform())
		w.ThenTheLockfileIs(pinned)
		w.ThenTheOutputDoesNotInclude("pinned <registry>/base:v1")
	})

	t.Run("A build uses the pin it records", func(t *testing.T) {
		w := newBuildingWorld(t)
		w.GivenARegistry()
		w.GivenTheRegistryHoldsAnIndex("<registry>/base:1.3", "coarse", thisPlatform())
		w.GivenTheRegistryHoldsAnIndex("<registry>/base:1.3.2", "fine", thisPlatform())
		w.GivenTheBuildFileDeclares(pullOfBase("1.3"))
		w.GivenTheBuildFileDeclares(`layer(name="app", base="base")`)
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageIsBuiltOn("fine", thisPlatform())
		w.ThenTheLockfileIs(lockfilePinning(pin{"<registry>/base:1.3", "1.3.2", "<digest of fine>"}))
	})
}
