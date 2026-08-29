// Package config parses process configuration from the environment. All three
// binaries (portal server, portal worker, pi-agent) load from here so a
// Kubernetes Deployment or a systemd unit only ever needs to set env vars.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Portal is the configuration for the central service (server + worker + migrate
// subcommands). Not every field is used by every subcommand; Load only validates
// what the requested subcommand needs.
type Portal struct {
	// Shared.
	DatabaseURL string // libpq/pgx DSN, e.g. postgres://user:pass@host:5432/db?sslmode=disable
	LogLevel    string // debug|info|warn|error

	// server only.
	ListenAddr    string // e.g. :8080
	SessionSecret []byte // >=32 bytes, signs admin session cookies
	AdminUsername string // seed admin, created on first boot if the table is empty
	AdminPassword string // seed admin password (plaintext; hashed at seed time)
	StatusAPIKey  string // bearer key for GET /api/v1/status (org-wide read)
	EnrollToken   string // fleet bootstrap secret for POST /api/v1/enroll; blank disables it

	// worker only.
	SyncInterval          time.Duration
	TinkerAccessBaseURL   string // e.g. http://tinker-access.default.svc:3000
	TinkerAccessUsersPath string // e.g. /api/get_users11102523982452806591
	TinkerAccessWASMPath  string // optional: local path to the Leptos wasm bundle for hash recovery
}

// LoadPortal reads and validates configuration for the given subcommand
// ("server", "worker", or "migrate").
func LoadPortal(subcommand string) (*Portal, error) {
	c := &Portal{
		DatabaseURL:           env("DATABASE_URL", ""),
		LogLevel:              env("LOG_LEVEL", "info"),
		ListenAddr:            env("LISTEN_ADDR", ":8080"),
		AdminUsername:         env("ADMIN_USERNAME", "captain"),
		AdminPassword:         env("ADMIN_PASSWORD", ""),
		StatusAPIKey:          env("STATUS_API_KEY", ""),
		EnrollToken:           env("ENROLL_TOKEN", ""),
		TinkerAccessBaseURL:   strings.TrimRight(env("TINKERACCESS_BASE_URL", ""), "/"),
		TinkerAccessUsersPath: env("TINKERACCESS_GET_USERS_PATH", ""),
		TinkerAccessWASMPath:  env("TINKERACCESS_WASM_PATH", ""),
	}

	if v := os.Getenv("SESSION_SECRET"); v != "" {
		c.SessionSecret = []byte(v)
	}

	d, err := parseDuration(env("SYNC_INTERVAL", "10m"))
	if err != nil {
		return nil, fmt.Errorf("SYNC_INTERVAL: %w", err)
	}
	c.SyncInterval = d

	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}

	switch subcommand {
	case "server":
		if len(c.SessionSecret) < 32 {
			return nil, fmt.Errorf("SESSION_SECRET is required and must be at least 32 bytes")
		}
	case "worker":
		if c.TinkerAccessBaseURL == "" {
			return nil, fmt.Errorf("TINKERACCESS_BASE_URL is required for the worker")
		}
		if c.TinkerAccessUsersPath == "" && c.TinkerAccessWASMPath == "" {
			return nil, fmt.Errorf("set TINKERACCESS_GET_USERS_PATH (or TINKERACCESS_WASM_PATH so the path can be recovered)")
		}
	case "migrate":
		// DATABASE_URL is enough.
	default:
		return nil, fmt.Errorf("unknown subcommand %q", subcommand)
	}

	return c, nil
}

// PiAgent is the configuration for the on-Pi agent.
type PiAgent struct {
	CentralBaseURL string // https://portal.example.org
	EnrollToken    string // fleet bootstrap secret, used only until this Pi has credentials
	PrinterSlug    string // set after enrollment (or by hand for dev); empty => enroll
	PrinterAPIKey  string // set after enrollment (or by hand for dev); empty => enroll
	CredsPath      string // where slug+key are persisted after enrollment
	GadgetImage    string // path to the USB gadget image, e.g. /piusb.bin
	ListenAddr     string // e.g. :80
	RefreshScript  string // path to usb-refresh.sh
	LogLevel       string
}

// NeedsEnrollment reports whether the agent still has to self-register.
func (c *PiAgent) NeedsEnrollment() bool { return c.PrinterSlug == "" || c.PrinterAPIKey == "" }

// LoadPiAgent reads and validates the pi-agent configuration. PRINTER_SLUG /
// PRINTER_API_KEY are read from the environment first, then from the persisted
// creds file; if still absent the agent will enroll using ENROLL_TOKEN.
func LoadPiAgent() (*PiAgent, error) {
	c := &PiAgent{
		CentralBaseURL: strings.TrimRight(env("CENTRAL_BASE_URL", ""), "/"),
		EnrollToken:    env("ENROLL_TOKEN", ""),
		PrinterSlug:    env("PRINTER_SLUG", ""),
		PrinterAPIKey:  env("PRINTER_API_KEY", ""),
		CredsPath:      env("CREDS_PATH", "/var/lib/resin-pi-agent/creds.env"),
		GadgetImage:    env("GADGET_IMAGE", "/piusb.bin"),
		ListenAddr:     env("LISTEN_ADDR", ":80"),
		RefreshScript:  env("USB_REFRESH_SCRIPT", "/usr/local/bin/usb-refresh.sh"),
		LogLevel:       env("LOG_LEVEL", "info"),
	}
	if c.CentralBaseURL == "" {
		return nil, fmt.Errorf("CENTRAL_BASE_URL is required")
	}

	// Fall back to persisted credentials from a previous enrollment.
	if c.NeedsEnrollment() {
		if slug, key, ok := readCredsFile(c.CredsPath); ok {
			c.PrinterSlug, c.PrinterAPIKey = slug, key
		}
	}
	// No credentials and no token is fine: the Pi self-registers against an
	// open enroll endpoint. ENROLL_TOKEN is only needed if the portal requires
	// one (public-internet deployments).
	return c, nil
}

// readCredsFile parses a KEY=VALUE file for PRINTER_SLUG / PRINTER_API_KEY.
func readCredsFile(path string) (slug, key string, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		k, v, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch strings.TrimSpace(k) {
		case "PRINTER_SLUG":
			slug = strings.Trim(strings.TrimSpace(v), `"'`)
		case "PRINTER_API_KEY":
			key = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return slug, key, slug != "" && key != ""
}

// WriteCredsFile persists slug+key so the Pi doesn't re-enroll every boot.
func WriteCredsFile(path, slug, key string) error {
	if dir := filepathDir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	body := fmt.Sprintf("PRINTER_SLUG=%s\nPRINTER_API_KEY=%s\n", slug, key)
	return os.WriteFile(path, []byte(body), 0o600)
}

func filepathDir(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return ""
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseDuration accepts Go durations ("10m", "1h30m") and also a bare integer
// meaning seconds, which is friendlier in a Helm values file.
func parseDuration(s string) (time.Duration, error) {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return time.ParseDuration(s)
}
