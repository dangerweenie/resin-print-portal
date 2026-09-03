package piagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dangerweenie/resin-print-portal/internal/buildinfo"
)

// AgentUpdateCheckInterval is how often a running agent asks the portal whether
// it should replace its own binary. Updates are never urgent; a slow cadence
// keeps the noise down.
const AgentUpdateCheckInterval = 5 * time.Minute

// maxUpdateAttempts caps how many times the agent will try to install the same
// target version before giving up and waiting for the portal to change it. The
// Pi then shows as "behind" in the admin UI for a human to look at.
const maxUpdateAttempts = 3

// restartTrackFile is shared with pi/agent-guard.sh: the guard counts restarts
// recorded here and rolls back to pi-agent.prev if the new binary crash-loops.
const restartTrackFile = "restart-track"

const updateStateFile = "update-state.json"

type updateState struct {
	Target   string `json:"target"`
	Attempts int    `json:"attempts"`
}

// SelfUpdater replaces the running pi-agent binary in place when the portal
// advertises a newer build. Every scrap of state lives in the systemd
// StateDirectory (next to the enrollment creds), never on the boot partition.
type SelfUpdater struct {
	client   *CentralClient
	stateDir string
	exePath  string
	running  string
	restart  func() // trigger a graceful shutdown; systemd (Restart=always) brings us back
	log      *slog.Logger
}

// NewSelfUpdater builds an updater. credsPath is cfg.CredsPath — its directory
// is the StateDirectory. restart is called after a successful binary swap.
func NewSelfUpdater(client *CentralClient, credsPath string, restart func(), log *slog.Logger) *SelfUpdater {
	exe, err := os.Executable()
	if err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
	} else {
		exe = ""
	}
	return &SelfUpdater{
		client:   client,
		stateDir: filepath.Dir(credsPath),
		exePath:  exe,
		running:  buildinfo.Resolve(),
		restart:  restart,
		log:      log,
	}
}

// Run checks once now and then on AgentUpdateCheckInterval until ctx is done.
func (u *SelfUpdater) Run(ctx context.Context) {
	u.Tick(ctx)
	t := time.NewTicker(AgentUpdateCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			u.Tick(ctx)
		}
	}
}

// ConfirmStart clears the crash-loop restart tracker and, if we came up running
// the version a previous update was aiming for, marks that update done. Call it
// once the HTTP listener is up and the portal has answered at least once.
func (u *SelfUpdater) ConfirmStart() {
	_ = os.Remove(filepath.Join(u.stateDir, restartTrackFile))
	st := u.loadState()
	if st.Target != "" && st.Target == u.running {
		u.log.Info("pi-agent self-update confirmed", "version", u.running)
		_ = os.Remove(filepath.Join(u.stateDir, updateStateFile))
	}
}

// Tick performs one check and, if warranted, one update.
func (u *SelfUpdater) Tick(ctx context.Context) {
	info, err := u.client.AgentUpdate(ctx)
	if err != nil {
		u.log.Debug("agent-update check failed", "err", err)
		return
	}
	if !info.UpdateAvailable || info.TargetVersion == "" || info.TargetVersion == u.running {
		return
	}
	st := u.loadState()
	if st.Target == info.TargetVersion && st.Attempts >= maxUpdateAttempts {
		u.log.Warn("pi-agent self-update parked until the portal changes target",
			"target", info.TargetVersion, "attempts", st.Attempts, "running", u.running)
		return
	}
	if u.exePath == "" {
		u.log.Warn("pi-agent self-update: can't locate my own binary path")
		return
	}
	// Never pull the drive out from under a running print.
	if cj, err := u.client.FetchCurrentJob(ctx); err == nil && cj.CurrentJob != nil &&
		cj.CurrentJob.Status == "printing" {
		u.log.Info("pi-agent self-update deferred — a print is in progress")
		return
	}

	// Record the attempt before doing anything risky, so a download failure or
	// a crash mid-swap still counts toward the give-up limit.
	if st.Target != info.TargetVersion {
		st.Attempts = 0
	}
	st.Target = info.TargetVersion
	st.Attempts++
	u.saveState(st)

	if err := u.apply(ctx, info); err != nil {
		u.log.Error("pi-agent self-update failed", "target", info.TargetVersion,
			"attempt", st.Attempts, "err", err)
		return
	}
	u.log.Warn("pi-agent self-update applied — restarting",
		"from", u.running, "to", info.TargetVersion)
	u.restart()
}

func (u *SelfUpdater) apply(ctx context.Context, info AgentUpdateInfo) error {
	dir := filepath.Dir(u.exePath)
	tmp, err := os.CreateTemp(dir, ".pi-agent-new-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // harmless no-op once the rename succeeds

	body, _, err := u.client.DownloadAgent(ctx, info.URL)
	if err != nil {
		tmp.Close()
		return err
	}
	defer body.Close()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	got := hex.EncodeToString(h.Sum(nil))
	if info.SHA256 != "" && !strings.EqualFold(got, info.SHA256) {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, info.SHA256)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}

	// Stash the current binary as pi-agent.prev for the crash-loop guard.
	prev := u.exePath + ".prev"
	_ = os.Remove(prev)
	if err := copyFile(u.exePath, prev, 0o755); err != nil {
		u.log.Warn("pi-agent self-update: couldn't stash rollback copy", "err", err)
	}

	// Rename over the running binary. Linux allows this even though the file is
	// executing — the running process keeps the old inode; the next start uses
	// the new one.
	if err := os.Rename(tmpName, u.exePath); err != nil {
		return fmt.Errorf("swap binary: %w", err)
	}
	return nil
}

func (u *SelfUpdater) loadState() updateState {
	var st updateState
	b, err := os.ReadFile(filepath.Join(u.stateDir, updateStateFile))
	if err != nil {
		return st
	}
	_ = json.Unmarshal(b, &st)
	return st
}

func (u *SelfUpdater) saveState(st updateState) {
	b, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(u.stateDir, updateStateFile), b, 0o600); err != nil {
		u.log.Warn("pi-agent self-update: couldn't persist update state", "err", err)
	}
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
