package sliced

import (
	"archive/zip"
	"encoding/json"
	"io"
	"math"
	"strings"
)

// Anycubic "Photon Workshop" ZIP container (.pwsz / .pp1 / .pm7 / .pm7m). No
// exact total print time is stored, so we ESTIMATE from per-layer exposure plus
// Z lift/retract plus light-off delay — same model as the Python port. Flag the
// result as not Exact.
type pwsp struct {
	MachineType struct {
		Name string `json:"name"`
	} `json:"machine_type"`
	MachineExtern struct {
		ActiveResins []string `json:"active_resins"`
		UserResins   []struct {
			Property struct {
				Name string `json:"name"`
			} `json:"property"`
			Slicepara struct {
				OffTime    float64 `json:"off_time"`
				ZDownSpeed float64 `json:"zdown_speed"`
			} `json:"slicepara"`
		} `json:"user_resins"`
	} `json:"machine_extern"`
}

type layersConf struct {
	Paras []struct {
		ExposureTime float64 `json:"exposure_time"`
		ZupHeight    float64 `json:"zup_height"`
		ZupSpeed     float64 `json:"zup_speed"`
	} `json:"paras"`
}

func parsePhotonWorkshop(path string) (Info, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return Info{}, errf("%s: not a readable zip: %v", path, err)
	}
	defer zr.Close()

	var pwspFile, confFile *zip.File
	for _, f := range zr.File {
		switch {
		case strings.HasSuffix(f.Name, ".pwsp"):
			pwspFile = f
		case strings.HasSuffix(f.Name, "layers_controller.conf"):
			confFile = f
		}
	}
	if pwspFile == nil || confFile == nil {
		return Info{}, errf("%s: not a Photon Workshop container (missing .pwsp or layers_controller.conf)", path)
	}

	var meta pwsp
	if err := readJSONZip(pwspFile, &meta); err != nil {
		return Info{}, errf("%s: bad .pwsp: %v", path, err)
	}
	var conf layersConf
	if err := readJSONZip(confFile, &conf); err != nil {
		return Info{}, errf("%s: bad layers_controller.conf: %v", path, err)
	}
	if len(conf.Paras) == 0 {
		return Info{}, errf("%s: layers_controller.conf has no layer data", path)
	}

	machineName := meta.MachineType.Name
	if machineName == "" {
		machineName = "unknown"
	}

	// Resolve light-off delay and retract speed from the active resin profile,
	// if the structure matches; otherwise fall back to defaults.
	var offTime, zDownSpeed float64
	if len(meta.MachineExtern.ActiveResins) > 0 {
		active := meta.MachineExtern.ActiveResins[0]
		for _, r := range meta.MachineExtern.UserResins {
			if r.Property.Name == active {
				offTime = r.Slicepara.OffTime
				zDownSpeed = r.Slicepara.ZDownSpeed
				break
			}
		}
	}

	var totalExposure, totalUp float64
	for _, p := range conf.Paras {
		totalExposure += p.ExposureTime
		if p.ZupSpeed != 0 {
			totalUp += p.ZupHeight / p.ZupSpeed
		}
	}
	totalDown := totalUp // assume symmetric if zdown_speed didn't resolve
	if zDownSpeed != 0 {
		totalDown = 0
		for _, p := range conf.Paras {
			totalDown += p.ZupHeight / zDownSpeed
		}
	}
	totalOff := offTime * float64(len(conf.Paras))

	estimate := totalExposure + totalUp + totalDown + totalOff

	return Info{
		Format:           "photon_workshop",
		MachineName:      machineName,
		LayerCount:       len(conf.Paras),
		EstimatedSeconds: int(math.Round(estimate)),
		Exact:            false,
	}, nil
}

func readJSONZip(f *zip.File, v any) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
