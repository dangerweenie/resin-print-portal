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

func TestVariantsFiveByteEM4100AlsoOffersLastFourBytes(t *testing.T) {
	// uid[1:] is deliberately the same 4 bytes as TestVariants, so the known
	// decimal forms from that test double as the expected values here.
	got := Variants([]byte{0x01, 0xA1, 0xB2, 0xC3, 0xD4})
	for _, want := range []string{
		"01A1B2C3D4", // full 5-byte hex
		"A1B2C3D4",   // dropped-leading-byte 4-byte hex (the common "card number")
		"2712847316", // dropped-leading-byte big-endian decimal
		"3569595041", // dropped-leading-byte little-endian decimal
	} {
		if !slices.Contains(got, want) {
			t.Errorf("Variants(5-byte EM4100) missing %q; got %v", want, got)
		}
	}
}

func TestVariantsFourByteHasNoEM4100Split(t *testing.T) {
	// A 4-byte UID (e.g. MIFARE, kept as a generic case) must not spuriously
	// grow a "dropped byte" variant -- that logic is 5-byte-only.
	got := Variants([]byte{0xA1, 0xB2, 0xC3, 0xD4})
	if slices.Contains(got, "B2C3D4") {
		t.Errorf("4-byte UID should not produce a dropped-leading-byte variant, got %v", got)
	}
}
