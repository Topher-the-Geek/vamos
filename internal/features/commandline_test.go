package features

import (
	"strings"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
)

// newCommandLineWorld starts a scenario of the command line feature, from its background:
// a project whose BUILD file would build the layer "app", were the command line right.
// Every scenario's command line fails, or shows help, before anything is built, so it needs no buildah.
// Commands run in a working directory of the scenario's own, so that a relative path that a command line names,
// were it written, wouldn't land in the source tree.
func newCommandLineWorld(t *testing.T) *world {
	t.Helper()
	t.Chdir(t.TempDir())
	w := newWorld(t)
	w.GivenAProjectThatPullsBusybox()
	w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", command=["echo", "hello"])`)
	return w
}

// aDigest is a digest, for an image name that must not include one.
var aDigest = "@sha256:" + strings.Repeat("0123456789abcdef", 4)

// TestCommandLine_HelpListsTheCommands specifies that vamos without a command shows its help,
// which lists the commands.
func TestCommandLine_HelpListsTheCommands(t *testing.T) {
	t.Run("vamos without a command shows the commands", func(t *testing.T) {
		w := newCommandLineWorld(t)
		w.WhenIRunVamosAlone()
		w.ThenItSucceeds()
		for _, command := range []string{"build", "push", "load", "save", "export", "prune", "coordinator", "worker"} {
			w.ThenTheOutputIncludes("\n  " + command + " ")
		}
	})

	t.Run("Help shows --tls-verify's default", func(t *testing.T) {
		w := newCommandLineWorld(t)
		w.WhenIRun("--help")
		w.ThenItSucceeds()
		w.ThenALineOfTheOutputHolds("--tls-verify ", "(default true)")
	})
}

// TestCommandLine_UsageMistakesPrintUsage specifies that command-line usage mistakes print usage,
// and that runtime failures do not: an unknown command or flag, a command without the arguments it takes,
// a flag of another command, or a flag without a value it takes.
func TestCommandLine_UsageMistakesPrintUsage(t *testing.T) {
	for _, example := range []struct {
		name   string
		args   []string
		errors []string
	}{
		{
			name:   "An unknown command fails",
			args:   []string{"publish"},
			errors: []string{`unknown command "publish"`},
		},
		{
			name:   "A misspelled command fails, naming the one meant",
			args:   []string{"pusj", "app"},
			errors: []string{`unknown command "pusj"; did you mean "push"?`},
		},
		{
			name:   "An unknown flag fails",
			args:   []string{"save", "app", "app.tar", "--bogus"},
			errors: []string{"unknown flag: --bogus"},
		},
		{
			name:   "An unknown shorthand flag fails",
			args:   []string{"push", "-x", "app", "app"},
			errors: []string{"unknown shorthand flag: 'x' in -x"},
		},
		{
			name:   "A push without a target fails, showing an example",
			args:   []string{"push"},
			errors: []string{"missing target and image name", "vamos push server ghcr.io/me/app:1.0"},
		},
		{
			name:   "A push without an image name fails, showing an example",
			args:   []string{"push", "app"},
			errors: []string{`missing image name after target "app"`, "vamos push server ghcr.io/me/app:1.0"},
		},
		{
			name:   "A load without an image name fails, showing an example",
			args:   []string{"load", "app"},
			errors: []string{`missing image name after target "app"`, "vamos load server app:dev"},
		},
		{
			name:   "A save without a file fails, showing an example",
			args:   []string{"save", "app"},
			errors: []string{`missing output file after target "app"`, "vamos save server app.tar"},
		},
		{
			name:   "An export without a folder fails, showing an example",
			args:   []string{"export", "app"},
			errors: []string{`missing folder after target "app"`, "vamos export codegen out"},
		},
		{
			name:   "A build without targets fails, showing an example",
			args:   []string{"build"},
			errors: []string{"missing target name", "vamos build packages"},
		},
		{
			name:   "A push with two images fails",
			args:   []string{"push", "app", "app:1", "app:2"},
			errors: []string{"expected a target and one image name, got 3 arguments: app app:1 app:2"},
		},
		{
			name:   "A save with two files fails",
			args:   []string{"save", "app", "a.tar", "b.tar"},
			errors: []string{"expected a target and one output file, got 3 arguments: app a.tar b.tar"},
		},
		{
			name:   "A prune with an argument fails, since it takes none",
			args:   []string{"prune", "all"},
			errors: []string{"vamos prune takes no arguments, got: all"},
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newCommandLineWorld(t)
			w.WhenIRun(example.args...)
			w.ThenItFailsShowingUsage(example.errors...)
		})
	}
}

// TestCommandLine_CommandSpecificFlagsBelongToTheirCommands specifies that the command-specific flags are
// build --rebuild, push --push-from, save --tag or -t, and export --replace, and that another command doesn't take
// them.
func TestCommandLine_CommandSpecificFlagsBelongToTheirCommands(t *testing.T) {
	for _, example := range []struct {
		name  string
		args  []string
		error string
	}{
		{
			name:  "--tag on push fails",
			args:  []string{"push", "app", "app", "--tag", "app:1"},
			error: "unknown flag: --tag",
		},
		{
			name:  "--replace on save fails",
			args:  []string{"save", "app", "app.tar", "--replace"},
			error: "unknown flag: --replace",
		},
		{
			name:  "--rebuild on push fails",
			args:  []string{"push", "app", "app", "--rebuild"},
			error: "unknown flag: --rebuild",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newCommandLineWorld(t)
			w.WhenIRun(example.args...)
			w.ThenItFailsShowingUsage(example.error)
		})
	}
}

// TestCommandLine_FlagsTakeValuesOfTheirKind specifies that a flag that takes a value is given one of its kind:
// a boolean for --tls-verify, which given alone means true, executor or client for --push-from, a time for
// --timestamp, no earlier than 1970, and id=ID with env=VARIABLE or src=FILE for --secret, each ID once.
func TestCommandLine_FlagsTakeValuesOfTheirKind(t *testing.T) {
	for _, example := range []struct {
		name   string
		args   []string
		errors []string
	}{
		{
			name:   "A flag without its value fails",
			args:   []string{"save", "app", "app.tar", "--tag"},
			errors: []string{"flag needs an argument: --tag"},
		},
		{
			name:   "-C without its value fails",
			args:   []string{"save", "app", "app.tar", "-C"},
			errors: []string{"flag needs an argument: 'C' in -C"},
		},
		{
			name:   "A --tls-verify that isn't a boolean fails",
			args:   []string{"push", "app", "app", "--tls-verify=maybe"},
			errors: []string{`invalid argument "maybe" for "--tls-verify" flag`},
		},
		{
			name:   "A --push-from that isn't executor or client fails",
			args:   []string{"push", "app", "app", "--push-from=remote"},
			errors: []string{`invalid argument "remote" for "--push-from" flag: want "executor" or "client"`},
		},
		{
			name:   "A --timestamp that isn't a time fails",
			args:   []string{"save", "app", "app.tar", "--timestamp", "yesterday"},
			errors: []string{`invalid argument "yesterday" for "--timestamp" flag`, "want Unix seconds"},
		},
		{
			name:   "A --timestamp before 1970 fails",
			args:   []string{"build", "app", "--timestamp=-1"},
			errors: []string{`invalid argument "-1" for "--timestamp" flag`, "must not be before 1970"},
		},
		{
			name:   "A --secret without a source fails",
			args:   []string{"build", "app", "--secret", "id=token"},
			errors: []string{`invalid argument "id=token"`, `secret "token" needs env=VARIABLE or src=FILE`},
		},
		{
			name:   "A --secret without an ID fails",
			args:   []string{"build", "app", "--secret", "token,env=TOKEN"},
			errors: []string{`"token" isn't key=value; want id=ID,env=VARIABLE or id=ID,src=FILE`},
		},
		{
			name:   "A --secret with an unknown key fails",
			args:   []string{"build", "app", "--secret", "id=token,file=/t"},
			errors: []string{`unknown key "file"; want id=ID,env=VARIABLE or id=ID,src=FILE`},
		},
		{
			name:   "A --secret given twice fails",
			args:   []string{"build", "app", "--secret", "id=token,env=A", "--secret", "id=token,src=t"},
			errors: []string{`secret "token" is given twice`},
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newCommandLineWorld(t)
			w.WhenIRun(example.args...)
			w.ThenItFailsShowingUsage(example.errors...)
		})
	}

	// That it means true shows only against a registry, as the push feature does.
	t.Run("--tls-verify given alone takes no value", func(t *testing.T) {
		w := newCommandLineWorld(t)
		w.WhenIRun("push", "--tls-verify", "app")
		w.ThenItFailsShowingUsage(`missing image name after target "app"`) // rather than --tls-verify taking "app"
	})
}

// TestCommandLine_ImageNamesAreReferences specifies that the image names that push, load and save --tag give
// are lowercase references, with a tag or none, and without a digest.
func TestCommandLine_ImageNamesAreReferences(t *testing.T) {
	for _, example := range []struct {
		name   string
		args   []string
		errors []string
	}{
		{
			name:   "An uppercase name to push fails",
			args:   []string{"push", "app", "MyApp"},
			errors: []string{`invalid image name "MyApp"`, "must be lowercase"},
		},
		{
			name:   "An uppercase name to load fails",
			args:   []string{"load", "app", "MyApp"},
			errors: []string{`invalid image name "MyApp"`, "must be lowercase"},
		},
		{
			name:   "A name with a space fails",
			args:   []string{"load", "app", "my app"},
			errors: []string{`invalid image name "my app"`},
		},
		{
			name:   "A name with an empty tag fails",
			args:   []string{"load", "app", "app:"},
			errors: []string{`invalid image name "app:"`},
		},
		{
			name:   "A name to push with a digest fails",
			args:   []string{"push", "app", "app" + aDigest},
			errors: []string{`image name "app` + aDigest + `" must not include a digest`},
		},
		{
			name:   "A name to load with a digest fails",
			args:   []string{"load", "app", "app" + aDigest},
			errors: []string{`image name "app` + aDigest + `" must not include a digest`},
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newCommandLineWorld(t)
			w.WhenIRun(example.args...)
			w.ThenItFailsWith(example.errors...)
			w.ThenNoTargetRan()
		})
	}

	t.Run("A --tag with a digest fails", func(t *testing.T) {
		w := newCommandLineWorld(t)
		w.WhenISave("app", tagged("app"+aDigest))
		w.ThenItFailsWith(`image name "app` + aDigest + `" must not include a digest`)
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
	})

	t.Run("A --tag that isn't a name fails", func(t *testing.T) {
		w := newCommandLineWorld(t)
		w.WhenISave("app", tagged("Bad Tag"))
		w.ThenItFailsWith(`invalid image name "Bad Tag"`)
		w.ThenNoTargetRan()
		w.ThenNothingIsSaved()
	})
}

// TestCommandLine_DirectoryChoosesThePackage specifies that -C names an existing directory, relative to the working
// directory, as other tools do, whose targets can be named without a label.
func TestCommandLine_DirectoryChoosesThePackage(t *testing.T) {
	for _, example := range []struct {
		name  string
		dir   string
		error string
	}{
		{
			name:  "A directory that isn't there fails",
			dir:   "/nope",
			error: "directory /nope does not exist",
		},
		{
			name:  "A file fails",
			dir:   testenv.ProjectDir + "/BUILD",
			error: testenv.ProjectDir + "/BUILD is not a directory",
		},
		{
			name:  "A relative directory is relative to the working directory",
			dir:   "sub",
			error: "directory <working directory>/sub does not exist",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newCommandLineWorld(t)
			w.GivenTheCommandRunsIn(example.dir)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}
