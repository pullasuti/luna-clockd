package main

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// The vendor's net_mgr has an Android FrameworkListener command socket with
// "hotspot" commands. Since 0.80.0 start.sh stops net_mgr and setup-ap.sh runs
// hostapd itself; this client is the fallback route, used by setup-ap.sh via
// clockd -netmgr when /data/local/clockd/use-net_mgr exists.
//
//	hotspot ssid <name>     -> "0 Success"
//	hotspot start           -> "1 Started"   (uap0 at 192.168.255.249/29, its dnsmasq: DHCP only)
//	hotspot stop / status   -> "0 Stopped" / "1 Started"
//
// Messages are NUL-terminated text without sequence numbers; replies are
// "<code> <text>" (22 and 500 are errors). The socket is SEQPACKET.

const netMgrSocket = "/dev/socket/net_mgr"

// netMgr sends one command and returns net_mgr's first reply.
func netMgr(cmd string) (code int, text string, err error) {
	c, err := net.DialTimeout("unixpacket", netMgrSocket, 3*time.Second)
	if err != nil {
		return 0, "", err
	}
	defer c.Close()
	if _, err := c.Write(append([]byte(cmd), 0)); err != nil {
		return 0, "", err
	}
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 4096)
	n, err := c.Read(buf)
	if err != nil {
		return 0, "", err
	}
	reply := strings.TrimRight(string(buf[:n]), "\x00")
	reply = strings.SplitN(reply, "\x00", 2)[0]
	f := strings.SplitN(reply, " ", 2)
	code, err = strconv.Atoi(f[0])
	if err != nil {
		return 0, reply, fmt.Errorf("unexpected reply %q", reply)
	}
	if len(f) > 1 {
		text = f[1]
	}
	if code == 22 || code >= 500 {
		return code, text, errors.New(text)
	}
	return code, text, nil
}
