package main

import "testing"

func TestWakeHistory(t *testing.T) {
	var h wakeHistory
	for i := 0; i < wakeHistoryMax+5; i++ {
		h.add(wakeEvent{Score: float64(i)})
	}
	l := h.list()
	if len(l) != wakeHistoryMax {
		t.Fatalf("%d events, want %d", len(l), wakeHistoryMax)
	}
	if l[0].Score != wakeHistoryMax+4 || l[len(l)-1].Score != 5 {
		t.Fatalf("order: newest %v, oldest %v", l[0].Score, l[len(l)-1].Score)
	}
}
