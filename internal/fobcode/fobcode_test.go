package fobcode

import (
	"slices"
	"testing"
)

func TestVariants(t *testing.T) {
	got := Variants([]byte{0xA1, 0xB2, 0xC3, 0xD4})
	for _, want := range []string{
		"A1B2C3D4", "a1b2c3d4", "A1:B2:C3:D4", "a1:b2:c3:d4",
		"2712847316", // big-endian
		"3569595041", // little-endian
	} {
		if !slices.Contains(got, want) {
			t.Errorf("Variants missing %q; got %v", want, got)
		}
	}
}

func TestVariantsFromHex(t *testing.T) {
	for _, in := range []string{"A1B2C3D4", "a1:b2:c3:d4", "0xA1B2C3D4", " A1B2C3D4 "} {
		v, err := VariantsFromHex(in)
		if err != nil {
			t.Fatalf("VariantsFromHex(%q): %v", in, err)
		}
		if !slices.Contains(v, "A1B2C3D4") {
			t.Errorf("VariantsFromHex(%q) = %v", in, v)
		}
	}
	if _, err := VariantsFromHex("nothex"); err == nil {
		t.Error("expected error for non-hex input")
	}
	if _, err := VariantsFromHex(""); err == nil {
		t.Error("expected error for empty input")
	}
}

func TestVariantsSevenByte(t *testing.T) {
	v := Variants([]byte{0x04, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66})
	if !slices.Contains(v, "04112233445566") || !slices.Contains(v, "04:11:22:33:44:55:66") {
		t.Errorf("7-byte variants = %v", v)
	}
}
