package main

import "testing"

func TestShushFactor(t *testing.T) {
	for _, c := range []struct {
		rise, want float32
	}{
		{0, 1}, {10, 1}, {12, 1 - 2.0/50*0.8}, {60, 0.2}, {120, 0.2},
	} {
		if got := shushFactor(c.rise); got < c.want-0.01 || got > c.want+0.01 {
			t.Errorf("shushFactor(%v) = %v, want ~%v", c.rise, got, c.want)
		}
	}
}
