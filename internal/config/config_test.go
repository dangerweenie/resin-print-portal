package config

import "testing"

func TestValidateCentralBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"http with port", "http://192.168.40.253:8080", false},
		{"https with host", "https://portal.tinkermill.org", false},
		{"bare IP, no scheme", "192.168.40.253", true}, // the actual bug seen in the field
		{"bare host, no scheme", "portal.tinkermill.org", true},
		{"unsupported scheme", "ftp://192.168.40.253", true},
		{"scheme, no host", "http://", true},
		{"empty", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCentralBaseURL(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateCentralBaseURL(%q) err = %v, wantErr %v", tc.raw, err, tc.wantErr)
			}
		})
	}
}

func TestLoadPiAgentRejectsSchemelessCentralURL(t *testing.T) {
	t.Setenv("CENTRAL_BASE_URL", "192.168.40.253")
	if _, err := LoadPiAgent(); err == nil {
		t.Fatal("expected LoadPiAgent to reject a schemeless CENTRAL_BASE_URL up front")
	}
}
