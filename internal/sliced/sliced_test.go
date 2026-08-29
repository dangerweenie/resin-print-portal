package sliced

import (
	"archive/zip"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// --- fixture builders, ported from printer-upload/tests/fixtures/sliced_files.py ---

func writeGoo(t *testing.T, path string, printTime, layerCount uint32, layerHeight, exposure, bottomExposure float32, bottomLayers uint32, volume float32, machine string) {
	t.Helper()
	const gooLen = 195454
	buf := make([]byte, gooLen)
	copy(buf[0:4], "V3.0")
	putStr(buf, 12, 32, "UVtools")
	putStr(buf, 92, 32, machine)
	binary.BigEndian.PutUint32(buf[195310:], layerCount)
	putBEFloat(buf, 195332, layerHeight)
	putBEFloat(buf, 195336, exposure)
	putBEFloat(buf, 195369, bottomExposure)
	binary.BigEndian.PutUint32(buf[195373:], bottomLayers)
	binary.BigEndian.PutUint32(buf[195446:], printTime)
	putBEFloat(buf, 195450, volume)
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeCTBPlain(t *testing.T, path string, magic uint32, layerCount uint32, layerHeight, exposure, bottomExposure float32, bottomLayers, printTime uint32) {
	t.Helper()
	h := make([]byte, 116)
	binary.LittleEndian.PutUint32(h[0:], magic)
	binary.LittleEndian.PutUint32(h[4:], 1)
	putLEFloat(h, 32, layerHeight)
	putLEFloat(h, 36, exposure)
	putLEFloat(h, 40, bottomExposure)
	binary.LittleEndian.PutUint32(h[48:], bottomLayers)
	binary.LittleEndian.PutUint32(h[68:], layerCount)
	binary.LittleEndian.PutUint32(h[76:], printTime)
	if err := os.WriteFile(path, h, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeCTBEncrypted(t *testing.T, path string, layerCount uint32, layerHeight, exposure, bottomExposure float32, bottomLayers, printTime uint32) {
	t.Helper()
	const blockSize = 288
	s := make([]byte, blockSize)
	putLEFloat(s, 36, layerHeight)
	putLEFloat(s, 40, exposure)
	putLEFloat(s, 44, bottomExposure)
	binary.LittleEndian.PutUint32(s[52:], bottomLayers)
	binary.LittleEndian.PutUint32(s[64:], layerCount)
	binary.LittleEndian.PutUint32(s[76:], printTime)

	enc := aesCBCEncryptNoPadding(t, s, ctbEncAESKey, ctbEncAESIV)

	header := make([]byte, 48)
	binary.LittleEndian.PutUint32(header[0:], magicCTBEncrypted)
	binary.LittleEndian.PutUint32(header[4:], uint32(len(enc))) // SettingsSize
	binary.LittleEndian.PutUint32(header[8:], 48)               // SettingsOffset
	binary.LittleEndian.PutUint32(header[16:], 1)               // Version

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, _ = f.Write(header)
	_, _ = f.Write(enc)
}

func writePWSZ(t *testing.T, path, machine string, layerCount int, exposure, zupHeight, zupSpeed, zDownSpeed, offTime float64) {
	t.Helper()
	const resinCode = "SOME_RESIN"

	type para struct {
		ExposureTime float64 `json:"exposure_time"`
		ZupHeight    float64 `json:"zup_height"`
		ZupSpeed     float64 `json:"zup_speed"`
	}
	conf := struct {
		Paras []para `json:"paras"`
	}{}
	for i := 0; i < layerCount; i++ {
		conf.Paras = append(conf.Paras, para{exposure, zupHeight, zupSpeed})
	}

	meta := map[string]any{
		"machine_type": map[string]any{"name": machine},
		"machine_extern": map[string]any{
			"active_resins": []string{resinCode},
			"user_resins": []map[string]any{{
				"property":  map[string]any{"name": resinCode},
				"slicepara": map[string]any{"off_time": offTime, "zdown_speed": zDownSpeed},
			}},
		},
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	writeZipJSON(t, zw, "anycubic_photon_resins.pwsp", meta)
	writeZipJSON(t, zw, "layers_controller.conf", conf)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// --- tests ---

func TestParseGoo(t *testing.T) {
	p := filepath.Join(t.TempDir(), "job.goo")
	writeGoo(t, p, 17553, 1200, 0.05, 2.5, 35.0, 5, 1234.5, "Elegoo Saturn 3")

	got, err := GetInfo(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Format != "goo" || !got.Exact {
		t.Errorf("format/exact = %q/%v", got.Format, got.Exact)
	}
	if got.EstimatedSeconds != 17553 {
		t.Errorf("EstimatedSeconds = %d, want 17553", got.EstimatedSeconds)
	}
	if got.LayerCount != 1200 || got.BottomLayerCount != 5 {
		t.Errorf("layers = %d / bottom %d", got.LayerCount, got.BottomLayerCount)
	}
	if got.MachineName != "Elegoo Saturn 3" {
		t.Errorf("MachineName = %q", got.MachineName)
	}
	if got.ExposureTimeS != 2.5 || got.LayerHeightMM != 0.05 {
		t.Errorf("exposure/height = %v / %v", got.ExposureTimeS, got.LayerHeightMM)
	}
}

func TestParseGooRejectsBadMarker(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.goo")
	buf := make([]byte, 195454)
	copy(buf, "XXXX")
	_ = os.WriteFile(p, buf, 0o644)
	if _, err := GetInfo(p); err == nil {
		t.Fatal("expected error for bad version marker")
	}
}

func TestParseCTBPlain(t *testing.T) {
	p := filepath.Join(t.TempDir(), "job.ctb")
	writeCTBPlain(t, p, magicCBDDLP, 800, 0.05, 2.5, 35.0, 5, 14400)

	got, err := GetInfo(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Format != "ctb_plain" || got.EstimatedSeconds != 14400 || got.LayerCount != 800 {
		t.Errorf("got %+v", got)
	}
}

func TestParseCTBEncrypted(t *testing.T) {
	p := filepath.Join(t.TempDir(), "job.ctb")
	writeCTBEncrypted(t, p, 900, 0.03, 1.8, 30.0, 6, 16000)

	got, err := GetInfo(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Format != "ctb_encrypted" || !got.Exact {
		t.Errorf("format/exact = %q/%v", got.Format, got.Exact)
	}
	if got.EstimatedSeconds != 16000 || got.LayerCount != 900 || got.BottomLayerCount != 6 {
		t.Errorf("got %+v", got)
	}
	if got.ExposureTimeS != 1.8 {
		t.Errorf("ExposureTimeS = %v, want 1.8", got.ExposureTimeS)
	}
}

func TestParsePhotonWorkshopEstimate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "job.pwsz")
	// 100 layers: exposure 8s each; lift 6mm up @60mm/s = 0.1s; down @90 = 0.0667s;
	// off_time 0.5s each. per-layer = 8 + 0.1 + 0.06667 + 0.5 = 8.66667; x100 = 866.667 -> 867
	writePWSZ(t, p, "Anycubic Photon Mono M7 Pro", 100, 8.0, 6.0, 60.0, 90.0, 0.5)

	got, err := GetInfo(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Format != "photon_workshop" || got.Exact {
		t.Errorf("format/exact = %q/%v", got.Format, got.Exact)
	}
	if got.LayerCount != 100 {
		t.Errorf("LayerCount = %d", got.LayerCount)
	}
	if got.MachineName != "Anycubic Photon Mono M7 Pro" {
		t.Errorf("MachineName = %q", got.MachineName)
	}
	want := int(math.Round(100 * (8.0 + 6.0/60.0 + 6.0/90.0 + 0.5)))
	if got.EstimatedSeconds != want {
		t.Errorf("EstimatedSeconds = %d, want %d", got.EstimatedSeconds, want)
	}
}

func TestGetInfoUnsupported(t *testing.T) {
	p := filepath.Join(t.TempDir(), "job.stl")
	_ = os.WriteFile(p, []byte("x"), 0o644)
	if _, err := GetInfo(p); err == nil {
		t.Fatal("expected error for unsupported extension")
	}
}

// --- helpers ---

func putStr(buf []byte, off, length int, s string) {
	b := []byte(s)
	if len(b) > length {
		b = b[:length]
	}
	copy(buf[off:off+length], b)
}
func putBEFloat(buf []byte, off int, f float32) {
	binary.BigEndian.PutUint32(buf[off:], math.Float32bits(f))
}
func putLEFloat(buf []byte, off int, f float32) {
	binary.LittleEndian.PutUint32(buf[off:], math.Float32bits(f))
}

func aesCBCEncryptNoPadding(t *testing.T, data, key, iv []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(data)%block.BlockSize() != 0 {
		t.Fatalf("fixture data %d not block-aligned", len(data))
	}
	out := make([]byte, len(data))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, data)
	return out
}

func writeZipJSON(t *testing.T, zw *zip.Writer, name string, v any) {
	t.Helper()
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatal(err)
	}
}
