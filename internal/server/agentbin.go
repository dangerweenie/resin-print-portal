package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// agentBinary describes the cross-compiled pi-agent this image can hand out for
// a self-update. It is loaded once at startup; if the file isn't present (a
// local `make build`, say) OK is false and the update endpoints report "no
// update available" instead of failing.
type agentBinary struct {
	OK      bool
	Path    string
	Version string // the version string every Pi built from this commit reports
	SHA256  string
	Size    int64
}

func loadAgentBinary(path, version string) agentBinary {
	ab := agentBinary{Path: path, Version: version}
	f, err := os.Open(path)
	if err != nil {
		return ab
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return ab
	}
	ab.OK = true
	ab.Size = n
	ab.SHA256 = hex.EncodeToString(h.Sum(nil))
	return ab
}
