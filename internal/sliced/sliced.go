// Package sliced parses print-time, layer count, and target-machine name out of
// sliced resin print files. It is a Go port of the original
// printer-upload/sliced_file_info.py; byte offsets were validated there against
// real sample files (see docs/resin_plans.md). The encrypted-.ctb AES step uses
// the Go standard library rather than the pure-Python fallback that module had.
package sliced

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Error is returned for unrecognized or unparseable files.
type Error struct{ msg string }

func (e *Error) Error() string { return e.msg }

func errf(format string, a ...any) error { return &Error{msg: fmt.Sprintf(format, a...)} }

// Info is the metadata extracted from a sliced file.
type Info struct {
	Format              string  // goo | ctb_encrypted | ctb_plain | photon_workshop
	SoftwareName        string  // slicer that produced the file, when present
	MachineName         string  // target machine embedded in the file, when present
	LayerCount          int     //
	LayerHeightMM       float64 //
	ExposureTimeS       float64 //
	BottomExposureTimeS float64 //
	BottomLayerCount    int     //
	EstimatedSeconds    int     // print time — firmware-exact for goo/ctb, an estimate for photon_workshop
	VolumeMM3           float64 //
	Exact               bool    // true when EstimatedSeconds came straight from the file
}

// GetInfo parses the file at path, dispatching on its extension.
func GetInfo(path string) (Info, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".goo":
		return parseGoo(path)
	case ".ctb":
		return parseCTB(path)
	case ".pwsz", ".pp1", ".pm7", ".pm7m":
		return parsePhotonWorkshop(path)
	default:
		return Info{}, errf("%s: unsupported extension %q", path, ext)
	}
}

// FormatDuration renders seconds as "4h 53m" (or "12m 30s" under an hour),
// matching the old module's helper.
func FormatDuration(seconds int) string {
	if seconds < 0 {
		seconds = 0
	}
	h := seconds / 3600
	m := (seconds % 3600) / 60
	s := seconds % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm %ds", m, s)
}

func round2(f float64) float64 { return float64(int64(f*100+sign(f)*0.5)) / 100 }
func round4(f float64) float64 { return float64(int64(f*10000+sign(f)*0.5)) / 10000 }

func sign(f float64) float64 {
	if f < 0 {
		return -1
	}
	return 1
}
