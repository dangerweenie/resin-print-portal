// Package gadget drives the USB mass-storage gadget on the Pi by shelling out
// to usb-refresh.sh, which does the modprobe -r / losetup -fP / mount / copy /
// remount cycle documented in CLAUDE.md.
package gadget

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Gadget invokes usb-refresh.sh with the right arguments.
type Gadget struct {
	script string
	image  string
}

// New returns a Gadget bound to a usb-refresh.sh path and a gadget image path.
func New(scriptPath, imagePath string) *Gadget {
	return &Gadget{script: scriptPath, image: imagePath}
}

// Write places exactly srcPath onto the gadget (replacing whatever was there)
// and re-presents the drive to the printer.
func (g *Gadget) Write(ctx context.Context, srcPath string) error {
	return g.run(ctx, srcPath)
}

// Clear empties the gadget and re-presents an empty drive.
func (g *Gadget) Clear(ctx context.Context) error {
	return g.run(ctx, "--clear")
}

func (g *Gadget) run(ctx context.Context, arg string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, g.script, arg)
	if g.image != "" {
		cmd.Env = append(cmd.Environ(), "PIUSB_IMAGE="+g.image)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("usb-refresh.sh %s: %w: %s", arg, err, strings.TrimSpace(string(out)))
	}
	return nil
}
