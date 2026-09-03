// Package buildinfo exposes a single human-readable version string for every
// binary in this module. It is set at link time by the Makefile / Dockerfile
// (`-X .../internal/buildinfo.Version=$(git describe ...)`) and falls back to
// the VCS revision that `go build` stamps into the binary, then to "dev".
package buildinfo

import (
	"runtime/debug"
	"sync"
)

// Version, when non-empty, is injected via -ldflags at build time.
var Version string

var (
	once     sync.Once
	resolved string
)

// Resolve returns the best available version string. It is stable for the life
// of the process.
func Resolve() string {
	once.Do(func() {
		if Version != "" {
			resolved = Version
			return
		}
		resolved = fromBuildInfo()
	})
	return resolved
}

func fromBuildInfo() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var rev string
	var dirty bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}
