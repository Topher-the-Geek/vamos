package features

import (
	"encoding/base64"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/registry"
)

// Given steps set up a scenario's project.

// GivenAProjectThatPullsBusybox starts the BUILD file with the pull "busybox", of busybox:1.36,
// the one BUILD text that a step writes rather than the scenario. It stops the scenario if the BUILD file holds
// anything already.
func (w *world) GivenAProjectThatPullsBusybox() {
	w.t.Helper()
	if w.files["BUILD"] != "" {
		w.t.Fatal("the BUILD file holds declarations already, which the pull would replace")
	}
	w.files["BUILD"] = testenv.PullBusybox
}

// GivenTheBuildFileDeclares appends build to the BUILD file, dedented.
func (w *world) GivenTheBuildFileDeclares(build string) {
	w.t.Helper()
	w.GivenThePackageDeclares(".", build)
}

// GivenTheBuildFileChanges replaces the first old in the BUILD file with new.
// The BUILD text that it changes is the scenario's own, or a fixture declared in the same feature's file,
// so that a reader can find what it changes. It stops the scenario if the BUILD file doesn't hold old.
func (w *world) GivenTheBuildFileChanges(old, new string) {
	w.t.Helper()
	if !strings.Contains(w.files["BUILD"], old) {
		w.t.Fatalf("the BUILD file doesn't hold %q\n%s", old, w.files["BUILD"])
	}
	w.files["BUILD"] = strings.Replace(w.files["BUILD"], old, new, 1)
}

// GivenThePackageDeclares appends build, dedented, to the BUILD file of the package in dir, "." for the project's.
// A blank build appends nothing, though the package then has its BUILD file, empty if it had none.
func (w *world) GivenThePackageDeclares(dir, build string) {
	w.t.Helper()
	file := path.Join(dir, "BUILD")
	if strings.TrimSpace(build) == "" {
		w.files[file] += ""
		return
	}
	w.files[file] += dedent(build)
}

// GivenIWorkIn makes later commands run in dir, within the project, "." for the project itself,
// as -C names it: unqualified names on the command line then refer to its package.
func (w *world) GivenIWorkIn(dir string) {
	w.t.Helper()
	if dir = path.Clean(dir); path.IsAbs(dir) || dir == ".." || strings.HasPrefix(dir, "../") {
		w.t.Fatalf("%s isn't within the project", dir)
	}
	if w.cDir != "" {
		w.t.Fatal("GivenTheCommandRunsIn has given -C already")
	}
	w.dir = dir
}

// GivenTheCommandRunsIn makes later commands name dir with -C as it is given, rather than a directory within the
// project, for a scenario of -C's mistakes: dir may be missing, a file, or relative to the working directory.
func (w *world) GivenTheCommandRunsIn(dir string) {
	w.t.Helper()
	if w.dir != "" && w.dir != "." {
		w.t.Fatal("GivenIWorkIn has given a directory within the project already")
	}
	w.cDir = dir
}

// GivenTheProjectHolds adds files to the project, by path, replacing any it held at those paths.
func (w *world) GivenTheProjectHolds(project files) {
	w.t.Helper()
	maps.Copy(w.files, project)
}

// GivenTheProjectModes gives the project's files and directories these modes, rather than 0o644 and 0o755.
func (w *world) GivenTheProjectModes(project modes) {
	w.t.Helper()
	maps.Copy(w.modes, project)
}

// edit is a change to the project file at path, relative to the project, to hold content,
// when a command opens it for the at'th time. Storing a new file opens it twice, to hash it and then to copy it.
type edit struct {
	path, content string
	at            int
}

// GivenTheFileChangesWhileTheBuildReadsIt changes the project file at path to hold content
// between each command's hashing it and copying it, as an editor might while a build runs.
func (w *world) GivenTheFileChangesWhileTheBuildReadsIt(path, content string) {
	w.t.Helper()
	w.edit = &edit{path, content, 2}
}

// GivenTheFileChangesOnceTheBuildHasReadIt changes the project file at path to hold content
// once each command has read it, as an editor might while a build runs: a third open would be a second read.
func (w *world) GivenTheFileChangesOnceTheBuildHasReadIt(path, content string) {
	w.t.Helper()
	w.edit = &edit{path, content, 3}
}

// GivenTheFileIsTouched sets the modification time of the project file at path an hour ahead, its content as it was.
func (w *world) GivenTheFileIsTouched(path string) {
	w.t.Helper()
	w.touched = append(w.touched, path)
}

// GivenNoFileChangesWhileTheBuildReadsIt undoes GivenTheFileChangesWhileTheBuildReadsIt
// and GivenTheFileChangesOnceTheBuildHasReadIt, for the commands that follow.
func (w *world) GivenNoFileChangesWhileTheBuildReadsIt() {
	w.t.Helper()
	w.edit = nil
}

// GivenTheProjectLinks adds symbolic links to the project, by path, to their targets,
// in which <outside> stands for the folder of GivenAFileOutsideTheProjectHolds.
func (w *world) GivenTheProjectLinks(project links) {
	w.t.Helper()
	maps.Copy(w.links, project)
}

// GivenTheEnvironmentSets sets the environment variable name of the vamos process, for the rest of the scenario.
func (w *world) GivenTheEnvironmentSets(name, value string) {
	w.t.Helper()
	w.t.Setenv(name, value)
}

// GivenTheEnvironmentDoesNotSet unsets the environment variable name of the vamos process, for the rest of the
// scenario.
func (w *world) GivenTheEnvironmentDoesNotSet(name string) {
	w.t.Helper()
	w.t.Setenv(name, "") // which restores it once the scenario ends
	if err := os.Unsetenv(name); err != nil {
		w.t.Fatal(err)
	}
}

// GivenAFileOutsideTheProjectHolds writes content to the file name, a path, in a folder outside the project,
// which <outside> stands for in the secrets of the withSecrets option and the targets of GivenTheProjectLinks.
func (w *world) GivenAFileOutsideTheProjectHolds(name, content string) {
	w.t.Helper()
	if w.outside == "" {
		w.outside = w.t.TempDir()
	}
	path := filepath.Join(w.outside, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		w.t.Fatal(err)
	}
}

// GivenTheProjectFileCannotBeChecked makes the project file at path, besides any that earlier calls named,
// one whose existence can't be checked, as when its directory isn't searchable,
// which permissions don't make it as the tests run as root.
func (w *world) GivenTheProjectFileCannotBeChecked(path string) {
	w.t.Helper()
	w.denied = append(w.denied, path)
}

// GivenARegistry starts a registry of the scenario's own, which <registry> stands for in image names and errors.
// It serves plain HTTP, and registries.conf marks it insecure, as the executor needs, which pushes and pulls
// with its settings; the client follows --tls-verify, if it is given, and otherwise registries.conf too.
func (w *world) GivenARegistry() {
	w.t.Helper()
	w.startRegistry(nil, true)
}

// GivenARegistryThatRegistriesConfDoesNotMarkInsecure starts a registry as GivenARegistry does,
// which serves plain HTTP, but which registries.conf doesn't mark insecure, so that the executor expects HTTPS.
func (w *world) GivenARegistryThatRegistriesConfDoesNotMarkInsecure() {
	w.t.Helper()
	w.startRegistry(nil, false)
}

// GivenARegistryThatNeedsALogin starts a registry as GivenARegistry does, which refuses any request that doesn't
// log in as user with password.
func (w *world) GivenARegistryThatNeedsALogin(user, password string) {
	w.t.Helper()
	reg := newRegistry()
	w.login = &authn.Basic{Username: user, Password: password}
	w.startRegistry(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != user || p != password {
			rw.Header().Set("WWW-Authenticate", `Basic realm="vamos"`)
			http.Error(rw, `{"errors":[{"code":"UNAUTHORIZED","message":"log in"}]}`, http.StatusUnauthorized)
			return
		}
		reg.ServeHTTP(rw, r)
	}), true)
}

// newRegistry returns an empty registry that logs nothing.
func newRegistry() http.Handler {
	return registry.New(registry.Logger(log.New(io.Discard, "", 0)))
}

// startRegistry serves handler, or an empty registry, as the scenario's registry, marked insecure if insecure.
func (w *world) startRegistry(handler http.Handler, insecure bool) {
	w.t.Helper()
	if w.registry != "" {
		w.t.Fatal("the scenario has a registry already")
	}
	if handler == nil {
		handler = newRegistry()
	}
	server := httptest.NewServer(handler)
	w.t.Cleanup(server.Close)
	w.registry = strings.TrimPrefix(server.URL, "http://")
	if insecure {
		testenv.InsecureRegistry(w.t, w.registry)
	}
}

// atRegistry returns s with each <registry> replaced by the host of the scenario's registry,
// failing the scenario if s names it but no Given step started one.
func (w *world) atRegistry(s string) string {
	w.t.Helper()
	if strings.Contains(s, "<registry>") && w.registry == "" {
		w.t.Fatalf("%q names <registry>, but no Given step started one", s)
	}
	return strings.ReplaceAll(s, "<registry>", w.registry)
}

// GivenTheAuthFileLogsIn makes REGISTRY_AUTH_FILE, for the rest of the scenario, a file that logs in to the
// scenario's registry as user with password.
func (w *world) GivenTheAuthFileLogsIn(user, password string) {
	w.t.Helper()
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
	file := filepath.Join(w.t.TempDir(), "auth.json")
	content := `{"auths": {"` + w.atRegistry("<registry>") + `": {"auth": "` + auth + `"}}}`
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		w.t.Fatal(err)
	}
	w.GivenTheEnvironmentSets("REGISTRY_AUTH_FILE", file)
}
