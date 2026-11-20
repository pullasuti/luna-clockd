package main

import (
	"fmt"
	"os"
	"time"

	"lenovo-satellite/internal/wifi"
)

// wpaTool is "clockd -wpa CMD" / "-wpa-watch 30s": a look at wpa_supplicant
// through the same control-socket client clockd uses (the vendor wpa_cli is
// not used any more). Prints the reply, then events with the time they came.
func wpaTool(cmd string, watch time.Duration) int {
	c := &wifi.Ctrl{}
	defer c.Close()
	ev, cancel := c.Subscribe()
	defer cancel()
	if cmd == "" {
		cmd = "PING"
	}
	out, err := c.Request(cmd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "clockd -wpa:", err)
		return 1
	}
	fmt.Println(out)
	if watch <= 0 {
		return 0
	}
	end := time.After(watch)
	for {
		select {
		case e := <-ev:
			fmt.Printf("%s <%d>%s\n", time.Now().Format("15:04:05.000"), e.Level, e.Text)
		case <-end:
			return 0
		}
	}
}
