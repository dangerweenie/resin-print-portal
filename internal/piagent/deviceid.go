package piagent

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// DeviceID returns a stable identifier for this Pi. It prefers the SoC serial
// from the device tree / cpuinfo; if that's missing or all-zeros it falls back
// to a random id persisted next to the creds file so it survives reboots.
func DeviceID(credsPath string) string {
	if s := socSerial(); s != "" {
		return "pi-" + s
	}
	idPath := filepath.Join(filepath.Dir(credsPath), "device-id")
	if b, err := os.ReadFile(idPath); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id
		}
	}
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	id := "gen-" + hex.EncodeToString(buf)
	_ = os.MkdirAll(filepath.Dir(idPath), 0o755)
	_ = os.WriteFile(idPath, []byte(id+"\n"), 0o600)
	return id
}

func socSerial() string {
	// Device tree (Raspberry Pi OS): a NUL-terminated string.
	if b, err := os.ReadFile("/sys/firmware/devicetree/base/serial-number"); err == nil {
		if s := cleanSerial(string(b)); s != "" {
			return s
		}
	}
	// Fallback: the "Serial" line in /proc/cpuinfo.
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(line, ":")
			if ok && strings.TrimSpace(k) == "Serial" {
				if s := cleanSerial(v); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func cleanSerial(s string) string {
	s = strings.TrimSpace(strings.Trim(s, "\x00"))
	s = strings.TrimLeft(s, "0")
	if s == "" || strings.Trim(s, "0") == "" {
		return ""
	}
	return s
}
