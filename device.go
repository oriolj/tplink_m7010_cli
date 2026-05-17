package main

// Device abstracts over the supported routers (TP-Link M7010 AES+RSA
// envelope, GL.iNet Mudi GL-E5800 OpenWrt JSON-RPC). Discovery is
// opportunistic: prefer the kernel's default gateway, then a parallel
// TCP probe. Widget modes that can't reach anything emit empty JSON
// rather than burning power on doomed logins.

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Device lifecycle is Connect → (Fetch | Shutdown | Reboot)* → Close.
type Device interface {
	Name() string
	Connect(password string) error
	Fetch() (*Status, error)
	Shutdown() error
	Reboot() error
	Close()
}

type SupportedDevice struct {
	ID           string // CLI alias: "m7010" / "mudi"
	Title        string
	DefaultAddr  string
	AddrEnvs     []string // env vars consulted in order
	PasswordEnvs []string
	PasswordPath string // path under $XDG_CONFIG_HOME
	New          func(addr string, debug bool) Device
}

var supportedDevices = []SupportedDevice{
	{
		ID:           "m7010",
		Title:        "TP-Link M7010",
		DefaultAddr:  "192.168.0.1",
		AddrEnvs:     []string{"M7010_ADDR", "TPLINK_ADDR"},
		PasswordEnvs: []string{"M7010_PASS", "TPLINK_PASS"},
		PasswordPath: "tplink-m7010/password",
		New: func(addr string, debug bool) Device {
			return NewClient(addr, debug)
		},
	},
	{
		ID:           "mudi",
		Title:        "GL.iNet Mudi (GL-E5800)",
		DefaultAddr:  "192.168.8.1",
		AddrEnvs:     []string{"MUDI_ADDR", "GLINET_ADDR"},
		PasswordEnvs: []string{"MUDI_PASS", "GLINET_PASS"},
		PasswordPath: "gl-e5800/password",
		New: func(addr string, debug bool) Device {
			return NewMudiClient(addr, debug)
		},
	},
}

func findDeviceByID(id string) *SupportedDevice {
	for i := range supportedDevices {
		if supportedDevices[i].ID == id {
			return &supportedDevices[i]
		}
	}
	return nil
}

// resolveAddr: explicit flag > env vars (in registration order) > default.
func resolveAddr(d *SupportedDevice, flagAddr string) string {
	if flagAddr != "" {
		return flagAddr
	}
	for _, e := range d.AddrEnvs {
		if v := os.Getenv(e); v != "" {
			return v
		}
	}
	return d.DefaultAddr
}

// resolvePassword: explicit flag > env vars (in registration order) >
// password file. Returns "" if nothing's set — Connect then errors cleanly.
func resolvePassword(d *SupportedDevice, flagPass string) string {
	if flagPass != "" {
		return flagPass
	}
	for _, e := range d.PasswordEnvs {
		if v := os.Getenv(e); v != "" {
			return v
		}
	}
	return readPasswordFileAt(d.PasswordPath)
}

func readPasswordFileAt(rel string) string {
	data, err := os.ReadFile(filepath.Join(xdgConfigDir(), rel))
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(data), "\r\n")
}

func xdgConfigDir() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config")
	}
	return "~/.config"
}

// detectDevice tries two cheap signals, in order:
//
//  1. The kernel's default gateway. If we're already routing through a
//     known device's IP that's almost certainly the device we want.
//  2. Parallel TCP probe of every supported address, short timeout.
//
// Returns nil if nothing answers — callers in widget modes should emit
// empty output rather than burning power on a doomed login.
//
// Gateway-first matters: 192.168.0.1 is often accepted (pointing at
// nothing useful) through upstream NAT, and a raw TCP probe will pick
// the wrong device. The gateway signal is unambiguous because that's
// literally where our packets go.
func detectDevice(timeout time.Duration) *SupportedDevice {
	if gw := defaultGateway(); gw != "" {
		for i, d := range supportedDevices {
			if d.DefaultAddr == gw {
				return &supportedDevices[i]
			}
		}
	}
	results := make([]bool, len(supportedDevices))
	var wg sync.WaitGroup
	for i, d := range supportedDevices {
		wg.Add(1)
		go func(i int, addr string) {
			defer wg.Done()
			results[i] = reachable(addr, timeout)
		}(i, d.DefaultAddr)
	}
	wg.Wait()
	for i, ok := range results {
		if ok {
			return &supportedDevices[i]
		}
	}
	return nil
}

// defaultGateway returns the IPv4 default gateway from /proc/net/route, or
// "" if no default route exists or we can't read the file (non-Linux).
func defaultGateway() string {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Scan() // header
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 || fields[1] != "00000000" {
			continue
		}
		gwHex := fields[2]
		if len(gwHex) != 8 {
			continue
		}
		// Gateway is little-endian 4 hex bytes.
		var ip [4]byte
		for i := 0; i < 4; i++ {
			b, err := strconv.ParseUint(gwHex[i*2:i*2+2], 16, 8)
			if err != nil {
				return ""
			}
			ip[3-i] = byte(b)
		}
		return net.IP(ip[:]).String()
	}
	return ""
}

func openDevice(d *SupportedDevice, flagAddr, flagPass string, debug bool) (Device, error) {
	addr := resolveAddr(d, flagAddr)
	pass := resolvePassword(d, flagPass)
	if pass == "" {
		return nil, fmt.Errorf("no password for %s (try env %s or %s/%s)",
			d.Title, d.PasswordEnvs[0], xdgConfigDir(), d.PasswordPath)
	}
	dev := d.New(addr, debug)
	if err := dev.Connect(pass); err != nil {
		return nil, err
	}
	return dev, nil
}

func supportedIDs() string {
	ids := make([]string, len(supportedDevices))
	for i, d := range supportedDevices {
		ids[i] = d.ID
	}
	return strings.Join(ids, ", ")
}
