package diag

import (
	"net"
	"os"
)

// Notify sends an sd_notify message to systemd. It does nothing outside a
// Type=notify unit.
func Notify(msg string) {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return
	}
	if addr[0] == '@' {
		addr = "\x00" + addr[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return
	}
	_, _ = conn.Write([]byte(msg))
	_ = conn.Close()
}
