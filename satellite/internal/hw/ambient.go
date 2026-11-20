package hw

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// AmbientLight is the STK3311 ambient light / proximity sensor (stk3310 IIO driver).
//
// It sits behind the tinted front window, so readings are heavily attenuated
// and uncalibrated (no /factory/als_calibration.txt on this unit). Measured at
// a 0.379 s integration time and scale 0.1: dark 0, cozy lamp light 6-7, all
// room lights on ~270, phone flashlight thousands.
type AmbientLight struct {
	dir string
}

// IntegrationTime is long enough to separate "dark" from "dim lamp".
const IntegrationTime = "0.378880"

// OpenAmbientLight finds the sensor and sets its integration time and gain.
func OpenAmbientLight() (*AmbientLight, error) {
	devs, _ := filepath.Glob("/sys/bus/iio/devices/iio:device*")
	for _, d := range devs {
		name, _ := os.ReadFile(filepath.Join(d, "name"))
		if strings.TrimSpace(string(name)) != "stk3310" {
			continue
		}
		a := &AmbientLight{dir: d}
		// most sensitive gain, longer integration (ignore errors: keep defaults)
		os.WriteFile(filepath.Join(d, "in_illuminance_scale"), []byte("0.1"), 0)
		os.WriteFile(filepath.Join(d, "in_illuminance_integration_time"), []byte(IntegrationTime), 0)
		return a, nil
	}
	return nil, fmt.Errorf("no stk3310 light sensor found")
}

func (a *AmbientLight) readInt(f string) (int, error) {
	b, err := os.ReadFile(filepath.Join(a.dir, f))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// Raw is the illuminance count (0..65535).
func (a *AmbientLight) Raw() (int, error) { return a.readInt("in_illuminance_raw") }

// Proximity is the IR proximity count (~5 nothing, ~300+ hand in front).
func (a *AmbientLight) Proximity() (int, error) { return a.readInt("in_proximity_raw") }

// LightLevel maps a raw count to 0..1 on a log scale: 0 dark, ~0.34 dim lamp,
// 1.0 at a fully lit room (~300) and above.
func LightLevel(raw float64) float64 {
	const full = 300
	if raw <= 0 {
		return 0
	}
	return min(1, math.Log1p(raw)/math.Log1p(full))
}
