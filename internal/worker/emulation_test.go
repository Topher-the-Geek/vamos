package worker

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// TestEmulatedPlatforms checks the reading of binfmt_misc handlers: enabled qemu handlers name platforms, the
// host's own and disabled or unknown handlers don't, and a missing directory means no emulation.
func TestEmulatedPlatforms(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("qemu-aarch64", "enabled\ninterpreter /usr/bin/qemu-aarch64\nflags: POCF\noffset 0\nmagic 7f454c46\n")
	write("qemu-riscv64", "enabled\ninterpreter /usr/bin/qemu-riscv64-static\nflags: F\n")
	write("qemu-s390x", "disabled\ninterpreter /usr/bin/qemu-s390x\n")
	write("qemu-x86_64", "enabled\ninterpreter /usr/bin/qemu-x86_64\n") // the host's own
	write("wine", "enabled\ninterpreter /usr/bin/wine\n")
	write("register", "")
	write("status", "enabled\n")
	host := imgspecv1.Platform{OS: "linux", Architecture: "amd64"}
	got := emulatedPlatforms(dir, host)
	want := []imgspecv1.Platform{
		{OS: "linux", Architecture: "arm64"},
		{OS: "linux", Architecture: "riscv64"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("emulatedPlatforms = %v, want %v", got, want)
	}
	if got := emulatedPlatforms(filepath.Join(dir, "missing"), host); got != nil {
		t.Errorf("emulatedPlatforms of a missing directory = %v, want none", got)
	}
}
