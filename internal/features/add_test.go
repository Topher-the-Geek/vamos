package features

import (
	"testing"
)

// newAddWorld starts a scenario of the add feature, from its background.
// add() copies a glob's or task's fileset beneath an absolute container directory, preserving the paths within it.
// Its project pulls busybox.
func newAddWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// newAddMistakeWorld starts a scenario of the add feature whose add() or workdir() is given wrongly,
// which loading catches before anything runs, so that it needs no buildah.
// Its project pulls busybox.
func newAddMistakeWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// TestAdd_FilesAreOwnedByRootUnlessOverridden specifies that added files are owned by root unless owner or group
// overrides ownership, naming users and groups in the image: with only owner, the group is that user's group,
// and with only group, the owner remains root. New parent directories receive the selected ownership too.
func TestAdd_FilesAreOwnedByRootUnlessOverridden(t *testing.T) {
	for _, example := range []struct {
		name  string
		args  string // add()'s arguments after its source and destination, as Starlark
		owner string // the owner the files and their new parents get, as "uid:gid"
	}{
		{
			name:  "Without an owner or group, files are root's",
			owner: "0:0",
		},
		{
			name:  "An owner named in the image owns the files, with the owner's group",
			args:  `, owner="nobody"`,
			owner: "65534:65534",
		},
		{
			name:  "A group alone leaves root the owner",
			args:  `, group="2000"`,
			owner: "0:2000",
		},
		{
			name:  "An owner and a group may each be named or numbered",
			args:  `, owner="1000", group="nobody"`,
			owner: "1000:65534",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newAddWorld(t)
			w.GivenTheBuildFileDeclares(`glob(name="code", include=["src"])`)
			w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[add("code", "/tmp/app"` +
				example.args + `)])`)
			w.GivenTheProjectHolds(files{"src/sub/app.sh": "echo hi"})
			w.WhenISave("app")
			w.ThenItSucceeds()
			w.ThenTheImageOwnersAre(owners{
				"/tmp":                    "0:0",
				"/tmp/app":                example.owner,
				"/tmp/app/src":            example.owner,
				"/tmp/app/src/sub":        example.owner,
				"/tmp/app/src/sub/app.sh": example.owner,
			})
		})
	}

	t.Run("An owner the image doesn't name fails the layer, leaving nothing behind", func(t *testing.T) {
		w := newAddWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="code", include=["app.sh"])
			layer(name="app", base="busybox", steps=[add("code", "/app", owner="no-such-user")])
		`)
		w.GivenTheProjectHolds(files{"app.sh": "echo hi"})
		w.WhenISave("app")
		w.ThenItFailsWith(`layer "app": add("code", "/app", owner="no-such-user") failed`,
			`looking up UID/GID for "no-such-user"`)
		w.ThenNothingIsSaved()
		w.ThenNoImageIsLeftBehind()
	})
}

// TestAdd_ExistingDirectoriesKeepTheirModesAndOwners specifies that directories the image already has
// keep their modes and owners, unless the fileset selects them itself:
// a selected directory brings its masked mode and the selected ownership.
func TestAdd_ExistingDirectoriesKeepTheirModesAndOwners(t *testing.T) {
	t.Run("Adding into an existing directory leaves its mode and owner", func(t *testing.T) {
		w := newAddWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="code", include=["app.sh"])
			layer(name="app", base="busybox", steps=[add("code", "/tmp", owner="nobody")])
		`)
		w.GivenTheProjectHolds(files{"app.sh": "echo hi"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageModesAre(modes{"/tmp": 0o1777, "/tmp/app.sh": 0o644})
		w.ThenTheImageOwnersAre(owners{"/tmp": "0:0", "/tmp/app.sh": "65534:65534"})
	})

	t.Run("A parent the image has keeps its owner when files beneath it are added for another", func(t *testing.T) {
		w := newAddWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="code", include=["app.sh"])
			layer(name="srv", base="busybox", steps=[run(["sh", "-c", "mkdir /srv && chown nobody:nobody /srv"])])
			layer(name="app", base="srv", steps=[add("code", "/srv/app", owner="1000")])
		`)
		w.GivenTheProjectHolds(files{"app.sh": "echo hi"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageOwnersAre(owners{"/srv": "65534:65534", "/srv/app": "1000:1000", "/srv/app/app.sh": "1000:1000"})
	})

	t.Run("A fileset's own directories don't replace the image's", func(t *testing.T) {
		w := newAddWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="code", include=["app.sh"])
			glob(name="nested", include=["tmp/app.sh"], mode_mask=0o777)
			layer(name="tmp", base="busybox", steps=[add("code", "/tmp")])
			layer(name="app", base="tmp", steps=[add("nested", "/")])
		`)
		w.GivenTheProjectHolds(files{"app.sh": "echo hi", "tmp/app.sh": "echo hi"})
		w.GivenTheProjectModes(modes{"tmp": 0o700})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/tmp"), files{"/tmp/app.sh": "echo hi"})
		w.ThenTheImageModesAre(modes{"/tmp": 0o1777})
	})

	t.Run("A directory the fileset selects brings its own mode and the selected ownership", func(t *testing.T) {
		w := newAddWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="all", include=["."], exclude=["BUILD"], mode_mask=0o777)
			layer(name="app", base="busybox", steps=[add("all", "/", owner="nobody")])
		`)
		w.GivenTheProjectHolds(files{"tmp/app.sh": "echo hi"})
		w.GivenTheProjectModes(modes{"tmp": 0o700})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageModesAre(modes{"/tmp": 0o700})
		w.ThenTheImageOwnersAre(owners{"/tmp": "65534:65534", "/tmp/app.sh": "65534:65534"})
	})

	t.Run("A new destination gets the usual directory mode", func(t *testing.T) {
		w := newAddWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="code", include=["app.sh"])
			layer(name="app", base="busybox", steps=[add("code", "/app")])
		`)
		w.GivenTheProjectHolds(files{"app.sh": "echo hi"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageModesAre(modes{"/app": 0o755})
	})
}

// TestAdd_DestinationMustBeAbsolute specifies that workdir() paths and add() destinations, which can refer to
// environment variables, must be absolute after expansion.
func TestAdd_DestinationMustBeAbsolute(t *testing.T) {
	for _, example := range []struct {
		name  string
		step  string
		error string // after the layer's name
	}{
		{
			name:  "An add() destination that is relative once expanded fails",
			step:  `add("code", "$DIR/app")`,
			error: `add("code", "$DIR/app") failed: dest "$DIR/app" is "srv/app", which must be an absolute path`,
		},
		{
			name:  "A workdir() path that is relative once expanded fails",
			step:  `workdir("$DIR/app")`,
			error: `workdir("$DIR/app") failed: path "$DIR/app" is "srv/app", which must be an absolute path`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newAddWorld(t)
			w.GivenTheBuildFileDeclares(`glob(name="code", include=["app.sh"])`)
			w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[build_env(DIR="srv"), ` +
				example.step + `])`)
			w.GivenTheProjectHolds(files{"app.sh": "echo hi"})
			w.WhenISave("app")
			w.ThenItFailsWith(`layer "app": ` + example.error)
			w.ThenNothingIsSaved()
			w.ThenNoImageIsLeftBehind()
		})
	}

	for _, example := range []struct {
		name    string
		sources string // the BUILD text that declares what step adds, if anything
		step    string
		error   string
	}{
		{
			name:    "An add() destination that is relative as written fails as the BUILD file loads",
			sources: `glob(name="code", include=["app.sh"])`,
			step:    `add("code", "app")`,
			error:   `/project/BUILD:3:45: add: dest "app" must be an absolute path`,
		},
		{
			name:  "A workdir() path that is relative as written fails as the BUILD file loads",
			step:  `workdir("app")`,
			error: `/project/BUILD:2:49: workdir: path "app" must be an absolute path`,
		},
		{
			name:  "A workdir() path relative before its variable fails as the BUILD file loads",
			step:  `workdir("app/$DIR")`,
			error: `/project/BUILD:2:49: workdir: path "app/$DIR" must be an absolute path`,
		},
		{
			name:  "An empty workdir() path fails as the BUILD file loads",
			step:  `workdir("")`,
			error: `/project/BUILD:2:49: workdir: path "" must be an absolute path`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newAddMistakeWorld(t)
			w.GivenTheBuildFileDeclares(example.sources)
			w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[` + example.step + `])`)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestAdd_OwnerAndGroupAreStrings specifies that owner and group require strings, including numeric IDs
// such as owner = "1000".
func TestAdd_OwnerAndGroupAreStrings(t *testing.T) {
	for _, example := range []struct {
		name  string
		args  string // add()'s arguments after its source and destination
		error string
	}{
		{
			name:  "A numeric owner that isn't a string fails",
			args:  `owner=1000`,
			error: `/project/BUILD:3:45: add: for parameter "owner": got int, want string`,
		},
		{
			name:  "A numeric group that isn't a string fails",
			args:  `group=2000`,
			error: `/project/BUILD:3:45: add: for parameter "group": got int, want string`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newAddMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`glob(name="code", include=["app.sh"])`)
			w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[add("code", "/app", ` +
				example.args + `)])`)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestAdd_OwnerAndGroupAreGivenApart specifies that owner names a user and group a group,
// so neither holds the other, as "user:group" would.
func TestAdd_OwnerAndGroupAreGivenApart(t *testing.T) {
	for _, example := range []struct {
		name  string
		args  string // add()'s arguments after its source and destination
		error string
	}{
		{
			name:  "An owner with a group fails, saying to give them apart",
			args:  `owner="app:app"`,
			error: `/project/BUILD:3:45: add: owner "app:app" must not contain ":"; give owner and group apart`,
		},
		{
			name:  "A group with a colon fails, saying to give them apart",
			args:  `group="app:app"`,
			error: `/project/BUILD:3:45: add: group "app:app" must not contain ":"; give owner and group apart`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newAddMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`glob(name="code", include=["app.sh"])`)
			w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[add("code", "/app", ` +
				example.args + `)])`)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}
