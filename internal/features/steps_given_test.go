package features

import (
	"cmp"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/containerd/platforms"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
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

// GivenTheBuildFileChanges replaces the first old in the BUILD file with new, in both of which <registry> stands for
// the host of the scenario's registry.
// The BUILD text that it changes is the scenario's own, or a fixture declared in the same feature's file,
// so that a reader can find what it changes. It stops the scenario if the BUILD file doesn't hold old.
func (w *world) GivenTheBuildFileChanges(old, new string) {
	w.t.Helper()
	old, new = w.atRegistry(old), w.atRegistry(new)
	if !strings.Contains(w.files["BUILD"], old) {
		w.t.Fatalf("the BUILD file doesn't hold %q\n%s", old, w.files["BUILD"])
	}
	w.files["BUILD"] = strings.Replace(w.files["BUILD"], old, new, 1)
}

// GivenThePackageDeclares appends build, dedented, to the BUILD file of the package in dir, "." for the project's,
// in which <registry> stands for the host of the scenario's registry.
// A blank build appends nothing, though the package then has its BUILD file, empty if it had none.
func (w *world) GivenThePackageDeclares(dir, build string) {
	w.t.Helper()
	file := path.Join(dir, "BUILD")
	if strings.TrimSpace(build) == "" {
		w.files[file] += ""
		return
	}
	w.files[file] += dedent(w.atRegistry(build))
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
	testenv.Unsetenv(w.t, name)
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
	w.server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if w.stopped.Load() {
			// Dropped, as a registry that has gone drops it, while its port stays the scenario's.
			if conn, _, err := http.NewResponseController(rw).Hijack(); err == nil {
				conn.Close()
			}
			return
		}
		w.asked.Add(1)
		handler.ServeHTTP(rw, r)
	}))
	w.t.Cleanup(w.server.Close)
	w.registry = strings.TrimPrefix(w.server.URL, "http://")
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

// GivenADockerDaemon starts a Docker daemon of the scenario's own, on an amd64 machine, at a Unix socket that
// DOCKER_HOST names: a fake of the API that loading an image uses, which keeps what it loads, and accepts it.
func (w *world) GivenADockerDaemon() {
	w.t.Helper()
	w.startDocker("unix", "", "x86_64")
}

// GivenADockerDaemonListeningOn starts a Docker daemon as GivenADockerDaemon does, at a "unix" socket or a "tcp" port,
// as network says, that DOCKER_HOST names: over TCP, plain HTTP, as Docker serves one without $DOCKER_CERT_PATH.
func (w *world) GivenADockerDaemonListeningOn(network string) {
	w.t.Helper()
	w.startDocker(network, "", "x86_64")
}

// GivenADockerDaemonOn starts a Docker daemon as GivenADockerDaemon does, on a machine that uname names machine,
// such as armv7l.
func (w *world) GivenADockerDaemonOn(machine string) {
	w.t.Helper()
	w.startDocker("unix", "", machine)
}

// GivenADockerDaemonThatRefusesLoads starts a Docker daemon as GivenADockerDaemon does, which refuses each load,
// saying why, as a daemon whose disk is full does.
func (w *world) GivenADockerDaemonThatRefusesLoads(why string) {
	w.t.Helper()
	w.startDocker("unix", fmt.Sprintf(`{"errorDetail":{"message":%q},"error":%q}`, why, why), "x86_64")
}

// startDocker starts the scenario's fake Docker daemon, on network, which answers loads with reply, on machine.
func (w *world) startDocker(network, reply, machine string) {
	w.t.Helper()
	w.givesTheDaemon("fake")
	w.docker = testenv.FakeDocker(w.t, network, reply, machine)
}

// givesTheDaemon records which Docker daemon the scenario loads into, failing it if a Given step has given one.
func (w *world) givesTheDaemon(daemon string) {
	w.t.Helper()
	if w.daemon != "" {
		w.t.Fatalf("a Given step gave the %s Docker daemon already", w.daemon)
	}
	w.daemon = daemon
}

// GivenNoDockerDaemon makes DOCKER_HOST, for the rest of the scenario, a socket that nothing listens at,
// /nonexistent/docker.sock, without TLS, which the environment might otherwise ask of it.
func (w *world) GivenNoDockerDaemon() {
	w.t.Helper()
	w.givesTheDaemon("none")
	w.GivenTheEnvironmentSets("DOCKER_HOST", "unix:///nonexistent/docker.sock")
	w.GivenTheEnvironmentDoesNotSet("DOCKER_TLS_VERIFY")
	w.GivenTheEnvironmentDoesNotSet("DOCKER_CERT_PATH")
}

// GivenTheHostsDockerDaemon makes the scenario load into the host's own Docker daemon, at $DOCKER_HOST or
// /var/run/docker.sock, as vamos finds it, and skips the scenario without one, even where VAMOS_REQUIRE_INTEGRATION
// is set: a host that runs the whole suite needs buildah, but not Docker.
func (w *world) GivenTheHostsDockerDaemon() {
	w.t.Helper()
	w.givesTheDaemon("host")
	w.dockerHost = cmp.Or(os.Getenv("DOCKER_HOST"), "unix:///var/run/docker.sock")
	if err := exec.Command("docker", "--host", w.dockerHost, "info").Run(); err != nil {
		w.t.Skipf("needs a Docker daemon at %s: %v", w.dockerHost, err)
	}
}

// GivenTheRegistryHoldsAnIndex pushes to the image name image, in which <registry> stands for the scenario's registry,
// an index of an image of random files for each of platforms, which label names, with the platform,
// for ThenTheImageIsBuiltOn: a tag that names another index already moves.
func (w *world) GivenTheRegistryHoldsAnIndex(image, label string, platformNames ...string) {
	w.t.Helper()
	var index v1.ImageIndex = empty.Index
	for _, platform := range platformNames {
		img := w.heldImage(label, platform)
		p, err := platforms.Parse(platform)
		if err != nil {
			w.t.Fatal(err)
		}
		index = mutate.AppendManifests(index, mutate.IndexAddendum{
			Add: img,
			Descriptor: v1.Descriptor{
				Platform: &v1.Platform{OS: p.OS, Architecture: p.Architecture, Variant: p.Variant},
			},
		})
	}
	if err := remote.WriteIndex(w.registryRef(image), index, w.readingTheRegistry()...); err != nil {
		w.t.Fatal(err)
	}
}

// GivenTheRegistryHoldsAnImage pushes to the image name image, in which <registry> stands for the scenario's
// registry, an image of random files for platform alone, rather than an index, which label names, with the platform.
func (w *world) GivenTheRegistryHoldsAnImage(image, label, platform string) {
	w.t.Helper()
	if err := remote.Write(w.registryRef(image), w.heldImage(label, platform), w.readingTheRegistry()...); err != nil {
		w.t.Fatal(err)
	}
}

// heldImage returns a new image of random files for platform, which label names, with the platform.
func (w *world) heldImage(label, platform string) v1.Image {
	w.t.Helper()
	img, err := random.Image(256, 1)
	if err != nil {
		w.t.Fatal(err)
	}
	config, err := img.ConfigFile()
	if err != nil {
		w.t.Fatal(err)
	}
	p, err := platforms.Parse(platform)
	if err != nil {
		w.t.Fatal(err)
	}
	config.OS, config.Architecture, config.Variant = p.OS, p.Architecture, p.Variant
	if img, err = mutate.ConfigFile(img, config); err != nil {
		w.t.Fatal(err)
	}
	if w.held == nil {
		w.held = map[string]v1.Image{}
	}
	key := w.heldKey(label, platform)
	if _, ok := w.held[key]; ok {
		w.t.Fatalf("the registry holds %s already", key)
	}
	w.held[key] = img
	return img
}

// heldKey returns the key of the image that label names for platform, normalized, among those that Given steps
// put in the registry.
func (w *world) heldKey(label, platform string) string {
	w.t.Helper()
	p, err := platforms.Parse(platform)
	if err != nil {
		w.t.Fatal(err)
	}
	return label + " for " + platformName(p.OS, p.Architecture, p.Variant)
}

// registryRef returns the image name image, in which <registry> stands for the scenario's registry, as a reference.
func (w *world) registryRef(image string) name.Reference {
	w.t.Helper()
	ref, err := name.ParseReference(w.atRegistry(image), name.Insecure)
	if err != nil {
		w.t.Fatal(err)
	}
	return ref
}

// GivenTheRegistryStops stops the scenario's registry answering, for the rest of the scenario: it drops each
// connection, keeping its port, so that no other server can take it.
func (w *world) GivenTheRegistryStops() {
	w.t.Helper()
	if w.server == nil {
		w.t.Fatal("no Given step started a registry")
	}
	w.stopped.Store(true)
}

// GivenStoresOfTheScenariosOwn gives the scenario a container store and build cache of its own, for the rest of the
// scenario, rather than the suite's, for a scenario that destroys what they hold, as prune does.
// It stops the scenario if a command has run already, on the suite's stores.
func (w *world) GivenStoresOfTheScenariosOwn() {
	w.t.Helper()
	if w.ran {
		w.t.Fatal("a command has run already, on the suite's stores")
	}
	w.stores = testenv.PrivateStores(w.t)
	if w.before != nil {
		before, err := w.stores.UnnamedImages()
		if err != nil {
			w.t.Fatal(err)
		}
		w.before = before
	}
}
