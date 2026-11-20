// facesvg prints the clock face frames for the README animation as JSON:
// the time with a blinking colon, weekday and weather, then a scrolled text, using the same
// display code clockd drives the real face with (internal/display).
//
//	go run ./tools/facesvg | node tools/facesvg/render.mjs > face.svg
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"lenovo-satellite/internal/display"
)

type frame struct {
	A  [6]byte `json:"a"`
	B  [6]byte `json:"b"`
	Ms int     `json:"ms"`
}

func main() {
	const (
		hhmm    = "2147"        // shown as 21:47
		day     = time.Thursday // weekday indicator
		blinks  = 4             // colon on/off cycles before and after the text
		blinkMs = 500           // as on the clock: on for the first half of each second
		text    = "hello luna"  // scrolled message (avoid k, m, w, x: they read badly on 7 segments)
		stepMs  = 250           // the default scroll speed (display.scroll_ms)
		holdMs  = 600           // blank pause after the text
		temp    = -3            // outdoor temperature (°C) from Home Assistant
		icon    = "snow"        // weather condition icon
	)
	var out []frame
	// the rest of the face stays on while text scrolls, as on the clock (face.go)
	weather := func(f *display.Frame) {
		f.Weekday(day)
		f.Temperature(temp)
		f.Set("celsius", true)
		f.Set(icon, true)
	}
	clock := func(colon bool) frame {
		var f display.Frame
		f.Big(hhmm)
		f.Set("colon", colon)
		weather(&f)
		return frame{A: f.A, B: f.B, Ms: blinkMs}
	}
	for i := 0; i < blinks; i++ {
		out = append(out, clock(true), clock(false))
	}
	strip := display.Strip(text)
	for s := 0; s <= display.ScrollSteps(strip); s++ {
		var f display.Frame
		f.SetBig(display.Window(strip, s))
		weather(&f)
		out = append(out, frame{A: f.A, B: f.B, Ms: stepMs})
	}
	out[len(out)-1].Ms = holdMs
	for i := 0; i < blinks; i++ {
		out = append(out, clock(true), clock(false))
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
