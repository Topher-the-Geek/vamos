package features

import (
	"cmp"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/spf13/afero"
	"go.podman.io/image/v5/docker/reference"
)

// When steps run the command line, as a user would: one step for each command, which takes the target it names
// and options for how to run it.

// What commands that send an image or files report once they have.
const (
	imageBuilt    = "Image built! "
	filesExported = "Files exported! "
)

// sentReports are what any command reports once it has sent an image, an index or files.
var sentReports = []string{" built! ", filesExported}

// option is a way to run a command, which When steps take after the target they name.
// A step fails the scenario at once given an option that its command doesn't take.
type option struct {
	name     string   // the function that made it, for a failure's message
	commands []string // the commands that take it
	apply    func(w *world, c *commandLine)
}

// commandLine is what a When step runs, as its options make it.
type commandLine struct {
	targets []string // the targets after the one the step names, which build takes
	name    string   // where save or export sends, if an option gives it, relative to the folder it sends to
	project bool     // whether name is relative to the directory that commands run in, in the project, instead
	sentBy  string   // the option that gave name
	tlsBy   string   // the option that gave --tls-verify, if one has
	flags   []string
}

// sendingCommands are the commands that send what they build to a folder.
var sendingCommands = []string{"save", "export"}

// everyCommand are the commands that build, which When steps run with options.
var everyCommand = []string{"save", "export", "build", "push", "load"}

// everyCommandAndGraph are everyCommand and graph, which configures a graph as they do, building nothing.
var everyCommandAndGraph = append(slices.Clone(everyCommand), "graph")

// to sends to the path name, relative to the folder that the command sends to,
// rather than to the target's name, with .tar for save. Save and export take it.
func to(name string) option {
	return option{"to", sendingCommands, func(w *world, c *commandLine) { c.sendTo(w, "to", name, false) }}
}

// intoTheProject saves to the path name within the directory that commands run in, in the project,
// which is kept on disk for it, rather than to a folder of the command's own. Save takes it.
// Steps about the destination then stop the scenario, since the project holds more than the command sent.
func intoTheProject(name string) option {
	return option{"intoTheProject", []string{"save"}, func(w *world, c *commandLine) {
		c.sendTo(w, "intoTheProject", name, true)
	}}
}

// sendTo makes the command send to name, within the project if project is true, as the option by says,
// failing the scenario if name is empty or another option has said where already.
func (c *commandLine) sendTo(w *world, by, name string, project bool) {
	w.t.Helper()
	if name == "" {
		w.t.Fatalf("%s() names no path to send to", by)
	}
	if c.sentBy != "" {
		w.t.Fatalf("both %s() and %s() say where to send", c.sentBy, by)
	}
	c.name, c.project, c.sentBy = name, project, by
}

// tagged records tag as the image's name in the archive, with --tag. Save takes it.
func tagged(tag string) option {
	return option{"tagged", []string{"save"}, func(_ *world, c *commandLine) {
		c.flags = append(c.flags, "--tag", tag)
	}}
}

// stampedAt gives --timestamp stamp, a time as RFC 3339 or Unix seconds give it. Every command takes it.
func stampedAt(stamp string) option {
	return option{"stampedAt", everyCommand, func(_ *world, c *commandLine) {
		c.flags = append(c.flags, "--timestamp", stamp)
	}}
}

// setting gives --set for each of sets, such as "mode=debug". Every command takes it.
func setting(sets ...string) option {
	return option{"setting", everyCommand, func(_ *world, c *commandLine) {
		for _, set := range sets {
			c.flags = append(c.flags, "--set", set)
		}
	}}
}

// withSecrets gives --secret for each of secrets, such as "id=token,env=TOKEN",
// in which <outside> stands for the folder of GivenAFileOutsideTheProjectHolds. Every command takes it.
func withSecrets(secrets ...string) option {
	return option{"withSecrets", everyCommand, func(w *world, c *commandLine) {
		w.t.Helper()
		for _, secret := range secrets {
			c.flags = append(c.flags, "--secret", w.atOutside(secret))
		}
	}}
}

// forPlatform builds for platform, such as linux/arm64, with --platform. Every command takes it.
func forPlatform(platform string) option {
	return option{"forPlatform", everyCommand, func(_ *world, c *commandLine) {
		c.flags = append(c.flags, "--platform", platform)
	}}
}

// fromTheClient pushes from the client, with --push-from=client, rather than from the executor. Push takes it.
func fromTheClient() option {
	return option{"fromTheClient", []string{"push"}, func(_ *world, c *commandLine) {
		c.flags = append(c.flags, "--push-from=client")
	}}
}

// withoutTLSVerification lets vamos itself reach a registry over plain HTTP, with --tls-verify=false.
// Every command takes it.
func withoutTLSVerification() option {
	return option{"withoutTLSVerification", everyCommand, func(w *world, c *commandLine) {
		c.verifyTLS(w, "withoutTLSVerification")
		c.flags = append(c.flags, "--tls-verify=false")
	}}
}

// withTLSVerification gives --tls-verify alone, which means true. Every command takes it.
func withTLSVerification() option {
	return option{"withTLSVerification", everyCommand, func(w *world, c *commandLine) {
		c.verifyTLS(w, "withTLSVerification")
		c.flags = append(c.flags, "--tls-verify")
	}}
}

// verifyTLS records that the option by gives --tls-verify, failing the scenario if another option has already.
func (c *commandLine) verifyTLS(w *world, by string) {
	w.t.Helper()
	if c.tlsBy != "" {
		w.t.Fatalf("both %s() and %s() give --tls-verify", c.tlsBy, by)
	}
	c.tlsBy = by
}

// onTheExecPlatform runs commands on platform, which EXEC_PLATFORM names, with --exec-platform. Every command
// takes it, and graph, which is configured for it.
func onTheExecPlatform(platform string) option {
	return option{"onTheExecPlatform", everyCommandAndGraph, func(_ *world, c *commandLine) {
		c.flags = append(c.flags, "--exec-platform", platform)
	}}
}

// replacing replaces what the folder exported to holds, with --replace. Export takes it.
func replacing() option {
	return option{"replacing", []string{"export"}, func(_ *world, c *commandLine) {
		c.flags = append(c.flags, "--replace")
	}}
}

// rebuilding runs the named targets again rather than reuse their cached results, with --rebuild. Build takes it.
func rebuilding() option {
	return option{"rebuilding", []string{"build"}, func(_ *world, c *commandLine) {
		c.flags = append(c.flags, "--rebuild")
	}}
}

// alongWith names targets after the one the step names. Build takes it.
func alongWith(targets ...string) option {
	return option{"alongWith", []string{"build"}, func(_ *world, c *commandLine) {
		c.targets = append(c.targets, targets...)
	}}
}

// withFlags gives flags as they are: for an example table whose rows are themselves flags,
// or a flag that one feature alone passes, which gets an option of its own once a second passes it too.
// It fails the scenario at once given a flag that an option names, so that the option's checks apply.
// Every command takes it.
func withFlags(flags ...string) option {
	return option{"withFlags", everyCommand, func(w *world, c *commandLine) {
		w.t.Helper()
		for _, flag := range flags {
			name, _, _ := strings.Cut(flag, "=")
			if len(name) > 2 && !strings.HasPrefix(name, "--") {
				name = name[:2] // a shorthand with its value attached, such as -tapp:1
			}
			if named, ok := namedFlags[name]; ok {
				w.t.Fatalf("withFlags() given %s; use %s()", flag, named)
			}
		}
		c.flags = append(c.flags, flags...)
	}}
}

// namedFlags are the options that give flags, by flag, and the step that gives -C.
var namedFlags = map[string]string{
	"--tag":           "tagged",
	"-t":              "tagged",
	"--timestamp":     "stampedAt",
	"--set":           "setting",
	"--secret":        "withSecrets",
	"--platform":      "forPlatform",
	"--exec-platform": "onTheExecPlatform",
	"--replace":       "replacing",
	"--rebuild":       "rebuilding",
	"--push-from":     "fromTheClient",
	"--tls-verify":    "withTLSVerification or withoutTLSVerification",
	"-C":              "GivenIWorkIn",
	"--dir":           "GivenIWorkIn",
}

// apply applies options to the command line of command, failing the scenario if command doesn't take one.
func (w *world) apply(command string, options []option) commandLine {
	w.t.Helper()
	var c commandLine
	for _, o := range options {
		if !slices.Contains(o.commands, command) {
			w.t.Fatalf("%s doesn't take %s()", command, o.name)
		}
		o.apply(w, &c)
	}
	return c
}

// WhenISave saves target to target.tar, in a folder of the command's own, unless options say where.
// It fails the scenario if a save that succeeds wrote no image that can be read.
func (w *world) WhenISave(target string, options ...option) {
	w.t.Helper()
	c := w.apply("save", options)
	name := cmp.Or(c.name, target+".tar")
	if !c.project {
		w.save("save", target, filepath.Join(w.newDest(), name), c.flags...)
		return
	}
	if w.prepared != "" {
		w.t.Fatal("saving into the project doesn't send to the folder that Given steps prepared")
	}
	if w.cDir != "" {
		w.t.Fatal("saving into the project needs a directory within it to run in, not GivenTheCommandRunsIn")
	}
	w.onDisk = true
	fsys := w.project()
	w.onDisk = false
	w.dest, w.inProject = filepath.Join(w.root, testenv.ProjectDir, w.dir), true
	w.saveIn(fsys, "save", target, filepath.Join(w.dest, name), c.flags...)
}

// WhenIWrite saves target with write, save's deprecated earlier name, to target.tar in a folder of the command's own.
// It is a step of its own, since write is a command of its own, rather than a way of saving.
func (w *world) WhenIWrite(target string) {
	w.t.Helper()
	w.save("write", target, filepath.Join(w.newDest(), target+".tar"))
}

// save runs command, save or write, to save target to path, with flags, and reads the image it saved.
func (w *world) save(command, target, path string, flags ...string) {
	w.t.Helper()
	w.saveIn(w.project(), command, target, path, flags...)
}

// saveIn is save in the project fsys.
func (w *world) saveIn(fsys afero.Fs, command, target, path string, flags ...string) {
	w.t.Helper()
	w.sent = imageBuilt
	w.runIn(fsys, append([]string{command, target, path}, flags...)...)
	if w.err == nil {
		img, err := tarball.ImageFromPath(path, nil)
		if err != nil {
			w.t.Fatalf("reading the saved image: %v", err)
		}
		w.image, w.archive = img, path
		if w.first == nil {
			w.first = img
		}
		w.saved = append(w.saved, img)
	}
}

// WhenIPush pushes target to the image name image, in which <registry> stands for the scenario's registry,
// with :latest if it gives no tag,
// and reads back what it pushed, an image or an index, for the steps about the image or the index to check.
// It fails the scenario if a push that succeeds pushed nothing that can be read.
func (w *world) WhenIPush(target, image string, options ...option) {
	w.t.Helper()
	c := w.apply("push", options)
	w.sendsNothing("WhenIPush")
	image = w.atRegistry(image)
	w.sent = " built! " // "Image built!" or "Index built!", once what was pushed says which
	w.run(append([]string{"push", target, image}, c.flags...)...)
	if w.sentAs = w.sentName(image); w.sentAs == "" {
		return // a name that push refused, rightly, for a scenario to check
	}
	if w.err != nil {
		w.sent = ""
		return
	}
	ref, err := name.ParseReference(image, name.Insecure)
	if err != nil {
		w.t.Fatal(err)
	}
	desc, err := remote.Get(ref, w.readingTheRegistry()...)
	if err != nil {
		w.t.Fatalf("reading what was pushed: %v", err)
	}
	if desc.MediaType.IsIndex() {
		w.index, err = desc.ImageIndex()
		w.sent = "Index built! "
	} else {
		w.image, err = desc.Image()
		w.sent = imageBuilt
	}
	if err != nil {
		w.t.Fatalf("reading what was pushed: %v", err)
	}
}

// WhenILoad loads target into Docker as the image name image, with :latest if it gives no tag, and, from the
// scenario's own daemon, reads back what it loaded, for the steps about the image to check: the archive that the
// daemon loaded, whose names ThenTheArchiveNames checks. It fails the scenario if a load that succeeds loaded
// anything but one image into the scenario's daemon. What it loads into the host's daemon is removed once the
// scenario ends.
func (w *world) WhenILoad(target, image string, options ...option) {
	w.t.Helper()
	c := w.apply("load", options)
	if w.daemon == "" {
		w.t.Fatal("no Given step gave a Docker daemon to load into")
	}
	w.sendsNothing("WhenILoad")
	w.loadedBefore = 0
	if w.docker != nil {
		w.loadedBefore = len(w.docker())
	}
	if w.daemon == "host" {
		if named, err := reference.ParseNormalizedNamed(image); err == nil {
			host, name := w.dockerHost, reference.FamiliarString(reference.TagNameOnly(named))
			w.t.Cleanup(func() {
				rmi := exec.Command("docker", "--host", host, "rmi", "--force", name)
				if out, err := rmi.CombinedOutput(); err != nil {
					w.t.Logf("removing %s from Docker: %v\n%s", name, err, out)
				}
			})
		}
	}
	w.sent = imageBuilt
	w.run(append([]string{"load", target, image}, c.flags...)...)
	w.loadRan = true
	if w.sentAs = w.sentName(image); w.err != nil {
		w.sent = ""
		return
	}
	if w.docker == nil {
		return // the host's daemon, which keeps what it loads
	}
	loaded := w.docker()[w.loadedBefore:]
	if len(loaded) != 1 {
		w.t.Fatalf("Docker loaded %d images, want 1", len(loaded))
	}
	w.archive = filepath.Join(w.t.TempDir(), "loaded.tar")
	if err := os.WriteFile(w.archive, loaded[0], 0o644); err != nil {
		w.t.Fatal(err)
	}
	img, err := tarball.ImageFromPath(w.archive, nil)
	if err != nil {
		w.t.Fatalf("reading what Docker loaded: %v", err)
	}
	w.image = img
}

// sentName returns image as push and load name it, with the tag that they give a name without one,
// or nothing for a name they rightly refuse, failing the scenario if the command took it.
func (w *world) sentName(image string) string {
	w.t.Helper()
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		if w.err == nil {
			w.t.Fatalf("sent as %s, which isn't an image name: %v", image, err)
		}
		return ""
	}
	return reference.FamiliarString(reference.TagNameOnly(named))
}

// WhenIPrune prunes the build cache, which sends nothing.
func (w *world) WhenIPrune() {
	w.t.Helper()
	w.sendsNothing("WhenIPrune")
	w.run("prune")
}

// WhenIExport exports target to the folder target, in a folder of the command's own, unless options say where.
func (w *world) WhenIExport(target string, options ...option) {
	w.t.Helper()
	c := w.apply("export", options)
	dir := filepath.Join(w.newDest(), cmp.Or(c.name, target))
	w.sent = filesExported
	w.run(append([]string{"export", target, dir}, c.flags...)...)
	if w.err == nil {
		w.exported = dir
	}
}

// WhenIBuild builds target, and those that alongWith names, into the cache.
func (w *world) WhenIBuild(target string, options ...option) {
	w.t.Helper()
	c := w.apply("build", options)
	w.sendsNothing("WhenIBuild")
	args := append([]string{"build", target}, c.targets...)
	w.run(append(args, c.flags...)...)
}

// WhenIShowTheGraph shows the graph that target configures, which the scenario keeps, for Then steps that compare
// what other commands do with it.
func (w *world) WhenIShowTheGraph(target string, options ...option) {
	w.t.Helper()
	c := w.apply("graph", options)
	w.sendsNothing("WhenIShowTheGraph")
	w.run(append([]string{"graph", target}, c.flags...)...)
	if w.err != nil {
		return
	}
	g, err := graph.Decode([]byte(w.stdout))
	if err != nil {
		w.t.Fatalf("reading the graph: %v", err)
	}
	w.graph = g
}

// WhenIRun runs vamos with args as they are, after -C, for a command line that the other When steps don't give:
// one that is itself a mistake, or that asks for help. A command line that builds is given by the When step of its
// command and the options that name its flags. It sends nothing that a later step can check.
func (w *world) WhenIRun(args ...string) {
	w.t.Helper()
	w.sendsNothing("WhenIRun")
	w.run(args...)
}

// WhenIRunVamosAlone runs vamos without arguments, not even -C, so that no Given step may say where it runs.
func (w *world) WhenIRunVamosAlone() {
	w.t.Helper()
	if (w.dir != "" && w.dir != ".") || w.cDir != "" {
		w.t.Fatal("vamos alone runs in no directory that a Given step can give")
	}
	w.sendsNothing("WhenIRunVamosAlone")
	w.execute(w.project(), nil)
}

// sendsNothing records that the next command, run by step, sends nothing, so that nothing can be checked as unsent,
// failing the scenario if Given steps prepared a folder for it to send to.
func (w *world) sendsNothing(step string) {
	w.t.Helper()
	if w.prepared != "" {
		w.t.Fatalf("%s doesn't send to the folder that Given steps prepared", step)
	}
	w.dest, w.inProject, w.sent = "", false, ""
}

// newDest gives the next command a folder of its own to send to, so that checks see only what it sent,
// unless Given steps have put something in one for it.
func (w *world) newDest() string {
	w.dest, w.inProject = w.destination(), false
	w.prepared = ""
	return w.dest
}
