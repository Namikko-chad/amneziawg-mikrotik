// Package tunnel brings the AmneziaWG interface up and down (a minimal awg-quick replacement
// that works inside a RouterOS container: no resolvconf, no sysctl-heavy fwmark routing).
//
// Routing model: the container's own traffic (including the tunnel's UDP and SSH provisioning)
// uses the main table via the LAN interface. Packets forwarded to the container by the router (iif LAN) are
// looked up in a dedicated table that routes AllowedIPs into awg0, and are masqueraded.
package tunnel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Namikko-chad/amneziawg-mikrotik/backend/internal/conf"
)

const (
	Iface      = "awg0"
	table      = "51820"
	rulePrio   = "10000"
	defaultMTU = "1376"
	sockPath   = "/var/run/amneziawg/" + Iface + ".sock"
)

type Logger interface {
	Printf(format string, args ...any)
	Write(p []byte) (int, error)
}

type Manager struct {
	mu       sync.Mutex
	log      Logger
	lanIface string
	proc     *exec.Cmd
	exited   chan struct{}
	ipt      string
	cleanup  [][]string // commands to undo on Down, in reverse order
}

// New creates a manager. An empty lanIface means "detect from the default route".
func New(log Logger, lanIface string) *Manager {
	return &Manager{log: log, lanIface: lanIface}
}

// Running reports whether amneziawg-go is alive.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running()
}

func (m *Manager) running() bool {
	if m.proc == nil {
		return false
	}
	select {
	case <-m.exited:
		return false
	default:
		return true
	}
}

// Up (re)starts the tunnel with cfg.
func (m *Manager) Up(c *conf.Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.down()
	if err := m.up(c); err != nil {
		m.log.Printf("tunnel: up failed: %v", err)
		m.down()
		return err
	}
	m.log.Printf("tunnel: %s is up", Iface)
	return nil
}

// Down stops the tunnel.
func (m *Manager) Down() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.down()
}

func (m *Manager) up(c *conf.Config) error {
	os.Remove(sockPath)
	cmd := exec.Command("amneziawg-go", "-f", Iface)
	cmd.Env = append(os.Environ(), "LOG_LEVEL=error", "WG_PROCESS_FOREGROUND=1")
	cmd.Stdout, cmd.Stderr = m.log, m.log
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start amneziawg-go: %w", err)
	}
	m.proc, m.exited = cmd, make(chan struct{})
	go func(exited chan struct{}) {
		err := cmd.Wait()
		m.log.Printf("amneziawg-go exited: %v", err)
		close(exited)
	}(m.exited)

	if err := m.waitSocket(5 * time.Second); err != nil {
		return err
	}

	tmp, err := os.CreateTemp("", "awg-*.conf")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	tmp.WriteString(c.Strip())
	tmp.Close()
	if err := m.run("awg", "setconf", Iface, tmp.Name()); err != nil {
		return err
	}

	iface := c.Interface()
	for _, a := range iface.All("Address") {
		if !strings.Contains(a, "/") {
			if strings.Contains(a, ":") {
				a += "/128"
			} else {
				a += "/32"
			}
		}
		fam := "-4"
		if strings.Contains(a, ":") {
			fam = "-6"
		}
		if err := m.run("ip", fam, "address", "add", a, "dev", Iface); err != nil {
			if fam == "-6" {
				m.log.Printf("tunnel: skipping IPv6 address: %v", err)
				continue
			}
			return err
		}
	}
	mtu := iface.Get("MTU")
	if mtu == "" {
		mtu = defaultMTU
	}
	if err := m.run("ip", "link", "set", "mtu", mtu, "up", "dev", Iface); err != nil {
		return err
	}
	return m.setupRouting(c)
}

func (m *Manager) setupRouting(c *conf.Config) error {
	lan := m.lanIface
	if lan == "" {
		var err error
		if lan, err = defaultRouteIface(); err != nil {
			return err
		}
		m.log.Printf("tunnel: LAN interface is %s", lan)
	}
	writeSysctl("net/ipv4/ip_forward", "1")
	for _, k := range []string{"all", "default", lan, Iface} {
		writeSysctl("net/ipv4/conf/"+k+"/rp_filter", "0")
	}

	var routes []string
	for _, p := range c.Peers() {
		for _, a := range p.All("AllowedIPs") {
			pp, err := netip.ParsePrefix(a)
			if err != nil || !pp.Addr().Is4() {
				continue
			}
			if pp.Bits() == 0 {
				routes = append(routes, "default")
			} else {
				routes = append(routes, pp.Masked().String())
			}
		}
	}
	if len(routes) == 0 {
		return errors.New("no IPv4 AllowedIPs in config")
	}
	for _, r := range routes {
		if err := m.run("ip", "-4", "route", "replace", r, "dev", Iface, "table", table); err != nil {
			return err
		}
	}
	m.cleanup = append(m.cleanup, []string{"ip", "-4", "route", "flush", "table", table})

	if err := m.run("ip", "-4", "rule", "add", "iif", lan, "lookup", table, "priority", rulePrio); err != nil {
		return err
	}
	m.cleanup = append(m.cleanup, []string{"ip", "-4", "rule", "del", "priority", rulePrio})

	ipt, err := m.iptables()
	if err != nil {
		return err
	}
	rules := [][]string{
		{"-t", "nat", "POSTROUTING", "-o", Iface, "-j", "MASQUERADE"},
		{"-t", "mangle", "FORWARD", "-o", Iface, "-p", "tcp", "--tcp-flags", "SYN,RST", "SYN", "-j", "TCPMSS", "--clamp-mss-to-pmtu"},
	}
	for _, r := range rules {
		add := append([]string{ipt, r[0], r[1], "-A", r[2]}, r[3:]...)
		if err := m.run(add[0], add[1:]...); err != nil {
			if r[1] == "mangle" {
				m.log.Printf("tunnel: MSS clamping unavailable: %v", err)
				continue
			}
			return err
		}
		del := append([]string{ipt, r[0], r[1], "-D", r[2]}, r[3:]...)
		m.cleanup = append(m.cleanup, del)
	}
	return nil
}

// iptables picks a working backend: RouterOS kernels may lack nf_tables.
func (m *Manager) iptables() (string, error) {
	if m.ipt != "" {
		return m.ipt, nil
	}
	for _, bin := range []string{"iptables-legacy", "iptables-nft", "iptables"} {
		if _, err := exec.LookPath(bin); err != nil {
			continue
		}
		if err := exec.Command(bin, "-t", "nat", "-S").Run(); err == nil {
			m.ipt = bin
			m.log.Printf("tunnel: using %s", bin)
			return bin, nil
		}
	}
	return "", errors.New("no working iptables backend (need nat table for masquerade)")
}

func (m *Manager) down() {
	for i := len(m.cleanup) - 1; i >= 0; i-- {
		c := m.cleanup[i]
		exec.Command(c[0], c[1:]...).Run()
	}
	m.cleanup = nil
	if m.running() {
		m.proc.Process.Signal(syscall.SIGTERM)
		select {
		case <-m.exited:
		case <-time.After(3 * time.Second):
			m.proc.Process.Kill()
			<-m.exited
		}
		m.log.Printf("tunnel: %s is down", Iface)
	}
	m.proc = nil
	// In case a previous instance left the link behind.
	exec.Command("ip", "link", "del", Iface).Run()
}

func (m *Manager) waitSocket(d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			return nil
		}
		select {
		case <-m.exited:
			return errors.New("amneziawg-go exited during startup (is /dev/net/tun available?)")
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("timeout waiting for amneziawg-go socket")
}

func (m *Manager) run(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return nil
}

// defaultRouteIface returns the interface of the IPv4 default route. Docker names it eth0,
// RouterOS names it after the veth (e.g. veth-awg).
func defaultRouteIface() (string, error) {
	out, err := exec.Command("ip", "-4", "route", "show", "default").Output()
	if err != nil {
		return "", fmt.Errorf("ip route show default: %w", err)
	}
	if dev := ParseRouteDev(string(out)); dev != "" {
		return dev, nil
	}
	return "", errors.New("no IPv4 default route in container; set LAN_IFACE")
}

// ParseRouteDev extracts the "dev X" value from `ip route` output.
func ParseRouteDev(s string) string {
	f := strings.Fields(s)
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "dev" {
			return f[i+1]
		}
	}
	return ""
}

func writeSysctl(key, val string) {
	os.WriteFile("/proc/sys/"+key, []byte(val), 0o644)
}

// PeerStatus is runtime info from `awg show dump`.
type PeerStatus struct {
	PublicKey     string `json:"public_key"`
	Endpoint      string `json:"endpoint"`
	AllowedIPs    string `json:"allowed_ips"`
	LastHandshake int64  `json:"last_handshake"` // unix seconds, 0 = never
	RxBytes       uint64 `json:"rx_bytes"`
	TxBytes       uint64 `json:"tx_bytes"`
}

// Status returns peers from `awg show awg0 dump`.
func (m *Manager) Status() ([]PeerStatus, error) {
	if !m.Running() {
		return nil, nil
	}
	out, err := exec.Command("awg", "show", Iface, "dump").Output()
	if err != nil {
		return nil, err
	}
	return ParseDump(string(out)), nil
}

// ParseDump parses peer lines (all but the first) of `awg show <iface> dump`.
// Fields are tab-separated; interface line has many AWG fields we ignore.
func ParseDump(s string) []PeerStatus {
	var peers []PeerStatus
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for _, l := range lines[min(1, len(lines)):] {
		f := strings.Split(l, "\t")
		if len(f) < 8 {
			continue
		}
		hs, _ := strconv.ParseInt(f[4], 10, 64)
		rx, _ := strconv.ParseUint(f[5], 10, 64)
		tx, _ := strconv.ParseUint(f[6], 10, 64)
		peers = append(peers, PeerStatus{PublicKey: f[0], Endpoint: f[2], AllowedIPs: f[3], LastHandshake: hs, RxBytes: rx, TxBytes: tx})
	}
	return peers
}
