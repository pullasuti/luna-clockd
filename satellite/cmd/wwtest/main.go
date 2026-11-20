// wwtest: run microWakeWord live on the capture-service stream and report
// detections, peak scores and CPU cost.
//
//	wwtest <models dir> <seconds> [stream]
package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"syscall"
	"time"

	"lenovo-satellite/internal/capture"
	"lenovo-satellite/internal/wake"
)

func cpuTime() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func main() {
	if len(os.Args) < 3 {
		fmt.Println("usage: wwtest <models dir> <seconds> [stream]")
		os.Exit(1)
	}
	sec, _ := strconv.Atoi(os.Args[2])
	stream := capture.StreamEchoCancelled
	if len(os.Args) > 3 {
		stream, _ = strconv.Atoi(os.Args[3])
	}
	models, err := wake.LoadDir(os.Args[1])
	if err != nil || len(models) == 0 {
		fmt.Println("models:", err, len(models))
		os.Exit(1)
	}
	var dets []*wake.Detector
	for _, m := range models {
		d, err := wake.New(m, 0)
		if err != nil {
			fmt.Println(m.ID, err)
			os.Exit(1)
		}
		dets = append(dets, d)
		fmt.Printf("loaded %s (%q cutoff %.2f window %d step %dms)\n", m.ID, m.Phrase, m.Cutoff, m.Window, m.StepMs)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(sec)*time.Second)
	defer cancel()

	start, cpu0 := time.Now(), cpuTime()
	peak := make([]float64, len(dets))
	samples := 0
	last := time.Now()
	c := &capture.Client{Stream: stream}
	// TRACE=<ms>: print every model's peak score and the stream level per window
	traceMs, _ := strconv.Atoi(os.Getenv("TRACE"))
	var sumSq float64
	var nSq int
	c.Run(ctx, func(s []int16) {
		samples += len(s)
		for _, v := range s {
			sumSq += float64(v) * float64(v)
		}
		nSq += len(s)
		for i, d := range dets {
			if d.Process(s) {
				fmt.Printf("%s DETECTED %s\n", time.Now().Format("15:04:05.000"), d.Model.Phrase)
				d.Reset()
			}
			if sc := d.Score(); sc > peak[i] {
				peak[i] = sc
			}
		}
		if traceMs > 0 && time.Since(last) >= time.Duration(traceMs)*time.Millisecond {
			last = time.Now()
			db := -100.0
			if nSq > 0 && sumSq > 0 {
				db = 10 * math.Log10(sumSq/float64(nSq)/(32768*32768))
			}
			line := fmt.Sprintf("%s %6.1fdB", last.Format("15:04:05.000"), db)
			for i, d := range dets {
				line += fmt.Sprintf(" %s=%.2f", d.Model.ID, peak[i])
				peak[i] = 0
			}
			fmt.Println(line)
			sumSq, nSq = 0, 0
		}
		if traceMs == 0 && time.Since(last) > 5*time.Second {
			last = time.Now()
			for i, d := range dets {
				fmt.Printf("  %s peak score %.2f\n", d.Model.ID, peak[i])
				peak[i] = 0
			}
		}
	})
	wall, cpu := time.Since(start), cpuTime()-cpu0
	audio := time.Duration(samples) * time.Second / 16000
	fmt.Printf("audio %.1fs, wall %.1fs, cpu %.2fs = %.1f%% of one core\n",
		audio.Seconds(), wall.Seconds(), cpu.Seconds(), 100*cpu.Seconds()/wall.Seconds())
}
