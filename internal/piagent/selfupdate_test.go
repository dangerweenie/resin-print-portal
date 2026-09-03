package piagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updSrv is a stub portal for the self-updater: it advertises one target build
// and serves its bytes.
type updSrv struct {
	*httptest.Server
	target     string
	binary     []byte
	sha        string
	sendBadSHA bool
	printing   bool
	binaryHits int
}

func newUpdSrv(t *testing.T, target string, binary []byte) *updSrv {
	t.Helper()
	sum := sha256.Sum256(binary)
	s := &updSrv{target: target, binary: binary, sha: hex.EncodeToString(sum[:])}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/printers/resin/agent-update", func(w http.ResponseWriter, r *http.Request) {
		sha := s.sha
		if s.sendBadSHA {
			sha = strings.Repeat("0", 64)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"update_available": true,
			"target_version":   s.target,
			"portal_version":   s.target,
			"url":              "/api/v1/printers/resin/agent-binary",
			"sha256":           sha,
			"size":             len(s.binary),
		})
	})
	mux.HandleFunc("/api/v1/printers/resin/agent-binary", func(w http.ResponseWriter, r *http.Request) {
		s.binaryHits++
		_, _ = w.Write(s.binary)
	})
	mux.HandleFunc("/api/v1/printers/resin/current-job", func(w http.ResponseWriter, r *http.Request) {
		if s.printing {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"current_job": map[string]any{"id": 1, "status": "printing"},
			})
			return
		}
		_, _ = w.Write([]byte(`{"current_job":null}`))
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// newTestUpdater wires a SelfUpdater at a throwaway exe path so a swap can't
// touch the real test binary.
func newTestUpdater(t *testing.T, srv *updSrv, running string) (*SelfUpdater, string, *bool) {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "pi-agent")
	if err := os.WriteFile(exe, []byte("OLD-BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	client := NewCentralClient(srv.URL, "resin", "key")
	restarted := false
	u := NewSelfUpdater(client, filepath.Join(dir, "creds.env"), func() { restarted = true }, quietLog())
	u.exePath = exe
	u.running = running
	return u, exe, &restarted
}

func TestSelfUpdaterAppliesUpdate(t *testing.T) {
	srv := newUpdSrv(t, "v2", []byte("NEW-BINARY-BYTES"))
	u, exe, restarted := newTestUpdater(t, srv, "v1")

	u.Tick(context.Background())

	if !*restarted {
		t.Fatal("restart was not requested after a successful swap")
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "NEW-BINARY-BYTES" {
		t.Fatalf("binary not swapped: %q", got)
	}
	if prev, err := os.ReadFile(exe + ".prev"); err != nil || string(prev) != "OLD-BINARY" {
		t.Fatalf("rollback copy not stashed: %v %q", err, prev)
	}
	st := u.loadState()
	if st.Target != "v2" || st.Attempts != 1 {
		t.Fatalf("update state = %+v, want {v2 1}", st)
	}
}

func TestSelfUpdaterRejectsBadChecksum(t *testing.T) {
	srv := newUpdSrv(t, "v2", []byte("NEW-BINARY-BYTES"))
	srv.sendBadSHA = true
	u, exe, restarted := newTestUpdater(t, srv, "v1")

	u.Tick(context.Background())

	if *restarted {
		t.Fatal("restarted despite a checksum mismatch")
	}
	if got, _ := os.ReadFile(exe); string(got) != "OLD-BINARY" {
		t.Fatalf("binary changed despite a checksum mismatch: %q", got)
	}
	if st := u.loadState(); st.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (the try still counts)", st.Attempts)
	}
}

func TestSelfUpdaterSkipsDuringPrint(t *testing.T) {
	srv := newUpdSrv(t, "v2", []byte("NEW-BINARY-BYTES"))
	srv.printing = true
	u, exe, restarted := newTestUpdater(t, srv, "v1")

	u.Tick(context.Background())

	if *restarted {
		t.Fatal("restarted while a print was in progress")
	}
	if got, _ := os.ReadFile(exe); string(got) != "OLD-BINARY" {
		t.Fatal("binary swapped while a print was in progress")
	}
	if srv.binaryHits != 0 {
		t.Fatalf("downloaded the binary %d times while printing", srv.binaryHits)
	}
}

func TestSelfUpdaterParksAfterMaxAttempts(t *testing.T) {
	srv := newUpdSrv(t, "v2", []byte("NEW-BINARY-BYTES"))
	u, _, restarted := newTestUpdater(t, srv, "v1")
	u.saveState(updateState{Target: "v2", Attempts: maxUpdateAttempts})

	u.Tick(context.Background())

	if *restarted {
		t.Fatal("retried an update that already failed maxUpdateAttempts times")
	}
	if srv.binaryHits != 0 {
		t.Fatal("re-downloaded a parked update")
	}
}

func TestSelfUpdaterNoopWhenCurrent(t *testing.T) {
	srv := newUpdSrv(t, "v2", []byte("NEW-BINARY-BYTES"))
	u, _, restarted := newTestUpdater(t, srv, "v2") // already on target

	u.Tick(context.Background())

	if *restarted || srv.binaryHits != 0 {
		t.Fatal("acted on an update when already running the target version")
	}
}

func TestConfirmStartClearsStateOnSuccess(t *testing.T) {
	srv := newUpdSrv(t, "v2", []byte("x"))
	u, _, _ := newTestUpdater(t, srv, "v2")
	u.saveState(updateState{Target: "v2", Attempts: 1})
	track := filepath.Join(u.stateDir, restartTrackFile)
	if err := os.WriteFile(track, []byte("123\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	u.ConfirmStart()

	if _, err := os.Stat(filepath.Join(u.stateDir, updateStateFile)); !os.IsNotExist(err) {
		t.Fatal("update-state.json not cleared after arriving on the target version")
	}
	if _, err := os.Stat(track); !os.IsNotExist(err) {
		t.Fatal("restart tracker not cleared on a healthy start")
	}
}
