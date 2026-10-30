package features

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
)

// Then steps check what the last command did. Checks of whether it succeeded stop the scenario if they fail;
// the others report their failure and let the scenario go on.

// ThenItSucceeds checks that the command succeeded and reported sending its image or files if it sends any,
// skipping the scenario if it failed for want of the emulator that GivenThisMachineEmulates says it needs,
// as "exec format error" says,
// and nothing sent if it doesn't, that a save or a push reported the image's ID, or an index's digest,
// and that a push named the image it pushed to.
// It stops the scenario if the command failed or reported sending wrongly.
func (w *world) ThenItSucceeds() {
	w.t.Helper()
	if w.err != nil && w.emulating != "" && w.reported("exec format error") {
		w.t.Skipf("no emulator runs %s's programs here: %v", w.emulating, w.err)
	}
	if w.err != nil {
		w.t.Fatalf("vamos failed: %v\n%s", w.err, w.outputs())
	}
	if w.sent == "" {
		if slices.ContainsFunc(sentReports, w.triedToReport) {
			w.t.Fatalf("reported something sent, want nothing\n%s", w.outputs())
		}
	} else if !strings.HasPrefix(lastLine(w.stdout), w.sent) {
		w.t.Fatalf("stdout = %q, want its last line to report %q", w.stdout, w.sent)
	}
	var id string
	switch {
	case w.index != nil:
		// An index is reported by its digest, as a registry names it.
		digest, err := w.index.Digest()
		if err != nil {
			w.t.Fatal(err)
		}
		id = digest.String()
	case w.image != nil:
		// The ID reported is the one the image has where it was sent, as `docker images` shows it.
		id = w.imageID(w.image)
	}
	fields := strings.Fields(lastLine(w.stdout))
	if id != "" && (len(fields) == 0 || fields[len(fields)-1] != id) {
		w.t.Errorf("stdout = %q, want it to report the sent image's ID, %s", w.stdout, id)
	}
	if w.sentAs != "" && (len(fields) < 2 || fields[len(fields)-2] != w.sentAs) {
		w.t.Errorf("stdout = %q, want it to name the image sent as, %s", w.stdout, w.sentAs)
	}
}

// lastLine returns the last line of s, without its newline.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	return lines[len(lines)-1]
}

// ThenItFailsForTheClosedOutput checks that the command failed, as ThenItFailsWith checks, with wants, in which
// <closed> stands for the error that standard output gave it, after GivenStandardOutputIsClosed, or with only that
// error, without wants, and that the error wraps that error, rather than only quoting it.
func (w *world) ThenItFailsForTheClosedOutput(wants ...string) {
	w.t.Helper()
	if len(wants) == 0 {
		wants = []string{"<closed>"}
	}
	var expanded []string
	for _, want := range wants {
		expanded = append(expanded, strings.ReplaceAll(want, "<closed>", errOutputClosed.Error()))
	}
	w.ThenItFailsWith(expanded...)
	if !errors.Is(w.err, errOutputClosed) {
		w.t.Errorf("the command's error %v doesn't wrap standard output's", w.err)
	}
}

// ThenItTriedToPrintOnly checks that the command, whose standard output was closed, tried to print there only what
// starts with want: that nothing it printed before failed.
func (w *world) ThenItTriedToPrintOnly(want string) {
	w.t.Helper()
	if !strings.HasPrefix(w.tried, want) {
		w.t.Errorf("the command tried to print %q, want only what starts with %q", w.tried, want)
	}
}

// ThenItFailsWith checks that the command failed, at run time, with an error that contains each of wants,
// printed no usage, and reported sending nothing. In wants, placeholders stand for what atPlaceholders says, such as
// <destination> for the folder that the command sent to, and <coordinator> for the scenario's coordinator's URL.
// It stops the scenario if any of these doesn't hold.
func (w *world) ThenItFailsWith(wants ...string) {
	w.t.Helper()
	w.fails(false, wants)
}

// ThenItFailsShowingUsage checks that the command failed as a mistake on the command line does:
// as ThenItFailsWith checks, but printing usage.
func (w *world) ThenItFailsShowingUsage(wants ...string) {
	w.t.Helper()
	w.fails(true, wants)
}

// fails checks that the command failed with an error that contains each of wants, printing usage or not,
// and reported sending nothing, stopping the scenario if any of these doesn't hold.
func (w *world) fails(usage bool, wants []string) {
	w.t.Helper()
	if w.err == nil {
		w.t.Fatalf("vamos succeeded, want an error\n%s", w.outputs())
	}
	if len(wants) == 0 {
		w.t.Fatal("no error to expect")
	}
	var errs []error
	for _, want := range wants {
		want = w.atPlaceholders(want)
		if !strings.Contains(w.err.Error(), want) {
			errs = append(errs, fmt.Errorf("error = %q, want it to contain %q", w.err, want))
		}
	}
	if printed := w.triedToReport("Usage:"); printed && !usage {
		errs = append(errs, fmt.Errorf("printed usage for a failure at run time\n%s", w.outputs()))
	} else if !printed && usage {
		errs = append(errs, fmt.Errorf("printed no usage for a mistake on the command line\n%s", w.outputs()))
	}
	if slices.ContainsFunc(sentReports, w.reported) {
		errs = append(errs, fmt.Errorf("reported something sent after an error\n%s", w.outputs()))
	}
	if err := errors.Join(errs...); err != nil {
		w.t.Fatal(err)
	}
}

// atPlaceholders returns s with each <destination> replaced by the folder that the last command sent to,
// each <working directory> by the test's, each <registry> by the host of the scenario's registry,
// each <outside> by the folder of GivenAFileOutsideTheProjectHolds, each <coordinator> by the scenario's coordinator's
// URL,
// and each <saved image N> by the short ID of the Nth image the scenario saved, as vamos shows one.
func (w *world) atPlaceholders(s string) string {
	w.t.Helper()
	s = w.atOutside(w.atRegistry(w.atCoordinator(s)))
	for i, img := range w.saved {
		if placeholder := fmt.Sprintf("<saved image %d>", i+1); strings.Contains(s, placeholder) {
			s = strings.ReplaceAll(s, placeholder, w.imageID(img)[:12])
		}
	}
	if strings.Contains(s, "<saved image ") {
		w.t.Fatalf("%q names a saved image that the scenario hasn't saved", s)
	}
	if strings.Contains(s, "<working directory>") {
		wd, err := os.Getwd()
		if err != nil {
			w.t.Fatal(err)
		}
		s = strings.ReplaceAll(s, "<working directory>", wd)
	}
	if strings.Contains(s, "<destination>") && w.dest == "" {
		w.t.Fatalf("%q names <destination>, but no command has run that sends images or files", s)
	}
	return strings.ReplaceAll(s, "<destination>", w.dest)
}

// ThenTheCommandDoesNotSay checks that unwanted is in neither the command's error nor its output.
func (w *world) ThenTheCommandDoesNotSay(unwanted string) {
	w.t.Helper()
	if (w.err != nil && strings.Contains(w.err.Error(), unwanted)) || w.reported(unwanted) {
		w.t.Errorf("the command said %q\nerror: %v\n%s", unwanted, w.err, w.outputs())
	}
}

// reported reports whether the command wrote s to its standard output or error.
func (w *world) reported(s string) bool {
	return strings.Contains(w.stdout, s) || strings.Contains(w.stderr, s)
}

// triedToReport reports whether the command wrote s, as reported says, or tried to print it to standard output that
// was closed: for checks that it printed nothing it shouldn't, whether or not anyone saw it.
func (w *world) triedToReport(s string) bool {
	return w.reported(s) || strings.Contains(w.tried, s)
}

// outputs shows what the command wrote, for a failure's message.
func (w *world) outputs() string {
	if w.tried != "" {
		return fmt.Sprintf("stdout, closed, refused:\n%s\nstderr:\n%s", w.tried, w.stderr)
	}
	return fmt.Sprintf("stdout:\n%s\nstderr:\n%s", w.stdout, w.stderr)
}

// ThenTheOutputIncludes checks the command's standard output, where it shows its targets' statuses and steps' output.
// In want, placeholders stand for what atPlaceholders says, such as <destination> for the folder the command sent to.
func (w *world) ThenTheOutputIncludes(want string) {
	w.t.Helper()
	if want = w.atPlaceholders(want); !strings.Contains(w.stdout, want) {
		w.t.Errorf("stdout = %q, want it to include %q", w.stdout, want)
	}
}

// ThenALineOfTheOutputHolds checks that one line of the command's standard output holds each of parts, in order.
func (w *world) ThenALineOfTheOutputHolds(parts ...string) {
	w.t.Helper()
	for line := range strings.Lines(w.stdout) {
		rest, held := line, true
		for _, part := range parts {
			_, after, ok := strings.Cut(rest, part)
			if !ok {
				held = false
				break
			}
			rest = after
		}
		if held {
			return
		}
	}
	w.t.Errorf("stdout = %q, want a line that holds %q, in order", w.stdout, parts)
}

// ThenTheOutputIs checks that the command's standard output is exactly want.
func (w *world) ThenTheOutputIs(want string) {
	w.t.Helper()
	if w.stdout != want {
		w.t.Errorf("stdout = %q, want %q", w.stdout, want)
	}
}

// ThenTheOutputDoesNotInclude checks that unwanted isn't in the command's standard output.
func (w *world) ThenTheOutputDoesNotInclude(unwanted string) {
	w.t.Helper()
	if strings.Contains(w.stdout, unwanted) {
		w.t.Errorf("stdout = %q, want it not to include %q", w.stdout, unwanted)
	}
}

// ThenItLastReports checks that the last line of the command's standard output starts with want.
// In want, <destination> stands for the folder that the command sent to.
func (w *world) ThenItLastReports(want string) {
	w.t.Helper()
	if want = w.atPlaceholders(want); !strings.HasPrefix(lastLine(w.stdout), want) {
		w.t.Errorf("stdout = %q, want its last line to start with %q", w.stdout, want)
	}
}

// ThenTheCommandSays checks that the command wrote want to its standard output or error.
func (w *world) ThenTheCommandSays(want string) {
	w.t.Helper()
	if !w.reported(want) {
		w.t.Errorf("the command didn't say %q\n%s", want, w.outputs())
	}
}

// ThenTheErrorOutputIncludes checks the command's standard error, where steps' errors go.
func (w *world) ThenTheErrorOutputIncludes(want string) {
	w.t.Helper()
	if !strings.Contains(w.stderr, want) {
		w.t.Errorf("stderr = %q, want it to include %q", w.stderr, want)
	}
}

// observations are what a command's steps saw, by key, as they report it on lines of their output of the form
// "observed <key>=<value>": what leaves no trace in the image, such as a step's variables, secret files or hostname.
// A step observes and reports; the scenario judges, so that its failure names what was wrong.
type observations map[string]string

// ThenTheStepsObserved checks that the observations that the command's steps reported are exactly want:
// its failure lists each key whose value differed, that no step reported, or that a step reported but want doesn't
// name, or reported twice. A cached step's observations are replayed with its output.
func (w *world) ThenTheStepsObserved(want observations) {
	w.t.Helper()
	if len(want) == 0 {
		w.t.Fatal("no observations to expect")
	}
	for key := range want {
		if key == "" || key != strings.TrimSpace(key) || strings.Contains(key, "=") {
			w.t.Fatalf("%q can't be observed: a key is neither empty nor padded, and holds no =", key)
		}
	}
	if err := matchObservations(w.stdout, want); err != nil {
		w.t.Errorf("%v\nstdout:\n%s", err, w.stdout)
	}
}

// matchObservations checks that the observations in output are exactly want.
func matchObservations(output string, want observations) error {
	got := observations{}
	var errs []error
	for line := range strings.Lines(output) {
		rest, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "observed ")
		if !ok {
			continue
		}
		key, value, ok := strings.Cut(rest, "=")
		if !ok || key == "" || key != strings.TrimSpace(key) {
			errs = append(errs, fmt.Errorf("%q observes no key=value", strings.TrimSpace(line)))
			continue
		}
		if first, ok := got[key]; ok {
			errs = append(errs, fmt.Errorf("%q observed again, as %q, after %q", key, value, first))
			continue
		}
		got[key] = value
	}
	for _, key := range slices.Sorted(maps.Keys(want)) {
		if value, ok := got[key]; !ok {
			errs = append(errs, fmt.Errorf("%q not observed, want %q", key, want[key]))
		} else if value != want[key] {
			errs = append(errs, fmt.Errorf("%q observed %q, want %q", key, value, want[key]))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(got)) {
		if _, ok := want[key]; !ok {
			errs = append(errs, fmt.Errorf("%q observed %q too", key, got[key]))
		}
	}
	return errors.Join(errs...)
}

// status is what a command did with a target, as ThenTheTargetsWere expects it.
type status int

const (
	ran         status = iota + 1 // it ran, its result not in the cache
	reused                        // its cached result was reused
	rebuilt                       // it ran again, with --rebuild
	ranOrReused                   // either, for a scenario that doesn't specify which
	notBuilt                      // the command showed no status of it, in any configuration
)

// shownAs are the statuses that a command shows of a target that it ran, reused or rebuilt.
var shownAs = map[status][]string{
	ran:         {"building"},
	reused:      {"cached"},
	rebuilt:     {"rebuilding"},
	ranOrReused: {"building", "cached"},
}

func (s status) String() string {
	names := [...]string{"", "ran", "reused", "rebuilt", "ranOrReused", "notBuilt"}
	if s <= 0 || int(s) >= len(names) {
		return fmt.Sprintf("status(%d)", int(s))
	}
	return names[s]
}

// statuses are what a command did with targets, by how layer and task name them.
type statuses map[string]status

// layer names the layer name as a command's output does, configured as configuration, such as mode="debug",
// or with no arguments if there is none.
func layer(name string, configuration ...string) string {
	return target("layer", name, strings.Join(configuration, ", "))
}

// task names the task name as a command's output does, configured as configuration, such as mode="debug",
// or with no arguments if there is none.
func task(name string, configuration ...string) string {
	return target("task", name, strings.Join(configuration, ", "))
}

// target returns how the command's output names the target of kind named name, configured as configuration,
// such as `task "mode" (mode="debug")`, or with no arguments if configuration is empty.
func target(kind, name, configuration string) string {
	label := fmt.Sprintf("%s %q", kind, name)
	if configuration != "" {
		label += " (" + configuration + ")"
	}
	return label
}

// ThenTheTargetsWere checks what the command did with each target that want names, and ignores the others:
// that it showed one status of the target, with no reason after it, such as a cached result that can't be used,
// or for notBuilt, none at all, in any configuration. Its failure lists every target whose status differed.
func (w *world) ThenTheTargetsWere(want statuses) {
	w.t.Helper()
	if len(want) == 0 {
		w.t.Fatal("no statuses to expect")
	}
	labels := slices.Sorted(maps.Keys(want))
	for _, label := range labels {
		if _, ok := shownAs[want[label]]; !ok && want[label] != notBuilt {
			w.t.Fatalf("%s: no such status as %s", label, want[label])
		}
		if want[label] == notBuilt && label != unconfigured(label) {
			w.t.Fatalf("%s names a configuration, but notBuilt means in none; name the target alone", label)
		}
	}
	var errs []error
	for _, label := range labels {
		if want[label] == notBuilt {
			if lines := w.statusLines(label); len(lines) > 0 {
				errs = append(errs, fmt.Errorf("%s: shown as %q, want %s", label, lines, notBuilt))
			}
			continue
		}
		wants := shownAs[want[label]]
		if got := w.shownStatuses(label); len(got) != 1 || !slices.Contains(wants, got[0]) {
			errs = append(errs, fmt.Errorf("%s: %q, want one status of %q, %s", label, got, wants, want[label]))
		}
	}
	if err := errors.Join(errs...); err != nil {
		w.t.Errorf("%v\nstdout:\n%s", err, w.stdout)
	}
}

// unconfigured returns label, as target makes it, without the configuration that it names, if any.
func unconfigured(label string) string {
	kind, name, _ := strings.Cut(label, " ")
	quoted, err := strconv.QuotedPrefix(name)
	if err != nil {
		return label
	}
	return kind + " " + quoted
}

// shownStatuses returns the statuses that the command showed of the target that label names.
func (w *world) shownStatuses(label string) []string {
	prefix := label + ": "
	var shown []string
	for line := range strings.Lines(w.stdout) {
		if s, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), prefix); ok {
			shown = append(shown, s)
		}
	}
	return shown
}

// statusLines returns the lines of the command's output that show a status of the target that label names,
// with no configuration, in any configuration.
func (w *world) statusLines(label string) []string {
	var lines []string
	for line := range strings.Lines(w.stdout) {
		rest, ok := strings.CutPrefix(line, label)
		if ok && (strings.HasPrefix(rest, ": ") || strings.HasPrefix(rest, " (")) {
			lines = append(lines, strings.TrimSuffix(line, "\n"))
		}
	}
	return lines
}

// ThenNoTargetRan checks that the command wrote nothing to its standard output, where it shows targets' statuses
// and steps' output, nor to its standard error, where steps' errors go: it stopped before anything ran,
// as a mistake that loading the BUILD file catches stops it.
func (w *world) ThenNoTargetRan() {
	w.t.Helper()
	if w.stdout != "" || w.stderr != "" {
		w.t.Errorf("wrote output, want none, since nothing ran\n%s", w.outputs())
	}
}

// ThenTheTargetWasShownAs checks that the command showed exactly the statuses of the target that label names,
// in order, each starting with the one that shown gives, such as "rebuilding" or "building again, since",
// in which placeholders stand for what atPlaceholders says.
func (w *world) ThenTheTargetWasShownAs(label string, shown ...string) {
	w.t.Helper()
	if len(shown) == 0 {
		w.t.Fatal("no statuses to expect")
	}
	shown = w.allAtPlaceholders(shown)
	got := w.shownStatuses(label)
	matches := len(got) == len(shown)
	for i := 0; matches && i < len(got); i++ {
		matches = strings.HasPrefix(got[i], shown[i])
	}
	if !matches {
		w.t.Errorf("%s: shown as %q, want %q, in order\n%s", label, got, shown, w.stdout)
	}
}

// ThenTheTargetWasBuiltInOneConfiguration checks that the command showed statuses of the target that label names
// in one configuration alone, whatever its arguments. It fails the scenario at once if label names a configuration.
func (w *world) ThenTheTargetWasBuiltInOneConfiguration(label string) {
	w.t.Helper()
	if label != unconfigured(label) {
		w.t.Fatalf("%s names a configuration; name the target alone", label)
	}
	if lines := w.statusLines(label); len(lines) != 1 {
		w.t.Errorf("%s: shown as %q, want one status in one configuration\n%s", label, lines, w.stdout)
	}
}
