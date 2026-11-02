package features

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
	"github.com/containerd/platforms"
	"github.com/docker/distribution/registry/api/errcode"
	v2 "github.com/docker/distribution/registry/api/v2"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/opencontainers/go-digest"
	"go.podman.io/image/v5/docker/reference"
)

// Given steps set up a scenario's project.

// GivenStandardOutputIsClosed makes the next command's standard output fail every write, as a pipe does once
// what reads it has gone, such as head once it has read its lines: with the error that errOutputClosed is.
// What the command tried to print there is no part of its output, though steps that check it printed no usage, or
// reported nothing sent, see it.
func (w *world) GivenStandardOutputIsClosed() {
	w.t.Helper()
	w.outputClosed = true
}

// errOutputClosed is what writing to standard output fails with, after GivenStandardOutputIsClosed.
var errOutputClosed = errors.New("standard output is closed")

// closedOutput is standard output after GivenStandardOutputIsClosed, which fails every write, noting what it was
// given in tried.
type closedOutput struct{ tried *bytes.Buffer }

func (c closedOutput) Write(p []byte) (int, error) {
	c.tried.Write(p)
	return 0, errOutputClosed
}

// GivenAProjectThatPullsBusybox starts the BUILD file with the pull "busybox", of busybox:1.36,
// the one BUILD text that a step writes rather than the scenario. It stops the scenario if the BUILD file holds
// anything already. Unless the scenario gives a lockfile, the project then holds one that pins busybox,
// testenv.BusyboxLock, as testenv.Project adds it, so that no build asks Docker Hub about its tags.
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
// the host of the scenario's registry, and <outside> for the folder of GivenAFileOutsideTheProjectHolds.
// The BUILD text that it changes is the scenario's own, or a fixture declared in the same feature's file,
// so that a reader can find what it changes. It stops the scenario if the BUILD file doesn't hold old.
func (w *world) GivenTheBuildFileChanges(old, new string) {
	w.t.Helper()
	old, new = w.atOutside(w.atRegistry(old)), w.atOutside(w.atRegistry(new))
	if !strings.Contains(w.files["BUILD"], old) {
		w.t.Fatalf("the BUILD file doesn't hold %q\n%s", old, w.files["BUILD"])
	}
	w.files["BUILD"] = strings.Replace(w.files["BUILD"], old, new, 1)
}

// GivenThePackageDeclares appends build, dedented, to the BUILD file of the package in dir, "." for the project's,
// in which <registry> stands for the host of the scenario's registry, and <outside> for the folder of
// GivenAFileOutsideTheProjectHolds, which comes first.
// A blank build appends nothing, though the package then has its BUILD file, empty if it had none.
func (w *world) GivenThePackageDeclares(dir, build string) {
	w.t.Helper()
	file := path.Join(dir, "BUILD")
	if strings.TrimSpace(build) == "" {
		w.files[file] += ""
		return
	}
	w.files[file] += dedent(w.atOutside(w.atRegistry(build)))
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

// GivenThisMachineEmulates says that the scenario runs platform's programs, another's than this machine's,
// such as linux/amd64's on an arm64 machine, under an emulator, without which, as "exec format error" says,
// ThenItSucceeds skips the scenario:
// whether the kernel has one shows only as a command runs.
func (w *world) GivenThisMachineEmulates(platform string) {
	w.t.Helper()
	if platform == thisPlatform() {
		w.t.Fatalf("%s is this machine's platform, whose programs it runs itself", platform)
	}
	w.emulating = platform
}

// GivenTheProjectHolds adds files to the project, by path, replacing any it held at those paths.
func (w *world) GivenTheProjectHolds(project files) {
	w.t.Helper()
	maps.Copy(w.files, project)
}

// GivenTheLockfileHolds makes the project's vamos.lock, beside its BUILD file, or its WORKSPACE file if it has one,
// hold lockfile, dedented, in which <registry> stands for the host of the scenario's registry, and <digest of label>
// for the digest of the index, or image, that label names, which a Given step put there.
func (w *world) GivenTheLockfileHolds(lockfile string) {
	w.t.Helper()
	w.files["vamos.lock"] = dedent(w.atHeldDigests(w.atRegistry(lockfile)))
}

// GivenAnotherCommandPinsWhileTheTagsAreListed makes another command write lockfile, in which <registry> and
// <digest of label> stand as GivenTheLockfileHolds says, as the scenario's registry first lists tags for each command
// that follows: as one that pins a pull while that command resolves it would.
func (w *world) GivenAnotherCommandPinsWhileTheTagsAreListed(lockfile string) {
	w.t.Helper()
	if w.server == nil {
		w.t.Fatal("GivenAnotherCommandPinsWhileTheTagsAreListed needs the scenario's registry, which GivenARegistry starts")
	}
	w.pinnedMeanwhile = dedent(w.atHeldDigests(w.atRegistry(lockfile)))
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

// GivenFilesOutsideTheProjectHold writes each of written, by path, as GivenAFileOutsideTheProjectHolds does,
// and nothing for none.
func (w *world) GivenFilesOutsideTheProjectHold(written files) {
	w.t.Helper()
	for _, name := range slices.Sorted(maps.Keys(written)) {
		w.GivenAFileOutsideTheProjectHolds(name, written[name])
	}
}

// GivenTheEnvironmentSets sets the environment variable name of the vamos process, for the rest of the scenario,
// to value, in which <coordinator> stands for the URL of the scenario's coordinator.
func (w *world) GivenTheEnvironmentSets(name, value string) {
	w.t.Helper()
	w.t.Setenv(name, w.atCoordinator(value))
}

// GivenTheEnvironmentDoesNotSet unsets the environment variable name of the vamos process, for the rest of the
// scenario.
func (w *world) GivenTheEnvironmentDoesNotSet(name string) {
	w.t.Helper()
	testenv.Unsetenv(w.t, name)
}

// GivenAFileOutsideTheProjectHolds writes content to the file name, a path, in a folder outside the project,
// at an old time, which <outside> stands for in the secrets of the withSecrets option, the targets of links that
// Given and Then steps take, and BUILD text, which a scenario declares after this step.
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
	// An old time, which a command that touched the file would change.
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		w.t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		w.t.Fatal(err)
	}
	if w.outsideWas == nil {
		w.outsideWas = map[string]outsideFile{}
	}
	w.outsideWas[name] = outsideFile{content, info}
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
		if repo, ok := tagsList(r); ok {
			w.refusedMu.Lock()
			answer, refused := w.unlisting[repo]
			page, meanwhile := w.tagPage, w.pinMeanwhile
			w.refusedMu.Unlock()
			if meanwhile != nil {
				meanwhile()
			}
			if refused {
				if strings.HasPrefix(answer.body, "{") {
					rw.Header().Set("Content-Type", "application/json")
				}
				rw.WriteHeader(answer.status)
				rw.Write([]byte(answer.body))
				return
			}
			if page > 0 {
				serveTagPage(rw, r, handler, repo, page)
				return
			}
		}
		if w.refuses(r) {
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(http.StatusForbidden)
			rw.Write([]byte(`{"errors": [{"code": "DENIED", "message": "writes refused"}]}`))
			return
		}
		handler.ServeHTTP(rw, r)
	}))
	w.t.Cleanup(w.server.Close)
	w.registry = strings.TrimPrefix(w.server.URL, "http://")
	if insecure {
		testenv.InsecureRegistry(w.t, w.registry)
	}
}

// GivenTheRegistryRefusesWritesTo makes the scenario's registry refuse every manifest written to each of repositories,
// "team/app" for <registry>/team/app: one uploaded by digest, and a tag; it takes their blobs.
func (w *world) GivenTheRegistryRefusesWritesTo(repositories ...string) {
	w.t.Helper()
	w.refuse(repositories, false)
}

// GivenTheRegistryRefusesTagsIn makes the scenario's registry refuse every tag written to each of repositories,
// while it takes manifests uploaded there by digest, as a registry that starts refusing writes after an upload would.
func (w *world) GivenTheRegistryRefusesTagsIn(repositories ...string) {
	w.t.Helper()
	w.refuse(repositories, true)
}

// GivenTheRegistryAcceptsWritesAgain makes the scenario's registry accept what it refused.
func (w *world) GivenTheRegistryAcceptsWritesAgain() {
	w.t.Helper()
	w.refusedMu.Lock()
	defer w.refusedMu.Unlock()
	w.refused = nil
}

// refuse makes the scenario's registry refuse what is written to repositories: tags alone, if tagsOnly.
func (w *world) refuse(repositories []string, tagsOnly bool) {
	w.t.Helper()
	if w.registry == "" {
		w.t.Fatal("no Given step started a registry to refuse writes")
	}
	w.refusedMu.Lock()
	defer w.refusedMu.Unlock()
	if w.refused == nil {
		w.refused = map[string]bool{}
	}
	for _, repo := range repositories {
		w.refused[repo] = tagsOnly
	}
}

// refuses reports whether the scenario's registry refuses r, a write of a manifest.
func (w *world) refuses(r *http.Request) bool {
	if r.Method != http.MethodPut {
		return false
	}
	repo, reference, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v2/"), "/manifests/")
	if !ok {
		return false
	}
	w.refusedMu.Lock()
	defer w.refusedMu.Unlock()
	tagsOnly, refused := w.refused[repo]
	return refused && (!tagsOnly || !strings.HasPrefix(reference, "sha256:"))
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
		index = mutate.AppendManifests(index, mutate.IndexAddendum{
			Add:        w.heldImage(label, platform),
			Descriptor: v1.Descriptor{Platform: w.parsedPlatform(platform)},
		})
	}
	if err := remote.WriteIndex(w.registryRef(image), index, w.readingTheRegistry()...); err != nil {
		w.t.Fatal(err)
	}
	w.noteHeldDigest(label, index.Digest)
}

// noteHeldDigest notes the digest of the manifest that label names, an index or an image that a Given step put in
// the registry, as noteDigest does.
func (w *world) noteHeldDigest(label string, digest func() (v1.Hash, error)) {
	w.t.Helper()
	d, err := digest()
	if err != nil {
		w.t.Fatal(err)
	}
	w.noteDigest(label, d.String())
}

// noteDigest notes that label names the manifest whose digest is d, for <digest of label> to stand for.
func (w *world) noteDigest(label, d string) {
	if w.heldDigest == nil {
		w.heldDigest = map[string]string{}
	}
	w.heldDigest[label] = d
}

// atHeldDigests returns s with each <digest of label> replaced by the digest of the index, or image, that label names,
// which a Given step put in the registry, failing the scenario if it put none there.
func (w *world) atHeldDigests(s string) string {
	w.t.Helper()
	return heldDigestPlaceholder.ReplaceAllStringFunc(s, func(placeholder string) string {
		label := heldDigestPlaceholder.FindStringSubmatch(placeholder)[1]
		d, ok := w.heldDigest[label]
		if !ok {
			w.t.Fatalf("%q names %s, but no Given step put it in the registry", s, placeholder)
		}
		return d
	})
}

// heldDigestPlaceholder is <digest of label>, which atHeldDigests replaces.
var heldDigestPlaceholder = regexp.MustCompile(`<digest of ([^>]+)>`)

// GivenTheRegistryHoldsAnImage pushes to the image name image, in which <registry> stands for the scenario's
// registry, an image of random files for platform alone, rather than an index, which label names, with the platform.
func (w *world) GivenTheRegistryHoldsAnImage(image, label, platform string) {
	w.t.Helper()
	img := w.heldImage(label, platform)
	if err := remote.Write(w.registryRef(image), img, w.readingTheRegistry()...); err != nil {
		w.t.Fatal(err)
	}
	w.noteHeldDigest(label, img.Digest)
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

// parsedPlatform returns platform, as os/arch[/variant] names it, failing the scenario if it isn't one.
func (w *world) parsedPlatform(platform string) *v1.Platform {
	w.t.Helper()
	p, err := platforms.Parse(platform)
	if err != nil {
		w.t.Fatal(err)
	}
	return &v1.Platform{OS: p.OS, Architecture: p.Architecture, Variant: p.Variant}
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
	w.stores, w.ownStores = testenv.PrivateStores(w.t), true
	if w.before != nil { // as GivenBuildahCanBuildHere noted the suite's
		w.noteWhatTheStoresHold()
	}
}

// GivenTheProjectIsReachedThroughALink keeps the project on disk in a directory, project-real beside it, that
// /project, the directory that commands run in, is a symbolic link to, as a shell's $PWD can be.
func (w *world) GivenTheProjectIsReachedThroughALink() {
	w.t.Helper()
	w.linked = true
}

// GivenTheRegistrysImageRunsAs replaces the image at the image name image, in which <registry> stands for the
// scenario's registry, with one that runs as user, such as "1000:1000", rather than as it did.
// If the name names an index, the new image is this machine's image from it, and the name names it alone.
func (w *world) GivenTheRegistrysImageRunsAs(image, user string) {
	w.t.Helper()
	ref := w.registryRef(image)
	img, err := remote.Image(ref, append(w.readingTheRegistry(),
		remote.WithPlatform(*w.parsedPlatform(thisPlatform())))...)
	if err != nil {
		w.t.Fatal(err)
	}
	config, err := img.ConfigFile()
	if err != nil {
		w.t.Fatal(err)
	}
	config.Config.User = user
	if img, err = mutate.ConfigFile(img, config); err != nil {
		w.t.Fatal(err)
	}
	if err := remote.Write(ref, img, w.readingTheRegistry()...); err != nil {
		w.t.Fatal(err)
	}
}

// fakeRegistryHost is the host of the fake registry that GivenTheRegistryListsTags starts, which <registry> stands for
// then, so that a scenario that pins pulls needs no registry of its own.
const fakeRegistryHost = "registry.example"

// fakeRegistry is the cli.Registry that GivenTheRegistryListsTags gives commands, in place of the image's own
// registry: it lists the tags that steps give each repository, and says that each names a digest of its own, as
// <digest of image:tag> stands for, or the one GivenTheTagNames gave it.
type fakeRegistry struct {
	mu      sync.Mutex
	tags    map[string][]string      // the tags of each repository, by name
	refused map[string]bool          // the repositories whose tags it won't list
	names   map[string]digest.Digest // what a tag names, by image and tag, where GivenTheTagNames said
}

// Tags lists repo's tags, or fails as a registry that won't list them, or doesn't know repo, does.
func (r *fakeRegistry) Tags(_ context.Context, repo reference.Named) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refused[repo.Name()] {
		return nil, fmt.Errorf("fetching tags list: %w", errcode.ErrorCodeDenied.WithMessage("listing tags refused"))
	}
	tags, ok := r.tags[repo.Name()]
	if !ok {
		return nil, fmt.Errorf("fetching tags list: repository name not known to registry")
	}
	return slices.Clone(tags), nil
}

// Digest returns what the tag ref names, or fails as a registry without it does.
func (r *fakeRegistry) Digest(_ context.Context, ref reference.NamedTagged) (digest.Digest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Contains(r.tags[ref.Name()], ref.Tag()) {
		return "", fmt.Errorf("reading manifest %s in %s: %w", ref.Tag(), ref.Name(), v2.ErrorCodeManifestUnknown)
	}
	if d, ok := r.names[ref.Name()+":"+ref.Tag()]; ok {
		return d, nil
	}
	return fakeDigest(ref.Name() + ":" + ref.Tag()), nil
}

// fakeDigest is the digest of the manifest that the fake registry says image, a name and tag, names.
func fakeDigest(image string) digest.Digest {
	return digest.FromString(image)
}

// GivenTheRegistryListsTags gives commands a fake registry, in place of each image's own, whose repository repo, in
// which <registry> stands for its host, lists tags, besides those it listed already, each naming a manifest of its
// own, whose digest <digest of repo:tag> stands for, with repo as the scenario writes it or normalized.
// The fake answers what pinning pulls asks about tags; images can't be pulled from it.
func (w *world) GivenTheRegistryListsTags(repo string, tags ...string) {
	w.t.Helper()
	r := w.theFakeRegistry()
	name := w.repositoryName(repo)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tags[name]; !ok {
		r.tags[name] = []string{} // known, even with no tags
	}
	for _, tag := range tags {
		if !slices.Contains(r.tags[name], tag) {
			r.tags[name] = append(r.tags[name], tag)
		}
		d := fakeDigest(name + ":" + tag).String()
		w.noteDigest(name+":"+tag, d)
		w.noteDigest(w.atRegistry(repo)+":"+tag, d)
	}
}

// GivenTheTagNames makes the fake registry of GivenTheRegistryListsTags list image's tag, if it doesn't, and say that
// it names a manifest that label names, whose digest <digest of label> stands for, as a tag moved to it would.
// In image, <registry> stands for the fake's host.
func (w *world) GivenTheTagNames(image, label string) {
	w.t.Helper()
	r := w.theFakeRegistry()
	named, err := reference.ParseNormalizedNamed(w.atRegistry(image))
	if err != nil {
		w.t.Fatalf("image %q: %v", image, err)
	}
	tagged, ok := named.(reference.NamedTagged)
	if !ok {
		w.t.Fatalf("image %q has no tag", image)
	}
	w.GivenTheRegistryListsTags(tagged.Name(), tagged.Tag())
	r.mu.Lock()
	defer r.mu.Unlock()
	d := fakeDigest(label)
	r.names[tagged.Name()+":"+tagged.Tag()] = d
	w.noteDigest(label, d.String())
}

// GivenTheRegistryRefusesToListTags makes the scenario's registry, of its own or the fake of
// GivenTheRegistryListsTags, refuse to list the tags of repo, in which <registry> stands for its host, as a registry
// that forbids listing does, while it still says what each tag names.
func (w *world) GivenTheRegistryRefusesToListTags(repo string) {
	w.t.Helper()
	if w.fakeRegistry == nil && w.server != nil {
		named, err := reference.ParseNormalizedNamed(w.atRegistry(repo))
		if err != nil {
			w.t.Fatalf("repository %q: %v", repo, err)
		}
		w.answerTagLists(reference.Path(named), http.StatusForbidden,
			`{"errors": [{"code": "DENIED", "message": "listing tags refused"}]}`)
		return
	}
	r := w.theFakeRegistry()
	r.mu.Lock()
	defer r.mu.Unlock()
	name := w.repositoryName(repo)
	r.refused[name] = true
	if _, ok := r.tags[name]; !ok {
		r.tags[name] = nil // known, as the registry knows it
	}
}

// theFakeRegistry returns the fake registry that commands are given, starting it if no step has, as the scenario's
// registry, which <registry> stands for; it stops the scenario if a registry of its own was started instead.
func (w *world) theFakeRegistry() *fakeRegistry {
	w.t.Helper()
	if w.fakeRegistry != nil {
		return w.fakeRegistry
	}
	if w.registry != "" {
		w.t.Fatal("the scenario has a registry of its own, which commands ask about tags, rather than a fake one")
	}
	w.registry = fakeRegistryHost
	w.fakeRegistry = &fakeRegistry{tags: map[string][]string{}, refused: map[string]bool{},
		names: map[string]digest.Digest{}}
	return w.fakeRegistry
}

// repositoryName returns repo, in which <registry> stands for the scenario's registry's host, as a normalized
// repository name, stopping the scenario if it isn't one, or has a tag or digest.
func (w *world) repositoryName(repo string) string {
	w.t.Helper()
	named, err := reference.ParseNormalizedNamed(w.atRegistry(repo))
	if err != nil {
		w.t.Fatalf("repository %q: %v", repo, err)
	}
	if !reference.IsNameOnly(named) {
		w.t.Fatalf("repository %q has a tag or digest", repo)
	}
	return named.Name()
}

// GivenTheRegistryListsTagsInPagesOf makes the scenario's registry of its own list a repository's tags n at a time,
// in the order it holds them, each page linking to the next, as registries with many tags do.
func (w *world) GivenTheRegistryListsTagsInPagesOf(n int) {
	w.t.Helper()
	if w.server == nil || n < 1 {
		w.t.Fatal("no Given step started a registry of the scenario's own to list tags in pages, or no page size")
	}
	w.refusedMu.Lock()
	defer w.refusedMu.Unlock()
	w.tagPage = n
}

// tagsList returns the repository whose tags r lists, if r lists tags.
func tagsList(r *http.Request) (string, bool) {
	if r.Method != http.MethodGet {
		return "", false
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/v2/")
	if !ok {
		return "", false
	}
	return strings.CutSuffix(path, "/tags/list")
}

// serveTagPage serves the page of repo's tags, as registry handler lists them, that r asks for, page at a time:
// those after r's last, linking to the next page if there are more.
func serveTagPage(rw http.ResponseWriter, r *http.Request, handler http.Handler, repo string, page int) {
	all := httptest.NewRecorder()
	whole := r.Clone(r.Context()) // with its credentials, for a registry that needs them
	whole.URL.RawQuery, whole.RequestURI = "", ""
	handler.ServeHTTP(all, whole)
	var list struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if all.Code != http.StatusOK || json.Unmarshal(all.Body.Bytes(), &list) != nil {
		maps.Copy(rw.Header(), all.Header())
		rw.WriteHeader(all.Code)
		rw.Write(all.Body.Bytes())
		return
	}
	tags := list.Tags
	if last := r.URL.Query().Get("last"); last != "" {
		if i := slices.Index(tags, last); i >= 0 {
			tags = tags[i+1:]
		} else {
			tags = nil // past the end, rather than back at the start
		}
	}
	if len(tags) > page {
		rw.Header().Set("Link", fmt.Sprintf(`</v2/%s/tags/list?n=%d&last=%s>; rel="next"`, repo, page,
			url.QueryEscape(tags[page-1])))
		tags = tags[:page]
	}
	rw.Header().Set("Content-Type", "application/json")
	data, _ := json.Marshal(map[string]any{"name": list.Name, "tags": tags})
	rw.Write(data)
}

// GivenTheRegistryAlsoTags tags what image names in the scenario's registry, in which <registry> stands for its host,
// with each of tags too, in its repository.
func (w *world) GivenTheRegistryAlsoTags(image string, tags ...string) {
	w.t.Helper()
	ref := w.registryRef(image)
	desc, err := remote.Get(ref, w.readingTheRegistry()...)
	if err != nil {
		w.t.Fatal(err)
	}
	for _, tag := range tags {
		if err := remote.Tag(ref.Context().Tag(tag), desc, w.readingTheRegistry()...); err != nil {
			w.t.Fatal(err)
		}
	}
}

// GivenAPullThroughCacheMirrorsTheRegistryWithAnIndex starts a registry of its own that registries.conf makes a
// mirror of the scenario's registry, as a pull-through cache is, which holds at image, in which <registry> stands for
// the scenario's registry's host, an index, as GivenTheRegistryHoldsAnIndex makes one, which label names, though
// the scenario's registry may hold another there.
func (w *world) GivenAPullThroughCacheMirrorsTheRegistryWithAnIndex(image, label string, platformNames ...string) {
	w.t.Helper()
	if w.server == nil {
		w.t.Fatal("no Given step started a registry for the cache to mirror")
	}
	served := newRegistry()
	cache := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.cacheAsked.Add(1)
		served.ServeHTTP(rw, r)
	}))
	w.t.Cleanup(cache.Close)
	host := strings.TrimPrefix(cache.URL, "http://")
	testenv.MirroredRegistry(w.t, w.registry, host)
	var index v1.ImageIndex = empty.Index
	for _, platform := range platformNames {
		index = mutate.AppendManifests(index, mutate.IndexAddendum{
			Add:        w.heldImage(label, platform),
			Descriptor: v1.Descriptor{Platform: w.parsedPlatform(platform)},
		})
	}
	ref, err := name.ParseReference(strings.Replace(w.atRegistry(image), w.registry, host, 1), name.Insecure)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := remote.WriteIndex(ref, index, w.readingTheRegistry()...); err != nil {
		w.t.Fatal(err)
	}
	w.noteHeldDigest(label, index.Digest)
	w.cacheAsked.Store(0) // what writing it asked
}

// GivenTheRegistryAnswersTagListsOf makes the scenario's registry of its own answer every request to list the tags of
// repo, in which <registry> stands for its host, with status and body, as a registry or a proxy in front of one that
// refuses listing, or fails, may.
func (w *world) GivenTheRegistryAnswersTagListsOf(repo string, status int, body string) {
	w.t.Helper()
	if w.server == nil {
		w.t.Fatal("no Given step started a registry of the scenario's own")
	}
	named, err := reference.ParseNormalizedNamed(w.atRegistry(repo))
	if err != nil {
		w.t.Fatalf("repository %q: %v", repo, err)
	}
	w.answerTagLists(reference.Path(named), status, body)
}

// answerTagLists makes the scenario's registry answer lists of the tags of the repository at path with status and
// body.
func (w *world) answerTagLists(path string, status int, body string) {
	w.refusedMu.Lock()
	defer w.refusedMu.Unlock()
	if w.unlisting == nil {
		w.unlisting = map[string]tagListAnswer{}
	}
	w.unlisting[path] = tagListAnswer{status, body}
}

// tagListAnswer is how the scenario's registry answers a list of a repository's tags, in place of the list.
type tagListAnswer struct {
	status int
	body   string
}
