package worker

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/containerd/platforms"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// binfmtDir is where the kernel lists the binfmt_misc handlers registered, one file each.
const binfmtDir = "/proc/sys/fs/binfmt_misc"

// qemuArchitectures maps the architecture in a qemu interpreter's name, qemu-<arch>, to the platform it emulates.
var qemuArchitectures = map[string]imgspecv1.Platform{
	"aarch64":     {OS: "linux", Architecture: "arm64"},
	"x86_64":      {OS: "linux", Architecture: "amd64"},
	"i386":        {OS: "linux", Architecture: "386"},
	"arm":         {OS: "linux", Architecture: "arm", Variant: "v7"},
	"riscv64":     {OS: "linux", Architecture: "riscv64"},
	"ppc64le":     {OS: "linux", Architecture: "ppc64le"},
	"s390x":       {OS: "linux", Architecture: "s390x"},
	"mips64el":    {OS: "linux", Architecture: "mips64le"},
	"loongarch64": {OS: "linux", Architecture: "loong64"},
}

// emulatedPlatforms returns the platforms, other than host, whose programs this machine runs under emulation:
// those with an enabled binfmt_misc handler in dir whose interpreter is a qemu user-mode emulator, as
// tonistiigi/binfmt registers them. Without the directory, or handlers, there are none.
func emulatedPlatforms(dir string, host imgspecv1.Platform) []imgspecv1.Platform {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	hostName := platforms.Format(platforms.Normalize(host))
	var emulated []imgspecv1.Platform
	for _, e := range entries {
		if e.IsDir() || e.Name() == "register" || e.Name() == "status" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		p, ok := handlerPlatform(data)
		if !ok || platforms.Format(platforms.Normalize(p)) == hostName {
			continue
		}
		if !slices.ContainsFunc(emulated, func(have imgspecv1.Platform) bool {
			return platforms.Format(platforms.Normalize(have)) == platforms.Format(platforms.Normalize(p))
		}) {
			emulated = append(emulated, p)
		}
	}
	slices.SortFunc(emulated, func(a, b imgspecv1.Platform) int {
		return strings.Compare(platforms.Format(a), platforms.Format(b))
	})
	return emulated
}

// handlerPlatform reads one binfmt_misc handler's description, data, and returns the platform its interpreter
// emulates, if the handler is enabled and the interpreter is a qemu emulator this package knows.
func handlerPlatform(data []byte) (imgspecv1.Platform, bool) {
	enabled := false
	var interpreter string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "enabled":
			enabled = true
		case strings.HasPrefix(line, "interpreter "):
			interpreter = strings.TrimPrefix(line, "interpreter ")
		}
	}
	if !enabled || interpreter == "" {
		return imgspecv1.Platform{}, false
	}
	name := filepath.Base(interpreter)
	arch, ok := strings.CutPrefix(name, "qemu-")
	if !ok {
		return imgspecv1.Platform{}, false
	}
	arch = strings.TrimSuffix(arch, "-static")
	p, known := qemuArchitectures[arch]
	return p, known
}
