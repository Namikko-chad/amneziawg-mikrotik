// Package tunnel brings the AmneziaWG interface up and down (a minimal awg-quick replacement
// that works inside a RouterOS container: no resolvconf, no sysctl-heavy fwmark routing).
//
// Routing model: the container's own traffic (including the tunnel's UDP and SSH provisioning)
// uses the main table via the LAN interface. Packets forwarded to the container by the router (iif LAN) are
// looked up in a dedicated table that routes AllowedIPs into the link in use, and are masqueraded.
//
// Failover: with a backup config, a second instance runs on awg1. Both links are pinged through
// periodically; the forwarding table is pointed at the backup while the primary is failing and
// moved back once the primary has been healthy for a while.
package tunnel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Namikko-chad/amneziawg-mikrotik/backend/internal/conf"
)

const (
	Iface       = "awg0" // primary link
	BackupIface = "awg1" // backup link
	table       = "51820"
	rulePrio    = "10000"
	defaultMTU  = "1376"
	sockDir     = "/var/run/amneziawg/"
	// ProbeAddr answers pings only while a link is up, so the router's check-gateway can fall
	// back to the ISP when the tunnel goes down but the container keeps running.
	ProbeAddr = "198.18.0.1"

	checkInterval = 5 * time.Second
	failAfter     = 3  // failed checks in a row before the link in use is abandoned
	failbackAfter = 12 // successful checks in a row before going back to the primary link
)

var ifaces = [2]string{Iface, BackupIface}

type Logger interface {
	Printf(format string, args ...any)
	Write(p []byte) (int, error)
}

// link is one running amneziawg-go instance.
type link struct {
	iface   string
	proc    *exec.Cmd
	exited  chan struct{}
	routes  []string   // AllowedIPs as routes for the forwarding table
	cleanup [][]string // commands to undo on stop, in reverse order
	health  health
	checked bool // at least one health check has run
}

func (l *link) running() bool {
	if l == nil || l.proc == nil {
		return false
	}
	select {
	case <-l.exited:
		return false
	default:
		return true
	}
}

type Manager struct {
	mu         sync.Mutex
	log        Logger
	lanIface   string
	checkHosts []string
	ipt        string
	links      [2]*link   // primary, backup (either may be nil)
	using      int        // index of the link forwarded traffic goes to, -1 if none
	cleanup    [][]string // shared routing state to undo on Down
	stop       chan struct{}
}

// New creates a manager. An empty lanIface means "detect from the default route". checkHosts
// are pinged through each link to detect failures when a backup link is configured.
func New(log Logger, lanIface string, checkHosts []string) *Manager {
	return &Manager{log: log, lanIface: lanIface, checkHosts: checkHosts, using: -1}
}

// Running reports whether any amneziawg-go instance is alive.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.links[0].running() || m.links[1].running()
}

// Up (re)starts the tunnel. backup may be nil. If both are given, forwarded traffic goes
// through the primary link and moves to the backup while the primary is failing.
//
// If only the primary fails to start, the backup keeps running and the error is returned.
func (m *Manager) Up(primary, backup *conf.Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.down()
	err := m.up(primary, backup)
	if err != nil {
		m.log.Printf("tunnel: %v", err)
	}
	if m.using < 0 {
		m.down()
		if err == nil {
			err = errors.New("no link came up")
		}
	}
	return err
}

// Down stops the tunnel.
func (m *Manager) Down() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.down()
}

func (m *Manager) up(primary, backup *conf.Config) error {
	lan := m.lanIface
	if lan == "" {
		var err error
		if lan, err = defaultRouteIface(); err != nil {
			return err
		}
		m.log.Printf("tunnel: LAN interface is %s", lan)
	}
	writeSysctl("net/ipv4/ip_forward", "1")
	for _, k := range []string{"all", "default", lan} {
		writeSysctl("net/ipv4/conf/"+k+"/rp_filter", "0")
	}
	if _, err := m.iptables(); err != nil {
		return err
	}
	m.cleanup = append(m.cleanup, []string{"ip", "-4", "route", "flush", "table", table})
	if err := m.run("ip", "-4", "rule", "add", "iif", lan, "lookup", table, "priority", rulePrio); err != nil {
		return err
	}
	m.cleanup = append(m.cleanup, []string{"ip", "-4", "rule", "del", "priority", rulePrio})

	var errs [2]error
	for i, c := range []*conf.Config{primary, backup} {
		if c == nil {
			continue
		}
		l := &link{iface: ifaces[i]}
		m.links[i] = l
		if errs[i] = m.startLink(l, c, i); errs[i] != nil {
			m.stopLink(l)
			m.links[i] = nil
			continue
		}
		m.log.Printf("tunnel: %s is up", l.iface)
	}
	if errs[1] != nil {
		m.log.Printf("tunnel: backup link failed: %v", errs[1])
	}
	for i, l := range m.links {
		if l != nil {
			if err := m.use(i); err != nil {
				return err
			}
			break
		}
	}
	if m.links[0] != nil && m.links[1] != nil {
		m.stop = make(chan struct{})
		go m.monitor(m.stop, m.links)
	}
	if errs[0] != nil {
		if m.using == 1 {
			return fmt.Errorf("primary link failed, using backup: %w", errs[0])
		}
		return errs[0]
	}
	return errs[1]
}

func (m *Manager) startLink(l *link, c *conf.Config, idx int) error {
	sock := sockDir + l.iface + ".sock"
	os.Remove(sock)
	cmd := exec.Command("amneziawg-go", "-f", l.iface)
	cmd.Env = append(os.Environ(), "LOG_LEVEL=error", "WG_PROCESS_FOREGROUND=1")
	cmd.Stdout, cmd.Stderr = m.log, m.log
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start amneziawg-go: %w", err)
	}
	l.proc, l.exited = cmd, make(chan struct{})
	go func(iface string, exited chan struct{}) {
		err := cmd.Wait()
		m.log.Printf("amneziawg-go (%s) exited: %v", iface, err)
		close(exited)
	}(l.iface, l.exited)

	if err := waitSocket(sock, l.exited, 5*time.Second); err != nil {
		return err
	}

	tmp, err := os.CreateTemp("", "awg-*.conf")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	tmp.WriteString(c.Strip())
	tmp.Close()
	if err := m.run("awg", "setconf", l.iface, tmp.Name()); err != nil {
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
		if err := m.run("ip", fam, "address", "add", a, "dev", l.iface); err != nil {
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
	if err := m.run("ip", "link", "set", "mtu", mtu, "up", "dev", l.iface); err != nil {
		return err
	}
	writeSysctl("net/ipv4/conf/"+l.iface+"/rp_filter", "0")
	// Lives on the link, so it disappears with it.
	if err := m.run("ip", "-4", "address", "add", ProbeAddr+"/32", "dev", l.iface); err != nil {
		return err
	}

	if l.routes = Routes(c); len(l.routes) == 0 {
		return errors.New("no IPv4 AllowedIPs in config")
	}

	// Sockets bound to the interface (health checks) route through it even while forwarded
	// traffic uses the other link.
	ownTable, ownPrio := strconv.Itoa(51821+idx), strconv.Itoa(10001+idx)
	if err := m.run("ip", "-4", "route", "replace", "default", "dev", l.iface, "table", ownTable); err != nil {
		return err
	}
	l.cleanup = append(l.cleanup, []string{"ip", "-4", "route", "flush", "table", ownTable})
	if err := m.run("ip", "-4", "rule", "add", "oif", l.iface, "lookup", ownTable, "priority", ownPrio); err != nil {
		return err
	}
	l.cleanup = append(l.cleanup, []string{"ip", "-4", "rule", "del", "priority", ownPrio})

	rules := [][]string{
		{"-t", "nat", "POSTROUTING", "-o", l.iface, "-j", "MASQUERADE"},
		{"-t", "mangle", "FORWARD", "-o", l.iface, "-p", "tcp", "--tcp-flags", "SYN,RST", "SYN", "-j", "TCPMSS", "--clamp-mss-to-pmtu"},
	}
	for _, r := range rules {
		add := append([]string{m.ipt, r[0], r[1], "-A", r[2]}, r[3:]...)
		if err := m.run(add[0], add[1:]...); err != nil {
			if r[1] == "mangle" {
				m.log.Printf("tunnel: MSS clamping unavailable: %v", err)
				continue
			}
			return err
		}
		del := append([]string{m.ipt, r[0], r[1], "-D", r[2]}, r[3:]...)
		l.cleanup = append(l.cleanup, del)
	}
	return nil
}

func (m *Manager) stopLink(l *link) {
	for i := len(l.cleanup) - 1; i >= 0; i-- {
		c := l.cleanup[i]
		exec.Command(c[0], c[1:]...).Run()
	}
	l.cleanup = nil
	if l.running() {
		l.proc.Process.Signal(syscall.SIGTERM)
		select {
		case <-l.exited:
		case <-time.After(3 * time.Second):
			l.proc.Process.Kill()
			<-l.exited
		}
		m.log.Printf("tunnel: %s is down", l.iface)
	}
	exec.Command("ip", "link", "del", l.iface).Run()
}

// use points the forwarding table at link i. `route replace` swaps each route atomically,
// so forwarded traffic is not dropped during the switch.
func (m *Manager) use(i int) error {
	l := m.links[i]
	for _, r := range l.routes {
		if err := m.run("ip", "-4", "route", "replace", r, "dev", l.iface, "table", table); err != nil {
			return err
		}
	}
	if m.using >= 0 && m.using != i && m.links[m.using] != nil {
		for _, r := range m.links[m.using].routes {
			if !slices.Contains(l.routes, r) {
				exec.Command("ip", "-4", "route", "del", r, "table", table).Run()
			}
		}
	}
	m.using = i
	return nil
}

// Routes converts a config's IPv4 AllowedIPs into `ip route` destinations.
func Routes(c *conf.Config) []string {
	var routes []string
	for _, p := range c.Peers() {
		for _, a := range p.All("AllowedIPs") {
			pp, err := netip.ParsePrefix(a)
			if err != nil || !pp.Addr().Is4() {
				continue
			}
			r := pp.Masked().String()
			if pp.Bits() == 0 {
				r = "default"
			}
			if !slices.Contains(routes, r) {
				routes = append(routes, r)
			}
		}
	}
	return routes
}

// health counts consecutive check results of a link.
type health struct{ ok, fail int }

func (h *health) record(up bool) {
	if up {
		h.ok, h.fail = h.ok+1, 0
	} else {
		h.ok, h.fail = 0, h.fail+1
	}
}

// decide returns which link (0 primary, 1 backup) forwarded traffic should use.
func decide(using int, primary, backup health) int {
	switch using {
	case 0:
		if primary.fail >= failAfter && backup.ok > 0 {
			return 1
		}
	case 1:
		if primary.ok >= failbackAfter || (backup.fail >= failAfter && primary.ok > 0) {
			return 0
		}
	}
	return using
}

// monitor checks both links periodically and switches the forwarding table between them.
func (m *Manager) monitor(stop chan struct{}, links [2]*link) {
	t := time.NewTicker(checkInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		var ok [2]bool
		var wg sync.WaitGroup
		for i, l := range links {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok[i] = l.running() && m.check(l.iface)
			}()
		}
		wg.Wait()

		m.mu.Lock()
		select {
		case <-stop: // Down/Up ran while we were checking
			m.mu.Unlock()
			return
		default:
		}
		for i, l := range links {
			if l.checked && l.health.ok > 0 && !ok[i] {
				m.log.Printf("tunnel: %s check failed", l.iface)
			}
			l.health.record(ok[i])
			l.checked = true
		}
		if next := decide(m.using, links[0].health, links[1].health); next != m.using {
			if next == 1 {
				m.log.Printf("tunnel: primary link is down, switching to backup")
			} else {
				m.log.Printf("tunnel: switching back to primary link")
			}
			if err := m.use(next); err != nil {
				m.log.Printf("tunnel: switch failed: %v", err)
			}
		}
		m.mu.Unlock()
	}
}

// check pings the check hosts through iface; any reply counts as up.
func (m *Manager) check(iface string) bool {
	for _, h := range m.checkHosts {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := exec.CommandContext(ctx, "ping", "-c", "1", "-W", "2", "-I", iface, h).Run()
		cancel()
		if err == nil {
			return true
		}
	}
	return false
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
	if m.stop != nil {
		close(m.stop)
		m.stop = nil
	}
	for i, l := range m.links {
		if l != nil {
			m.stopLink(l)
			m.links[i] = nil
		}
	}
	for i := len(m.cleanup) - 1; i >= 0; i-- {
		c := m.cleanup[i]
		exec.Command(c[0], c[1:]...).Run()
	}
	m.cleanup = nil
	m.using = -1
	// In case a previous instance left links behind.
	for _, name := range ifaces {
		exec.Command("ip", "link", "del", name).Run()
	}
}

func waitSocket(path string, exited chan struct{}, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-exited:
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

// LinkStatus describes one link. Check is "ok", "fail" or "" (not checked: no backup configured).
type LinkStatus struct {
	Iface   string       `json:"iface"`
	Backup  bool         `json:"backup"`
	Running bool         `json:"running"`
	InUse   bool         `json:"in_use"`
	Check   string       `json:"check"`
	Peers   []PeerStatus `json:"peers"`
	Error   string       `json:"error,omitempty"`
}

// Status returns the configured links, primary first.
func (m *Manager) Status() []LinkStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []LinkStatus
	for i, l := range m.links {
		if l == nil {
			continue
		}
		ls := LinkStatus{Iface: l.iface, Backup: i == 1, Running: l.running(), InUse: i == m.using}
		if l.checked {
			ls.Check = "fail"
			if l.health.ok > 0 {
				ls.Check = "ok"
			}
		}
		if ls.Running {
			out, err := exec.Command("awg", "show", l.iface, "dump").Output()
			if err != nil {
				ls.Error = err.Error()
			} else {
				ls.Peers = ParseDump(string(out))
			}
		}
		out = append(out, ls)
	}
	return out
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
