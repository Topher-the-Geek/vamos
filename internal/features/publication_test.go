package features

import (
	"fmt"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
)

// newPublicationWorld starts a scenario of the publication feature, from its background: a project that pulls busybox,
// and a registry of the scenario's own, which <registry> stands for, to publish to.
// A publication declares a release: the tasks that must succeed, and the images that go to which repositories,
// which vamos publish builds in one run and, only if all of them succeed, publishes under every tag given.
func newPublicationWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenAProjectThatPullsBusybox()
	w.GivenARegistry()
	return w
}

// newPublicationMistakeWorld starts a scenario of the publication feature whose publication is declared or named
// wrongly, or whose command line is mistaken, which fails before anything runs, so that it needs no buildah.
// Its project pulls busybox and declares theTargetsToRelease.
func newPublicationMistakeWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenAProjectThatPullsBusybox()
	w.GivenTheBuildFileDeclares(theTargetsToRelease)
	return w
}

// theTargetsToRelease declares, on lines 2 and 3, the layer "app" on busybox and the task "test" on it.
const theTargetsToRelease = `
	layer(name="app", base="busybox")
	task(name="test", base="app", steps=[run(["true"])])
`

// aRelease declares the layer "app" on busybox, which adds hello.txt, the tasks "lint" and "test" on app, which pass,
// and the publication "release", which checks both tasks and publishes app to <registry>/team/app.
const aRelease = `
	glob(name="hello", include=["hello.txt"])
	layer(name="app", base="busybox", steps=[add("hello", "/data")], command=["cat", "/data/hello.txt"])
	task(name="lint", base="app", steps=[run(["true"])])
	task(name="test", base="app", steps=[run(["test", "-f", "/data/hello.txt"])])
	publication(name="release", checks=["lint", "test"], images={"<registry>/team/app": "app"})
`

// aReleaseOfTwoImages declares the layer "app" on busybox, which adds hello.txt, the layer "worker", which says
// nonce, and the publication "release", which publishes app to <registry>/team/app and worker to
// <registry>/team/worker, in that order.
func aReleaseOfTwoImages(nonce string) string {
	return fmt.Sprintf(`
		glob(name="hello", include=["hello.txt"])
		layer(name="app", base="busybox", steps=[add("hello", "/data")], command=["cat", "/data/hello.txt"])
		layer(name="worker", base="busybox", command=["echo", %q])
		publication(
		    name="release",
		    images={"<registry>/team/app": "app", "<registry>/team/worker": "worker"},
		)
	`, nonce)
}

// TestPublication_DeclaresItsChecksAndImages specifies that a BUILD file declares a publication with a name, the
// optional tasks that it checks, and its images, a nonempty dictionary from a repository, a lowercase registry
// repository name without a tag or digest, to the pull, layer or index published there,
// and that a mistake in one is reported as the BUILD file loads, naming the file, line and column.
func TestPublication_DeclaresItsChecksAndImages(t *testing.T) {
	t.Run("A publication names its checks and images", func(t *testing.T) {
		w := newPublicationWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(aRelease)
		w.GivenTheProjectHolds(files{"hello.txt": nonce})
		w.WhenIPublish("release", taggedAs("v1"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("lint"): ran, task("test"): ran, layer("app"): ran})
		w.ThenTheImageHolds(under("/data"), files{"/data/hello.txt": nonce})
		w.ThenTheRepositoryHasTags("<registry>/team/app", "v1")
	})

	t.Run("An index is published whole", func(t *testing.T) {
		w := newPublicationWorld(t)
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			glob(name="hello", include=["hello.txt"])
			layer(name="app", base="scratch", steps=[add("hello", "/")])
			index(name="server", layer="app", platforms=[%q, %q])
			publication(name="release", images={"<registry>/team/server": "server"})
		`, thisPlatform(), anotherPlatform()))
		w.GivenTheProjectHolds(files{"hello.txt": testenv.NewNonce()})
		w.WhenIPublish("release", taggedAs("v1"))
		w.ThenItSucceeds()
		w.ThenItLastReports("Index built! <registry>/team/server:v1 ")
		w.ThenTheIndexManifestsAre(manifest{platform: thisPlatform()}, manifest{platform: anotherPlatform()})
	})

	for _, example := range []struct {
		name        string
		publication string
		error       string
	}{
		{
			name: "A check that isn't a task is refused",
			publication: `publication(name="release", checks=["app"], ` +
				`images={"registry.example/team/app": "app"})`,
			error: `/project/BUILD:4:12: publication: checks[0] "app" is a layer(), not a task(); ` +
				"a check must be a task",
		},
		{
			name:        "An image that isn't a pull, layer or index is refused",
			publication: `publication(name="release", images={"registry.example/team/app": "test"})`,
			error: `/project/BUILD:4:12: publication: images["registry.example/team/app"] "test" is a task(), ` +
				"not a pull(), layer() or index(); a published target must be a pull, layer or index",
		},
		{
			name:        "A repository with a tag is refused",
			publication: `publication(name="release", images={"registry.example/team/app:v1": "app"})`,
			error: `/project/BUILD:4:12: publication: images: repository "registry.example/team/app:v1" ` +
				"must not include a tag; the tag comes from --tag",
		},
		{
			name: "A repository with a digest is refused",
			publication: `publication(name="release", images={"registry.example/team/app@sha256:` +
				`0000000000000000000000000000000000000000000000000000000000000000": "app"})`,
			error: `/project/BUILD:4:12: publication: images: repository "registry.example/team/app@sha256:` +
				`0000000000000000000000000000000000000000000000000000000000000000" must not include a digest; ` +
				"publish writes the tags --tag gives",
		},
		{
			name:        "A repository that isn't lowercase is refused",
			publication: `publication(name="release", images={"Registry.example/team/app": "app"})`,
			error: `/project/BUILD:4:12: publication: images: repository "Registry.example/team/app" ` +
				"must be lowercase",
		},
		{
			name:        "Two keys naming one repository are refused",
			publication: `publication(name="release", images={"busybox": "app", "docker.io/library/busybox": "app"})`,
			error: `/project/BUILD:4:12: publication: images has "docker.io/library/busybox" twice, as "busybox" ` +
				`and "docker.io/library/busybox"; a repository gets one image of a publication`,
		},
		{
			name:        "A publication without images is refused",
			publication: `publication(name="release", images={})`,
			error:       "/project/BUILD:4:12: publication: images is empty, so it publishes no image",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newPublicationMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.publication)
			w.WhenIPublish("release", taggedAs("v1"))
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
		})
	}
}

// TestPublication_PublishesATargetToSeveralRepositories specifies that one target can be published to several
// repositories of one publication, each receiving the same image under every tag given.
func TestPublication_PublishesATargetToSeveralRepositories(t *testing.T) {
	t.Run("An image goes to two repositories", func(t *testing.T) {
		w := newPublicationWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(name="app", base="busybox", command=["echo", "hello from vamos"])
			publication(name="release", images={"<registry>/team/app": "app", "<registry>/mirror/app": "app"})
		`)
		w.WhenIPublish("release", taggedAs("v1"))
		w.ThenItSucceeds()
		w.ThenTheyNameTheSameManifest("<registry>/team/app:v1", "<registry>/mirror/app:v1")
		w.ThenTheImageCommandIs("echo", "hello from vamos")
	})
}

// TestPublication_IsARootNeverADependency specifies that no target can depend on a publication, and that a
// publication's own result is never cached: publishing it again pushes again, while its checks and images are reused
// from the build cache.
func TestPublication_IsARootNeverADependency(t *testing.T) {
	t.Run("Depending on a publication is refused", func(t *testing.T) {
		w := newPublicationMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`
			publication(name="release", images={"registry.example/team/app": "app"})
			layer(name="later", base="release")
		`)
		w.WhenIBuild("later")
		w.ThenItFailsWith(`/project/BUILD:5:6: layer: base "release" is a publication(), which can't be depended on`)
		w.ThenNoTargetRan()
	})

	t.Run("Publishing again pushes again from the cache", func(t *testing.T) {
		w := newPublicationWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(aRelease)
		w.GivenTheProjectHolds(files{"hello.txt": nonce})
		w.WhenIPublish("release", taggedAs("v1"))
		w.ThenItSucceeds()
		w.WhenIPublish("release", taggedAs("v2"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("lint"): reused, task("test"): reused, layer("app"): reused})
		w.ThenTheRepositoryHasTags("<registry>/team/app", "v1", "v2")
		w.ThenTheyNameTheSameManifest("<registry>/team/app:v1", "<registry>/team/app:v2")
	})
}

// TestPublication_RepositoriesCanDependOnParameters specifies that a publication takes params, set with --set,
// and that its attributes can be functions of ctx, so that a parameter can choose its repositories.
func TestPublication_RepositoriesCanDependOnParameters(t *testing.T) {
	t.Run("A parameter chooses the registry", func(t *testing.T) {
		w := newPublicationWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(name="app", base="busybox", command=["echo", "hello from vamos"])
			publication(
			    name="release",
			    params={"env": param(default="production", values=["staging", "production"])},
			    images=lambda ctx: {
			        ("<registry>/staging/app" if ctx.env == "staging" else "<registry>/prod/app"): "app",
			    },
			)
		`)
		w.WhenIPublish("release", taggedAs("v1"), setting("env=staging"))
		w.ThenItSucceeds()
		w.ThenTheRepositoryHasTags("<registry>/staging/app", "v1")
		w.ThenTheRepositoryHasTags("<registry>/prod/app")
	})
}

// TestPublication_PublishRequiresATag specifies that publish requires --tag, which may be repeated, that every image
// is published under every tag given, and that a missing or invalid tag is a mistake on the command line, reported
// before anything is built, as is naming a target that isn't a publication, saying what it is.
func TestPublication_PublishRequiresATag(t *testing.T) {
	t.Run("Every image under every tag", func(t *testing.T) {
		w := newPublicationWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(aReleaseOfTwoImages(nonce))
		w.GivenTheProjectHolds(files{"hello.txt": "hello"})
		w.WhenIPublish("release", taggedAs("v1", "latest"))
		w.ThenItSucceeds()
		w.ThenTheImageCommandIs("echo", nonce) // the last published, worker:latest
		w.ThenTheRepositoryHasTags("<registry>/team/app", "v1", "latest")
		w.ThenTheRepositoryHasTags("<registry>/team/worker", "v1", "latest")
		w.ThenTheyNameTheSameManifest("<registry>/team/app:v1", "<registry>/team/app:latest")
		w.ThenTheyNameTheSameManifest("<registry>/team/worker:v1", "<registry>/team/worker:latest")
	})

	for _, example := range []struct {
		name  string
		args  []string
		error string
	}{
		{
			name:  "No tag is a command-line mistake",
			args:  []string{"publish", "release"},
			error: `required flag "tag" not set`,
		},
		{
			name:  "An invalid tag is a command-line mistake",
			args:  []string{"publish", "release", "--tag", "not a tag"},
			error: `--tag "not a tag" is not a valid tag`,
		},
		{
			name:  "A tag given twice is a command-line mistake",
			args:  []string{"publish", "release", "--tag", "v1", "--tag", "v1"},
			error: `--tag "v1" is given twice`,
		},
		{
			name:  "No publication is a command-line mistake",
			args:  []string{"publish", "--tag", "v1"},
			error: "missing publication, e.g. vamos publish release --tag 1.4.2",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newPublicationMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`publication(name="release", images={"registry.example/team/app": "app"})`)
			w.WhenIRun(example.args...)
			w.ThenItFailsShowingUsage(example.error)
		})
	}

	t.Run("Publish names something that isn't a publication", func(t *testing.T) {
		w := newPublicationMistakeWorld(t)
		w.WhenIPublish("app", taggedAs("v1"))
		w.ThenItFailsWith(`"app" is a layer(), not a publication(); publish takes a publication`)
		w.ThenNoTargetRan()
	})
}

// TestPublication_NothingIsPublishedUnlessEverythingSucceeds specifies that publish builds every check and image, with
// their dependencies, in one run, and pushes nothing to any repository unless all of them succeed.
func TestPublication_NothingIsPublishedUnlessEverythingSucceeds(t *testing.T) {
	t.Run("A failing check publishes nothing", func(t *testing.T) {
		w := newPublicationWorld(t)
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(name="app", base="busybox", command=["echo", %q])
			task(name="test", base="app", steps=[run(["false"])])
			publication(name="release", checks=["test"], images={"<registry>/team/app": "app"})
		`, testenv.NewNonce()))
		w.WhenIPublish("release", taggedAs("v1"))
		w.ThenItFailsWith(`task "test": run(["false"]) failed`)
		w.ThenTheRegistryWasNotAsked()
	})

	t.Run("A failing image publishes nothing, not even the others", func(t *testing.T) {
		w := newPublicationWorld(t)
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(name="app", base="busybox", command=["echo", %q])
			layer(name="broken", base="busybox", steps=[run(["false"])])
			publication(name="release", images={"<registry>/team/app": "app", "<registry>/team/broken": "broken"})
		`, testenv.NewNonce()))
		w.WhenIPublish("release", taggedAs("v1"))
		w.ThenItFailsWith(`layer "broken": run(["false"]) failed`)
		w.ThenTheRegistryWasNotAsked()
	})
}

// TestPublication_UploadsEveryImageBeforeAnyTag specifies that publishing first uploads every image's or index's
// content to each of its repositories by digest, with no tag, and writes tags only once every upload has succeeded,
// so that a failed upload leaves every tag of the publication as it was.
func TestPublication_UploadsEveryImageBeforeAnyTag(t *testing.T) {
	for _, example := range []struct {
		name    string
		options []option
	}{
		{
			name: "A failed upload from the executor writes no tag",
		},
		{
			name:    "A failed upload from the client writes no tag",
			options: []option{fromTheClient(), withoutTLSVerification()},
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newPublicationWorld(t)
			w.GivenTheBuildFileDeclares(aReleaseOfTwoImages(testenv.NewNonce()))
			w.GivenTheProjectHolds(files{"hello.txt": "hello"})
			w.WhenIPublish("release", append([]option{taggedAs("v1", "latest")}, example.options...)...)
			w.ThenItSucceeds()
			w.GivenTheProjectHolds(files{"hello.txt": "hello again"})
			w.GivenTheRegistryRefusesWritesTo("team/worker")
			w.WhenIPublish("release", append([]option{taggedAs("v2", "latest")}, example.options...)...)
			w.ThenItFailsWith("uploading to <registry>/team/worker", "writes refused")
			w.ThenTheRepositoryHasTags("<registry>/team/app", "v1", "latest")
			w.ThenTheRepositoryHasTags("<registry>/team/worker", "v1", "latest")
			w.ThenTheyNameTheSameManifest("<registry>/team/app:v1", "<registry>/team/app:latest")
		})
	}
}

// TestPublication_AFailureWhileTaggingReportsWhatWasTagged specifies that once tagging starts, publishing attempts
// every tag of every repository even if some fail, reads each written tag back, and fails if any tag was not written,
// naming each that failed and each that was written; publishing again then finishes the job.
func TestPublication_AFailureWhileTaggingReportsWhatWasTagged(t *testing.T) {
	for _, example := range []struct {
		name    string
		options []option
	}{
		{
			name: "Some tags fail, from the executor, and running it again finishes the job",
		},
		{
			name:    "Some tags fail, from the client, and running it again finishes the job",
			options: []option{fromTheClient(), withoutTLSVerification()},
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newPublicationWorld(t)
			w.GivenTheBuildFileDeclares(aReleaseOfTwoImages(testenv.NewNonce()))
			w.GivenTheProjectHolds(files{"hello.txt": "hello"})
			w.GivenTheRegistryRefusesTagsIn("team/app")
			options := append([]option{taggedAs("v1")}, example.options...)
			w.WhenIPublish("release", options...)
			w.ThenItFailsWith("1 of 2 tags weren't written", "<registry>/team/app:v1", "writes refused",
				"written: <registry>/team/worker:v1")
			w.ThenTheRepositoryHasTags("<registry>/team/app")
			w.ThenTheRepositoryHasTags("<registry>/team/worker", "v1")

			w.GivenTheRegistryAcceptsWritesAgain()
			w.WhenIPublish("release", options...)
			w.ThenItSucceeds()
			w.ThenTheRepositoryHasTags("<registry>/team/app", "v1")
			w.ThenTheRepositoryHasTags("<registry>/team/worker", "v1")
		})
	}
}

// TestPublication_ReportsEachDestination specifies that on success publish reports one line for each repository and
// tag, naming the destination and the image ID there, in the publication's order.
func TestPublication_ReportsEachDestination(t *testing.T) {
	t.Run("One line per destination", func(t *testing.T) {
		w := newPublicationWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(name="app", base="busybox", command=["echo", "hello from vamos"])
			publication(name="release", images={"<registry>/team/app": "app", "<registry>/mirror/app": "app"})
		`)
		w.WhenIPublish("release", taggedAs("v1", "latest"))
		w.ThenItSucceeds()
		w.ThenItReportsInOrder(
			"Image built! <registry>/team/app:v1 ", "Image built! <registry>/team/app:latest ",
			"Image built! <registry>/mirror/app:v1 ", "Image built! <registry>/mirror/app:latest ")
	})
}

// TestPublication_EachSidePublishesWithItsOwnSettings specifies that publish pushes from the executor, with its
// registry settings and credentials, by default, and from the client, with the client's and the command line's
// --tls-verify, given --push-from=client, as push does.
func TestPublication_EachSidePublishesWithItsOwnSettings(t *testing.T) {
	t.Run("The executor publishes over HTTPS where registries.conf says to", func(t *testing.T) {
		w := newBuildingWorld(t)
		w.GivenAProjectThatPullsBusybox()
		w.GivenARegistryThatRegistriesConfDoesNotMarkInsecure()
		w.GivenTheBuildFileDeclares(`
			layer(name="app", base="busybox", command=["echo", "hello from vamos"])
			publication(name="release", images={"<registry>/team/app": "app"})
		`)
		w.WhenIPublish("release", taggedAs("v1"), withoutTLSVerification())
		w.ThenItFailsWith("uploading to <registry>/team/app", "HTTP response to HTTPS client")
		w.ThenTheRepositoryHasTags("<registry>/team/app")
	})

	t.Run("The client publishes with --push-from=client", func(t *testing.T) {
		w := newBuildingWorld(t)
		w.GivenAProjectThatPullsBusybox()
		w.GivenARegistryThatRegistriesConfDoesNotMarkInsecure()
		w.GivenTheBuildFileDeclares(`
			layer(name="app", base="busybox", command=["echo", "hello from vamos"])
			publication(name="release", images={"<registry>/team/app": "app"})
		`)
		w.WhenIPublish("release", taggedAs("v1"), fromTheClient(), withoutTLSVerification())
		w.ThenItSucceeds()
		w.ThenTheRepositoryHasTags("<registry>/team/app", "v1")
		w.ThenTheImageCommandIs("echo", "hello from vamos")
	})
}

// TestPublication_BuildingAPublicationPublishesNothing specifies that vamos build of a publication builds its checks
// and its images as publish would, and pushes nothing.
func TestPublication_BuildingAPublicationPublishesNothing(t *testing.T) {
	t.Run("A dry run", func(t *testing.T) {
		w := newPublicationWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(aRelease)
		w.GivenTheProjectHolds(files{"hello.txt": nonce})
		w.WhenIBuild("release")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{task("lint"): ran, task("test"): ran, layer("app"): ran})
		w.ThenTheRegistryWasNotAsked()
	})
}

// theAppOnScratch declares the layer "app" on scratch, which adds hello.txt, so that a coordinator's worker builds it
// with no base to pull, and the publication "release", which publishes it to <registry>/team/app.
const theAppOnScratch = `
	glob(name="hello", include=["hello.txt"])
	layer(name="app", base="scratch", steps=[add("hello", "/data")])
	publication(name="release", images={"<registry>/team/app": "app"})
`

// aTaskThatWaits declares the task "waits" on scratch, which runs a command, and so waits on a coordinator for a worker
// of its execution platform.
const aTaskThatWaits = `
	task(name="waits", base="scratch", steps=[run(["/waits"])])
`

// TestPublication_ACoordinatorsWorkersPublish specifies that on a coordinator, publishing runs on its workers, with
// their registry settings and credentials, never on the coordinator itself.
func TestPublication_ACoordinatorsWorkersPublish(t *testing.T) {
	t.Run("A coordinator's workers publish", func(t *testing.T) {
		w := newPublicationWorld(t)
		w.GivenACoordinatorWithAWorker()
		w.GivenTheBuildFileDeclares(theAppOnScratch)
		hello := testenv.NewNonce()
		w.GivenTheProjectHolds(files{"hello.txt": hello})
		w.WhenIPublish("release", taggedAs("v1", "latest"), onTheCoordinator("<coordinator>"))
		w.ThenItSucceeds()
		w.ThenTheRepositoryHasTags("<registry>/team/app", "v1", "latest")
		w.ThenTheImageHolds(under("/data"), files{"/data/hello.txt": hello})
	})
}

// TestPublication_ACoordinatorAdmitsAPublicationAsOneJob specifies that a coordinator counts a publication as one job
// against its limit of jobs that haven't ended, however many repositories and tags it has,
// so that it refuses one, if at all, before anything is pushed, and that a busy coordinator refuses the publication's
// build first.
func TestPublication_ACoordinatorAdmitsAPublicationAsOneJob(t *testing.T) {
	// A coordinator admits each job as it is submitted, counting those running: the publication's build, which runs
	// first, meets the same limit as the publication, so a coordinator that is busy refuses that build already.
	// A coordinator's Go tests show the publication itself refused while another job runs.
	t.Run("A busy coordinator refuses before pushing", func(t *testing.T) {
		w := newPublicationWorld(t)
		w.GivenACoordinatorWithAWorker("--max-builds", "1", "--offer", anotherPlatform())
		w.GivenTheBuildFileDeclares(theAppOnScratch)
		w.GivenTheBuildFileDeclares(aTaskThatWaits)
		w.GivenTheProjectHolds(files{"hello.txt": testenv.NewNonce()})
		// Another client's build waits for a worker of a platform that none serves, holding the one job.
		w.GivenAnotherClientBuildsOnTheCoordinator("waits", onTheCoordinator("<coordinator>"),
			onTheExecPlatform(anotherPlatform()))
		w.WhenIPublish("release", taggedAs("v1"), onTheCoordinator("<coordinator>"))
		w.ThenItFailsWith("the executor is busy")
		w.ThenTheRepositoryHasTags("<registry>/team/app")
	})
}
