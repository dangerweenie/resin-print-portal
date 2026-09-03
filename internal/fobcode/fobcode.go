// Package fobcode turns a raw RFID UID into the various string forms an access
// system might have recorded it as, so the portal can match a tapped fob
// against members.code without the Pi knowing which format was used.
package fobcode

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Variants renders a UID every way we know how: uppercase/lowercase hex (with
// and without colons) and big-/little-endian decimal. The Pi only ever sends
// canonical uppercase hex; the portal expands to all of these and matches.
func Variants(uid []byte) []string {
	if len(uid) == 0 {
		return nil
	}
	hexUpper := strings.ToUpper(hex.EncodeToString(uid))
	hexLower := strings.ToLower(hexUpper)

	colonUpper := make([]string, len(uid))
	for i, b := range uid {
		colonUpper[i] = fmt.Sprintf("%02X", b)
	}
	cU := strings.Join(colonUpper, ":")
	cL := strings.ToLower(cU)

	out := []string{
		hexUpper, hexLower, cU, cL,
		strconv.FormatUint(bytesToUint(uid, false), 10),
		strconv.FormatUint(bytesToUint(uid, true), 10),
	}
	return dedupe(out)
}

// VariantsFromHex parses a hex UID (with or without ':' / '0x') and expands it.
func VariantsFromHex(s string) ([]string, error) {
	clean := strings.ReplaceAll(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "0x"), ":", "")
	if clean == "" {
		return nil, fmt.Errorf("empty UID")
	}
	b, err := hex.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("not a hex UID %q: %w", s, err)
	}
	return Variants(b), nil
}

func bytesToUint(uid []byte, littleEndian bool) uint64 {
	var v uint64
	if littleEndian {
		for i := len(uid) - 1; i >= 0; i-- {
			v = v<<8 | uint64(uid[i])
		}
	} else {
		for _, b := range uid {
			v = v<<8 | uint64(b)
		}
	}
	return v
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
