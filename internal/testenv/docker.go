package testenv

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// FakeDocker serves the parts of the Docker Engine API that loading an image uses, on a machine whose architecture
// uname names machine, such as "x86_64" or "armv7l", over plain HTTP on a "unix" socket or a "tcp" port,
// and points DOCKER_HOST at it, for the rest of the test.
// It answers loads with reply, a stream of JSON messages, or reports success if reply is empty.
// It returns a function that returns the tarballs loaded so far.
func FakeDocker(t *testing.T, network, reply, machine string) func() [][]byte {
	t.Helper()
	// The fake serves plain HTTP, whatever TLS the environment asks of a daemon.
	Unsetenv(t, "DOCKER_TLS_VERIFY")
	Unsetenv(t, "DOCKER_CERT_PATH")
	var listener net.Listener
	var err error
	switch network {
	case "unix":
		// Unix socket paths are limited to about 100 bytes, so avoid t.TempDir's long names.
		dir, mkErr := os.MkdirTemp("", "vamos-docker-")
		if mkErr != nil {
			t.Fatal(mkErr)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		sock := filepath.Join(dir, "docker.sock")
		listener, err = net.Listen("unix", sock)
		if err == nil {
			t.Setenv("DOCKER_HOST", "unix://"+sock)
		}
	case "tcp":
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			t.Setenv("DOCKER_HOST", "tcp://"+listener.Addr().String())
		}
	default:
		t.Fatalf("FakeDocker: unknown network %q", network)
	}
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var loaded [][]byte
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("API-Version", "1.45")
			io.WriteString(w, "OK")
		case strings.HasSuffix(r.URL.Path, "/info"):
			// The daemon runs on machine, as uname names it, whatever machine the tests run on.
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"OSType":"linux","Architecture":%q}`, machine)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images/load"):
			b, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			loaded = append(loaded, b)
			mu.Unlock()
			body := reply
			if body == "" {
				body = `{"stream":"Loaded image\n"}`
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, body)
		default:
			t.Errorf("fake Docker daemon got unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)

	return func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return loaded
	}
}
