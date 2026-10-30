package features

import (
	"cmp"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/spf13/afero"
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
	flags   []string
}

// sendingCommands are the commands that send what they build.
var sendingCommands = []string{"save", "export"}

// everyCommand are the commands that When steps run with options.
var everyCommand = []string{"save", "export", "build"}

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

// stampedAt gives --timestamp stamp, a time as RFC 3339 or Unix seconds give it. Save, export and build take it.
func stampedAt(stamp string) option {
	return option{"stampedAt", everyCommand, func(_ *world, c *commandLine) {
		c.flags = append(c.flags, "--timestamp", stamp)
	}}
}

// setting gives --set for each of sets, such as "mode=debug". Save, export and build take it.
func setting(sets ...string) option {
	return option{"setting", everyCommand, func(_ *world, c *commandLine) {
		for _, set := range sets {
			c.flags = append(c.flags, "--set", set)
		}
	}}
}

// withSecrets gives --secret for each of secrets, such as "id=token,env=TOKEN",
// in which <outside> stands for the folder of GivenAFileOutsideTheProjectHolds. Save, export and build take it.
func withSecrets(secrets ...string) option {
	return option{"withSecrets", everyCommand, func(w *world, c *commandLine) {
		w.t.Helper()
		for _, secret := range secrets {
			if strings.Contains(secret, "<outside>") && w.outside == "" {
				w.t.Fatalf("%q names <outside>, but no file outside the project was given", secret)
			}
			c.flags = append(c.flags, "--secret", strings.ReplaceAll(secret, "<outside>", w.outside))
		}
	}}
}

// forPlatform builds for platform, such as linux/arm64, with --platform. Save, export and build take it.
func forPlatform(platform string) option {
	return option{"forPlatform", everyCommand, func(_ *world, c *commandLine) {
		c.flags = append(c.flags, "--platform", platform)
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
// Save, export and build take it.
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
	"--tag":       "tagged",
	"-t":          "tagged",
	"--timestamp": "stampedAt",
	"--set":       "setting",
	"--secret":    "withSecrets",
	"--platform":  "forPlatform",
	"--replace":   "replacing",
	"--rebuild":   "rebuilding",
	"-C":          "GivenIWorkIn",
	"--dir":       "GivenIWorkIn",
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
	}
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
	if w.prepared != "" {
		w.t.Fatal("build sends nothing, so it doesn't use the folder that Given steps prepared")
	}
	w.dest, w.inProject, w.sent = "", false, "" // build sends nothing, so nothing can be checked as unsent
	args := append([]string{"build", target}, c.targets...)
	w.run(append(args, c.flags...)...)
}

// newDest gives the next command a folder of its own to send to, so that checks see only what it sent,
// unless Given steps have put something in one for it.
func (w *world) newDest() string {
	w.dest, w.inProject = w.destination(), false
	w.prepared = ""
	return w.dest
}
