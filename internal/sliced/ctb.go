package sliced

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"math"
	"os"
)

const (
	magicCTBEncrypted = 0x12FD0107
	magicCBDDLP       = 0x12FD0019
	magicCTB          = 0x12FD0086
	magicCTBV4        = 0x12FD0106
)

// The AES key/IV for the encrypted .ctb variant are fixed constants baked into
// UVtools (XOR-obfuscated in their source with the string "UVtools"). They are
// the same for every file — not a per-file or per-printer secret — so deriving
// them here just reimplements published open-source logic.
var (
	ctbEncAESKey = xorCipher(mustB64("hQ36XB6yTk+zO02ysyiowt8yC1buK+nbLWyfY40EXoU="), "UVtools")
	ctbEncAESIV  = xorCipher(mustB64("Wld+ampndVJecmVjYH5cWQ=="), "UVtools")
)

func parseCTB(path string) (Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()

	var magicBuf [4]byte
	if _, err := f.ReadAt(magicBuf[:], 0); err != nil {
		return Info{}, errf("%s: cannot read magic: %v", path, err)
	}
	magic := binary.LittleEndian.Uint32(magicBuf[:])

	if magic == magicCTBEncrypted {
		return parseCTBEncrypted(path, f)
	}
	return parseCTBPlain(path, f)
}

// parseCTBEncrypted handles magic 0x12FD0107. The 288-byte SlicerSettings block
// is AES-256-CBC encrypted; the exact firmware PrintTime lives inside it.
func parseCTBEncrypted(path string, f *os.File) (Info, error) {
	header := make([]byte, 48)
	if _, err := f.ReadAt(header, 0); err != nil {
		return Info{}, errf("%s: short encrypted .ctb header: %v", path, err)
	}
	settingsSize := binary.LittleEndian.Uint32(header[4:])
	settingsOffset := binary.LittleEndian.Uint32(header[8:])
	if settingsSize == 0 || settingsSize%aes.BlockSize != 0 || settingsSize > 1<<16 {
		return Info{}, errf("%s: implausible SettingsSize %d", path, settingsSize)
	}

	enc := make([]byte, settingsSize)
	if _, err := f.ReadAt(enc, int64(settingsOffset)); err != nil {
		return Info{}, errf("%s: cannot read settings block: %v", path, err)
	}

	plain, err := aesCBCDecryptNoPadding(enc, ctbEncAESKey, ctbEncAESIV)
	if err != nil {
		return Info{}, errf("%s: decrypt settings: %v", path, err)
	}
	if len(plain) < 80 {
		return Info{}, errf("%s: decrypted settings block too small", path)
	}

	// SlicerSettings little-endian offsets (see Python _CTB_ENC_SETTINGS_FMT):
	//   LayerHeight f@36  ExposureTime f@40  BottomExposureTime f@44
	//   BottomLayerCount I@52  LayerCount I@64  PrintTime I@76
	return Info{
		Format:              "ctb_encrypted",
		LayerCount:          int(binary.LittleEndian.Uint32(plain[64:])),
		LayerHeightMM:       round4(float64(leFloatAt(plain, 36))),
		ExposureTimeS:       round2(float64(leFloatAt(plain, 40))),
		BottomExposureTimeS: round2(float64(leFloatAt(plain, 44))),
		BottomLayerCount:    int(binary.LittleEndian.Uint32(plain[52:])),
		EstimatedSeconds:    int(binary.LittleEndian.Uint32(plain[76:])),
		Exact:               true,
	}, nil
}

// parseCTBPlain handles the older unencrypted headers (cbddlp / ctb / ctbv4).
// UNVALIDATED against a real sample — see the Python module docstring.
func parseCTBPlain(path string, f *os.File) (Info, error) {
	h := make([]byte, 116) // through ProjectorType; we only need up to PrintTime@76
	if _, err := f.ReadAt(h, 0); err != nil {
		return Info{}, errf("%s: short .ctb header: %v", path, err)
	}
	magic := binary.LittleEndian.Uint32(h[0:])
	if magic != magicCBDDLP && magic != magicCTB && magic != magicCTBV4 {
		return Info{}, errf("%s: unrecognized .ctb magic 0x%08X", path, magic)
	}
	// Little-endian offsets (see Python _CTB_PLAIN_HEADER_FMT):
	//   LayerHeightMillimeter f@32  LayerExposureSeconds f@36
	//   BottomExposureSeconds f@40  BottomLayersCount I@48
	//   LayerCount I@68  PrintTime I@76
	return Info{
		Format:              "ctb_plain",
		LayerCount:          int(binary.LittleEndian.Uint32(h[68:])),
		LayerHeightMM:       round4(float64(leFloatAt(h, 32))),
		ExposureTimeS:       round2(float64(leFloatAt(h, 36))),
		BottomExposureTimeS: round2(float64(leFloatAt(h, 40))),
		BottomLayerCount:    int(binary.LittleEndian.Uint32(h[48:])),
		EstimatedSeconds:    int(binary.LittleEndian.Uint32(h[76:])),
		Exact:               true,
	}, nil
}

func leFloatAt(buf []byte, off int) float32 {
	return math.Float32frombits(binary.LittleEndian.Uint32(buf[off:]))
}

func aesCBCDecryptNoPadding(data, key, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(data)%block.BlockSize() != 0 {
		return nil, errf("ciphertext is not a multiple of the block size")
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	return out, nil
}

func xorCipher(data []byte, key string) []byte {
	kb := []byte(key)
	out := make([]byte, len(data))
	for i, b := range data {
		out[i] = b ^ kb[i%len(kb)]
	}
	return out
}

func mustB64(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic("sliced: bad embedded base64: " + err.Error())
	}
	return b
}
