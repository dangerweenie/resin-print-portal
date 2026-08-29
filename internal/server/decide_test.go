package server

import "testing"

func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"  Jane Doe  ":    "jane doe",
		"@jane.doe":       "jane.doe",
		"JANE   DOE":      "jane doe",
		"\tHunter\nCohen": "hunter cohen",
		"":                "",
		"@":               "",
	}
	for in, want := range cases {
		if got := NormalizeName(in); got != want {
			t.Errorf("NormalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtensionAllowed(t *testing.T) {
	allow := []string{".pwsz", ".pm7", ".pm7m"}
	tests := []struct {
		name    string
		allowed []string
		file    string
		want    bool
	}{
		{"match", allow, "DancingRocky.pwsz", true},
		{"match case-insensitive", allow, "JOB.PWSZ", true},
		{"no match", allow, "figure.goo", false},
		{"no extension", allow, "README", false},
		{"empty allowlist allows anything", nil, "whatever.xyz", true},
	}
	for _, tc := range tests {
		if got := extensionAllowed(tc.allowed, tc.file); got != tc.want {
			t.Errorf("%s: extensionAllowed(%v, %q) = %v, want %v", tc.name, tc.allowed, tc.file, got, tc.want)
		}
	}
}

func TestMachineMismatch(t *testing.T) {
	if w := machineMismatch("Anycubic Photon Mono M7 Pro", "Anycubic Photon Mono M7 Pro"); w != "" {
		t.Errorf("exact match should not warn, got %q", w)
	}
	if w := machineMismatch("Anycubic Photon Mono M7 Pro", ""); w != "" {
		t.Errorf("empty sliced-for should not warn, got %q", w)
	}
	if w := machineMismatch("Anycubic Photon Mono M7 Pro", "unknown"); w != "" {
		t.Errorf("unknown sliced-for should not warn, got %q", w)
	}
	if w := machineMismatch("Anycubic Photon Mono M7 Pro", "Elegoo Saturn 3"); w == "" {
		t.Error("genuine mismatch should warn")
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd":    "passwd",
		`C:\Users\x\job.pwsz`: "job.pwsz",
		"my job (v2).goo":     "my job (v2).goo",
		"weird;name&here.ctb": "weird_name_here.ctb",
	}
	for in, want := range cases {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruthy(t *testing.T) {
	for _, s := range []string{"1", "on", "true", "yes", "x"} {
		if !truthy(s) {
			t.Errorf("truthy(%q) = false", s)
		}
	}
	for _, s := range []string{"", "0", "false", "off", "no"} {
		if truthy(s) {
			t.Errorf("truthy(%q) = true", s)
		}
	}
}
