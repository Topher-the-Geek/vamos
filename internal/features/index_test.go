package features

import (
	"fmt"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
)

// newIndexWorld starts a scenario of the index feature, from its background:
// a registry of the scenario's own, which <registry> stands for, to push an index to.
// An index has an image for each of its platforms, each a pull's or a layer's, configured for that platform.
// Its layers are on scratch and run nothing, so that any platform's can be built without an emulator.
func newIndexWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenARegistry()
	return w
}

// newIndexMistakeWorld starts a scenario of the index feature whose index is declared or named wrongly,
// which loading catches before anything runs, so that it needs no buildah.
// Its project declares theAppForAnIndex.
func newIndexMistakeWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenTheBuildFileDeclares(theAppForAnIndex)
	w.GivenTheProjectHolds(files{"app.sh": "echo hi"})
	return w
}

// theAppForAnIndex declares, on lines 1 to 3, the glob "code" of app.sh, the layer "app" on scratch, which adds it
// and takes a mode, and the task "build" on app, whose output is app.sh.
const theAppForAnIndex = `
	glob(name="code", include=["app.sh"])
	layer(name="app", params={"mode": "release"}, base="scratch", steps=[add("code", "/")])
	task(name="build", base="app", source="/", include=["app.sh"])
`

// anIndexOfApp declares the layer "app" on scratch, which adds app.sh, whose command is the script, nonce,
// and its greeting, and the index "server" of app for this machine's platform and another, in that order,
// which passes its own greeting on to app.
func anIndexOfApp(nonce string) string {
	return fmt.Sprintf(`
		glob(name="code", include=["app.sh"])
		layer(
		    name="app",
		    params={"greeting": "hi"},
		    base="scratch",
		    steps=[add("code", "/")],
		    command=lambda ctx: ["/app.sh", %q, ctx.greeting],
		)
		index(
		    name="server",
		    params={"greeting": "hi"},
		    layer=dep("app", greeting=arg("greeting")),
		    platforms=[%q, %q],
		)
	`, nonce, thisPlatform(), anotherPlatform())
}

// TestIndex_HasAnImageForEachPlatform specifies that with a single pull or layer reference, vamos configures that
// target once for each listed platform and preserves the list's order in the index, which push publishes whole.
func TestIndex_HasAnImageForEachPlatform(t *testing.T) {
	for _, example := range []struct {
		name    string
		options []option
	}{
		{
			name: "The executor pushes each platform's image, in the list's order",
		},
		{
			name:    "The client pushes each platform's image, in the list's order",
			options: []option{fromTheClient(), withoutTLSVerification()},
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newIndexWorld(t)
			nonce := testenv.NewNonce()
			w.GivenTheBuildFileDeclares(anIndexOfApp(nonce))
			w.GivenTheProjectHolds(files{"app.sh": "echo " + nonce})
			w.WhenIPush("server", "<registry>/server:v1", append(example.options, setting("greeting=hola"))...)
			w.ThenItSucceeds()
			w.ThenTheTargetsWere(statuses{
				layer("app", `greeting="hola"`):                                              ran,
				layer("app", fmt.Sprintf(`platform=%s, greeting="hola"`, anotherPlatform())): ran,
			})
			w.ThenTheIndexManifestsAre(
				manifest{platform: thisPlatform(), command: []string{"/app.sh", nonce, "hola"},
					files: files{"/app.sh": "echo " + nonce}},
				manifest{platform: anotherPlatform(), command: []string{"/app.sh", nonce, "hola"},
					files: files{"/app.sh": "echo " + nonce}},
			)
		})
	}

	t.Run("Building an index builds each platform's image", func(t *testing.T) {
		w := newBuildingWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(anIndexOfApp(nonce))
		w.GivenTheProjectHolds(files{"app.sh": "echo " + nonce})
		w.WhenIBuild("server")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{
			layer("app"): ran,
			layer("app", "platform="+anotherPlatform()): ran,
		})
	})

	t.Run("A platform's variant reaches the index and its image", func(t *testing.T) {
		w := newIndexWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(name="app", base="scratch", command=lambda ctx: ["app", %q, ctx.variant])
			index(name="server", layer="app", platforms=["linux/arm/v6", "linux/arm/v7"])
		`, nonce))
		w.WhenIPush("server", "<registry>/server:v1")
		w.ThenItSucceeds()
		w.ThenTheIndexManifestsAre(
			manifest{platform: "linux/arm/v6", command: []string{"app", nonce, "v6"}},
			manifest{platform: "linux/arm/v7", command: []string{"app", nonce, "v7"}},
		)
	})
}

// TestIndex_GivesEachPlatformItsOwnTarget specifies that an index whose layer is a nonempty dictionary, from each
// platform to its target, has each target's image for its platform, in the dictionary's order.
func TestIndex_GivesEachPlatformItsOwnTarget(t *testing.T) {
	t.Run("Each platform's image is its own target's, in the dictionary's order", func(t *testing.T) {
		w := newIndexWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(name="here", base="scratch", command=["here", %[1]q])
			layer(name="there", base="scratch", command=["there", %[1]q])
			index(name="server", layer={%[2]q: "there", %[3]q: dep("here")})
		`, nonce, anotherPlatform(), thisPlatform()))
		w.WhenIPush("server", "<registry>/server:v1")
		w.ThenItSucceeds()
		w.ThenTheIndexManifestsAre(
			manifest{platform: anotherPlatform(), command: []string{"there", nonce}},
			manifest{platform: thisPlatform(), command: []string{"here", nonce}},
		)
	})
}

// TestIndex_DeclaresEachPlatformOnce specifies that an index's name and layer are required, that layer is a pull or
// layer reference with a nonempty list of platforms, or a nonempty dictionary from each platform to its reference,
// without platforms, and that each platform is unique.
func TestIndex_DeclaresEachPlatformOnce(t *testing.T) {
	for _, example := range []struct {
		name  string
		index string // on line 4, after theAppForAnIndex
		error string
	}{
		{
			name:  "An empty list of platforms fails",
			index: `index(name="server", layer="app", platforms=[])`,
			error: "/project/BUILD:4:6: index: platforms must not be empty",
		},
		{
			name:  "A layer without platforms fails, saying to give them, or a dict",
			index: `index(name="server", layer="app")`,
			error: "/project/BUILD:4:6: index: platforms is missing: the platforms to build layer for, " +
				`such as ["linux/amd64", "linux/arm64"]; or give layer as a dict from each platform to its target`,
		},
		{
			name:  "A platform that isn't os/arch fails",
			index: `index(name="server", layer="app", platforms=["linux/amd64", "x86"])`,
			error: `/project/BUILD:4:6: index: platforms[1]: platform "x86": want os/arch`,
		},
		{
			name:  "A platform listed twice fails, naming both",
			index: `index(name="server", layer="app", platforms=["linux/arm64", "linux/amd64", "linux/arm64/v8"])`,
			error: "/project/BUILD:4:6: index: platforms[0] and platforms[2] are both linux/arm64; " +
				"an index has one image for each platform",
		},
		{
			name:  "A layer that isn't a reference fails",
			index: `index(name="server", layer=1, platforms=["linux/amd64"])`,
			error: "/project/BUILD:4:6: index: layer is int, want a label or dep()",
		},
		{
			name:  "Platforms and a dict fail, saying to give one",
			index: `index(name="server", layer={"linux/amd64": "app"}, platforms=["linux/amd64"])`,
			error: "/project/BUILD:4:6: index: give platforms, or a dict for layer, " +
				"whose keys are the platforms, not both",
		},
		{
			name:  "An empty dict fails",
			index: `index(name="server", layer={})`,
			error: "/project/BUILD:4:6: index: layer must not be empty",
		},
		{
			name:  "A dict whose keys are a platform twice fails, naming both",
			index: `index(name="server", layer={"linux/arm": "app", "linux/arm/v7": "app"})`,
			error: `/project/BUILD:4:6: index: layer["linux/arm"]'s key and layer["linux/arm/v7"]'s key ` +
				"are both linux/arm/v7",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newIndexMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.index)
			w.WhenISave("server")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestIndex_HoldsPullsAndLayersForTheirPlatforms specifies that an index holds pulls and layers, not tasks or other
// indexes, can't be a base, and that a dep() in it may pass user parameters but can't specify platform,
// which the list or dictionary already determines.
func TestIndex_HoldsPullsAndLayersForTheirPlatforms(t *testing.T) {
	for _, example := range []struct {
		name  string
		build string // on line 4, after theAppForAnIndex
		error string
	}{
		{
			name:  "An index of a task fails",
			build: `index(name="server", layer="build", platforms=["linux/amd64"])`,
			error: `/project/BUILD:4:6: index: layer "build" is a task(), not a pull() or layer()`,
		},
		{
			name: "An index of an index fails",
			build: "index(name=\"inner\", layer=\"app\", platforms=[\"linux/amd64\"])\n" +
				`index(name="server", layer="inner", platforms=["linux/amd64"])`,
			error: `/project/BUILD:5:6: index: layer "inner" is an index(), not a pull() or layer()`,
		},
		{
			name: "An index as a base fails",
			build: "index(name=\"inner\", layer=\"app\", platforms=[\"linux/amd64\"])\n" +
				`layer(name="server", base="inner")`,
			error: `/project/BUILD:5:6: layer: base "inner" is an index(), not a pull() or layer()`,
		},
		{
			name:  "A dep() that gives a platform fails",
			build: `index(name="server", layer=dep("app", platform="linux/amd64"), platforms=["linux/amd64"])`,
			error: "/project/BUILD:4:6: index: layer: an index's dep() can't give platform, " +
				"since platforms[0] says what each manifest is for",
		},
		{
			name:  "A dict's dep() that gives a platform fails",
			build: `index(name="server", layer={"linux/amd64": dep("app", platform=EXEC_PLATFORM)})`,
			error: `/project/BUILD:4:6: index: layer["linux/amd64"]: an index's dep() can't give platform, ` +
				`since layer["linux/amd64"]'s key says what each manifest is for`,
		},
		{
			name:  "An arg() of a parameter the index doesn't have fails",
			build: `index(name="server", layer=dep("app", mode=arg("mode")), platforms=["linux/amd64"])`,
			error: `/project/BUILD:4:47: arg: "server" has no parameter "mode" to pass on; it declares none`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newIndexMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.build)
			w.WhenISave("server")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestIndex_SuppliesItsOwnPlatforms specifies that an index supplies its own platform list, so it cannot be named
// with --platform, and that selecting a platform missing from the index is an error.
func TestIndex_SuppliesItsOwnPlatforms(t *testing.T) {
	t.Run("An index named with --platform fails, saying to name one of its targets", func(t *testing.T) {
		w := newIndexMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`index(name="server", layer="app", platforms=["linux/amd64"])`)
		w.WhenISave("server", forPlatform(anotherPlatform()))
		w.ThenItFailsWith(`"server" is an index(), whose manifests are each built for their own platform; ` +
			"name one of them to build it for the platform given")
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
	})

	t.Run("Saving an index without this machine's platform fails, naming those it has", func(t *testing.T) {
		w := newIndexMistakeWorld(t)
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`index(name="server", layer="app", platforms=[%q])`, anotherPlatform()))
		w.WhenISave("server")
		w.ThenItFailsWith(`index "server" has no manifest for ` + thisPlatform() +
			fmt.Sprintf(`, the host's platform; it has %q`, anotherPlatform()))
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
	})

	t.Run("A --set of a parameter the index doesn't have fails", func(t *testing.T) {
		w := newIndexMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`index(name="server", layer="app", platforms=["linux/amd64"])`)
		w.WhenISave("server", setting("mode=debug"))
		w.ThenItFailsWith(`--set mode=debug: "server" has no parameter "mode"; it declares none`)
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
	})
}
