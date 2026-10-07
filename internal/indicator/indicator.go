// Package indicator drives the Pi's physical tap feedback: a green LED +
// piezo buzzer when a tap just loaded a file onto the printer, a red LED when
// a tap was denied or a load failed. Pure Go via periph.io — the same
// approach internal/rfid used for the old SPI-based MFRC522 reader.
package indicator

import (
	"fmt"
	"sync"
	"time"

	"periph.io/x/conn/v3/gpio"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"periph.io/x/conn/v3/physic"
	host "periph.io/x/host/v3"
)

// Fixed wiring — nothing about this is configurable on the Pi:
//
//	green LED  anode (via ~330Ω) -> GPIO17 (pin 11); cathode -> GND (pin 14)
//	red LED    anode (via ~330Ω) -> GPIO22 (pin 15); cathode -> GND (pin 14)
//	piezo buzzer, one leg        -> GPIO18 (pin 12, hardware PWM0)
//	piezo buzzer, other leg      -> GND (pin 14)
const (
	greenPin = "GPIO17"
	redPin   = "GPIO22"
	buzzPin  = "GPIO18"

	litFor   = 3 * time.Second        // how long an LED stays on after a tap
	beepFor  = 200 * time.Millisecond // how long the buzzer tones for
	beepFreq = 2500 * physic.Hertz    // audible, well within a small piezo disc's range
)

// Lights is the real GPIO-backed indicator. A fake stands in for it in
// piagent's tests; see piagent.Indicator.
type Lights struct {
	green, red, buzz gpio.PinIO

	mu       sync.Mutex
	greenOff *time.Timer
	redOff   *time.Timer
}

// Open opens the three GPIO pins. Fails cleanly if the hardware isn't wired
// (a dev machine, or a Pi that hasn't had the LEDs/buzzer added yet) — this
// is cosmetic feedback, not identity, so callers should log the error and run
// without it rather than refuse to start, unlike rfid.Open.
func Open() (*Lights, error) {
	if _, err := host.Init(); err != nil {
		return nil, fmt.Errorf("indicator: periph host init: %w", err)
	}
	green := gpioreg.ByName(greenPin)
	red := gpioreg.ByName(redPin)
	buzz := gpioreg.ByName(buzzPin)
	if green == nil || red == nil || buzz == nil {
		return nil, fmt.Errorf("indicator: GPIO pin(s) not found (want %s, %s, %s)", greenPin, redPin, buzzPin)
	}
	if err := green.Out(gpio.Low); err != nil {
		return nil, fmt.Errorf("indicator: init green LED (%s): %w", greenPin, err)
	}
	if err := red.Out(gpio.Low); err != nil {
		return nil, fmt.Errorf("indicator: init red LED (%s): %w", redPin, err)
	}
	if err := buzz.Out(gpio.Low); err != nil {
		return nil, fmt.Errorf("indicator: init buzzer (%s): %w", buzzPin, err)
	}
	return &Lights{green: green, red: red, buzz: buzz}, nil
}

// Success lights the green LED for a few seconds and sounds one short beep —
// a tap that just loaded a file onto the printer.
func (l *Lights) Success() {
	l.flash(l.green, &l.greenOff)
	go l.beep()
}

// Deny lights the red LED for a few seconds — a tap that was denied, or a
// load that failed. No beep: a buzzer going off on every mistyped/uncertified
// tap in a shared space would get old fast.
func (l *Lights) Deny() {
	l.flash(l.red, &l.redOff)
}

// flash turns pin on and schedules it back off after litFor. A retrigger
// (another tap while it's still lit) restarts the timer instead of stacking
// goroutines or turning off early.
func (l *Lights) flash(pin gpio.PinIO, timer **time.Timer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if *timer != nil {
		(*timer).Stop()
	}
	_ = pin.Out(gpio.High)
	*timer = time.AfterFunc(litFor, func() { _ = pin.Out(gpio.Low) })
}

// beep drives the buzzer's hardware PWM pin at an audible frequency for
// beepFor, then stops. Run in its own goroutine — Success doesn't block on it.
func (l *Lights) beep() {
	_ = l.buzz.PWM(gpio.DutyHalf, beepFreq)
	time.Sleep(beepFor)
	_ = l.buzz.Out(gpio.Low)
}

// Close turns everything off. Safe to call even if nothing is currently lit.
func (l *Lights) Close() error {
	l.mu.Lock()
	if l.greenOff != nil {
		l.greenOff.Stop()
	}
	if l.redOff != nil {
		l.redOff.Stop()
	}
	l.mu.Unlock()
	_ = l.green.Out(gpio.Low)
	_ = l.red.Out(gpio.Low)
	_ = l.buzz.Out(gpio.Low)
	return nil
}
