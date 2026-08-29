package sliced

import (
	"encoding/binary"
	"math"
	"os"
)

// parseGoo reads a .goo header (Chitubox-family, big-endian). Every field is at
// a fixed offset; the running offsets here mirror parse_goo() in the original
// Python module, which was validated against a real Elegoo Saturn 3 file.
func parseGoo(path string) (Info, error) {
	buf, err := readAtMost(path, 300_000) // header + previews, with margin
	if err != nil {
		return Info{}, err
	}
	if len(buf) < 195_454 {
		return Info{}, errf("%s: file too short to be a .goo (%d bytes)", path, len(buf))
	}
	if string(buf[0:4]) != "V3.0" {
		return Info{}, errf("%s: unrecognized .goo version marker %q", path, buf[0:4])
	}

	off := 4 + 8 // Version + Magic
	softwareName := fixedStr(buf, off, 32)
	off += 32
	off += 24 // SoftwareVersion
	off += 24 // FileCreateTime
	machineName := fixedStr(buf, off, 32)
	off += 32
	off += 32 // MachineType
	off += 32 // ProfileName
	off += 2 + 2 + 2
	off += 116*116*2 + 2
	off += 290*290*2 + 2

	layerCount := binary.BigEndian.Uint32(buf[off:])
	off += 4
	off += 2 + 2 // ResolutionX, ResolutionY
	off += 1 + 1 // MirrorX, MirrorY
	off += 4 + 4 + 4
	layerHeight := beFloat32(buf, off)
	off += 4
	exposureTime := beFloat32(buf, off)
	off += 4
	off += 1     // DelayMode
	off += 4     // LightOffDelay
	off += 4 * 6 // BottomWaitTimeAfterCure .. WaitTimeBeforeCure
	bottomExposureTime := beFloat32(buf, off)
	off += 4
	bottomLayerCount := binary.BigEndian.Uint32(buf[off:])
	off += 4
	off += 4 * 16 // BottomLiftHeight .. RetractSpeed2
	off += 2 + 2  // BottomLightPWM, LightPWM
	off += 1      // PerLayerSettings
	printTime := binary.BigEndian.Uint32(buf[off:])
	off += 4
	volume := beFloat32(buf, off)

	return Info{
		Format:              "goo",
		SoftwareName:        softwareName,
		MachineName:         machineName,
		LayerCount:          int(layerCount),
		LayerHeightMM:       round4(float64(layerHeight)),
		ExposureTimeS:       round2(float64(exposureTime)),
		BottomExposureTimeS: round2(float64(bottomExposureTime)),
		BottomLayerCount:    int(bottomLayerCount),
		EstimatedSeconds:    int(printTime),
		VolumeMM3:           round2(float64(volume)),
		Exact:               true,
	}, nil
}

func readAtMost(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	got, err := f.Read(buf)
	if err != nil && got == 0 {
		return nil, err
	}
	return buf[:got], nil
}

func fixedStr(buf []byte, off, length int) string {
	if off+length > len(buf) {
		length = len(buf) - off
	}
	raw := buf[off : off+length]
	if i := indexByte(raw, 0); i >= 0 {
		raw = raw[:i]
	}
	return string(raw)
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func beFloat32(buf []byte, off int) float32 {
	return math.Float32frombits(binary.BigEndian.Uint32(buf[off:]))
}
