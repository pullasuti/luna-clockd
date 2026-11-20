package main

import (
	"os"
	"testing"
)

func TestProcesses(t *testing.T) {
	var p procSampler
	out := p.Processes()
	rows := out["procs"].([]procRow)
	self := false
	for _, r := range rows {
		if r.PID == os.Getpid() {
			self = true
			if r.MemMB <= 0 || r.Threads < 1 {
				t.Errorf("own process: %+v", r)
			}
		}
		if r.CPU < 0 || r.CPU > 100 {
			t.Errorf("cpu %v for %s", r.CPU, r.Name)
		}
	}
	if !self || out["cores"].(int) < 1 {
		t.Fatalf("own process missing or no cores: %d rows", len(rows))
	}
}
