// Package sshprov creates a new AmneziaWG client on a server set up by the Amnezia app,
// mirroring what the app itself does (WireguardConfigurator::prepareWireguardConfig).
package sshprov

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/ssh"

	"github.com/Namikko-chad/amneziawg-mikrotik/backend/internal/conf"
)

// Request holds SSH credentials. They are never persisted.
type Request struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	User       string `json:"user"`
	Password   string `json:"password"`
	PrivateKey string `json:"private_key"`
	Passphrase string `json:"passphrase"`
	ClientName string `json:"client_name"`
}

// Runner executes a shell script on the server and returns stdout.
type Runner interface {
	Run(ctx context.Context, script string) (string, error)
}

type flavor struct {
	container, bin, iface, confPath string
}

const awgDir = "/opt/amnezia/awg"

var flavors = []flavor{
	{"amnezia-awg2", "awg", "awg0", awgDir + "/awg0.conf"},
	{"amnezia-awg", "wg", "wg0", awgDir + "/wg0.conf"},
}

// Provision connects over SSH and returns a ready client config text.
func Provision(ctx context.Context, req Request, logf func(string, ...any)) (string, error) {
	if req.Host == "" || req.User == "" {
		return "", errors.New("host and user are required")
	}
	if req.Password == "" && req.PrivateKey == "" {
		return "", errors.New("password or private key is required")
	}
	if req.Port == 0 {
		req.Port = 22
	}
	r, err := dial(ctx, req)
	if err != nil {
		return "", err
	}
	defer r.Close()
	logf("ssh: connected to %s@%s:%d", req.User, req.Host, req.Port)
	return provision(ctx, r, req, logf)
}

func provision(ctx context.Context, r Runner, req Request, logf func(string, ...any)) (string, error) {
	names, err := r.Run(ctx, "docker ps --format '{{.Names}}'")
	if err != nil {
		return "", fmt.Errorf("docker ps: %w", err)
	}
	running := map[string]bool{}
	for _, n := range strings.Fields(names) {
		running[n] = true
	}
	var fl *flavor
	for i := range flavors {
		if running[flavors[i].container] {
			fl = &flavors[i]
			break
		}
	}
	if fl == nil {
		return "", errors.New("no running amnezia-awg / amnezia-awg2 container found on the server")
	}
	logf("ssh: using container %s (%s)", fl.container, fl.confPath)

	exec := func(script string) string {
		return fmt.Sprintf("docker exec -i %s sh -c %s", fl.container, shq(script))
	}
	serverText, err := r.Run(ctx, exec("cat "+fl.confPath))
	if err != nil {
		return "", fmt.Errorf("read server config: %w", err)
	}
	serverConf, err := conf.Parse(serverText)
	if err != nil {
		return "", fmt.Errorf("parse server config: %w", err)
	}
	srvIface := serverConf.Interface()
	if srvIface == nil {
		return "", errors.New("server config has no [Interface]")
	}
	pub, err := r.Run(ctx, exec("cat "+awgDir+"/wireguard_server_public_key.key"))
	if err != nil {
		return "", fmt.Errorf("read server public key: %w", err)
	}
	psk, err := r.Run(ctx, exec("cat "+awgDir+"/wireguard_psk.key"))
	if err != nil {
		return "", fmt.Errorf("read psk: %w", err)
	}
	pub, psk = strings.TrimSpace(pub), strings.TrimSpace(psk)

	ip, err := NextIP(serverConf)
	if err != nil {
		return "", err
	}
	priv, cpub, err := GenKeys()
	if err != nil {
		return "", err
	}
	logf("ssh: allocating %s for new client", ip)

	peer := fmt.Sprintf("\n[Peer]\nPublicKey = %s\nPresharedKey = %s\nAllowedIPs = %s/32\n", cpub, psk, ip)
	if _, err := r.Run(ctx, exec(fmt.Sprintf("echo %s | base64 -d >> %s", b64(peer), fl.confPath))); err != nil {
		return "", fmt.Errorf("append peer: %w", err)
	}
	sync := fmt.Sprintf("docker exec -i %s bash -c %s", fl.container,
		shq(fmt.Sprintf("%s syncconf %s <(%s-quick strip %s)", fl.bin, fl.iface, fl.bin, fl.confPath)))
	if _, err := r.Run(ctx, sync); err != nil {
		return "", fmt.Errorf("syncconf: %w", err)
	}
	if err := addToClientsTable(ctx, r, exec, cpub, req.ClientName); err != nil {
		logf("ssh: warning: clientsTable not updated: %v", err)
	}

	port := srvIface.Get("ListenPort")
	if port == "" {
		return "", errors.New("server config has no ListenPort")
	}
	return BuildClient(srvIface, ClientParams{
		PrivateKey: priv, Address: ip.String() + "/32",
		ServerPub: pub, PSK: psk, Endpoint: net.JoinHostPort(req.Host, port),
	}).String(), nil
}

func addToClientsTable(ctx context.Context, r Runner, exec func(string) string, clientID, name string) error {
	path := awgDir + "/clientsTable"
	raw, _ := r.Run(ctx, exec("cat "+path+" 2>/dev/null || true"))
	var table []map[string]any
	if s := strings.TrimSpace(raw); s != "" && strings.HasPrefix(s, "[") {
		if err := json.Unmarshal([]byte(s), &table); err != nil {
			return fmt.Errorf("parse: %w", err)
		}
	}
	if name == "" {
		name = "MikroTik " + time.Now().Format("2006-01-02 15:04")
	}
	table = append(table, map[string]any{
		"clientId": clientID,
		"userData": map[string]any{"clientName": name, "creationDate": time.Now().Format(time.RFC1123)},
	})
	b, _ := json.MarshalIndent(table, "", "    ")
	_, err := r.Run(ctx, exec(fmt.Sprintf("echo %s | base64 -d > %s", b64(string(b)), path)))
	return err
}

// ClientParams are the per-client values for BuildClient.
type ClientParams struct {
	PrivateKey, Address, ServerPub, PSK, Endpoint string
}

// serverOnly keys from the server [Interface] that must not be copied to the client.
var serverOnly = map[string]bool{
	"privatekey": true, "address": true, "listenport": true, "dns": true, "mtu": true, "table": true,
	"saveconfig": true, "preup": true, "postup": true, "predown": true, "postdown": true, "fwmark": true,
}

// BuildClient creates a client config, copying obfuscation parameters (Jc, S1, H1, ...) from the server.
func BuildClient(srv *conf.Section, p ClientParams) *conf.Config {
	iface := &conf.Section{Name: "Interface", Keys: []conf.KV{
		{Key: "Address", Value: p.Address},
		{Key: "DNS", Value: "1.1.1.1, 1.0.0.1"},
		{Key: "PrivateKey", Value: p.PrivateKey},
	}}
	for _, kv := range srv.Keys {
		if !serverOnly[strings.ToLower(kv.Key)] {
			iface.Keys = append(iface.Keys, kv)
		}
	}
	peer := &conf.Section{Name: "Peer", Keys: []conf.KV{
		{Key: "PublicKey", Value: p.ServerPub},
		{Key: "PresharedKey", Value: p.PSK},
		{Key: "AllowedIPs", Value: "0.0.0.0/0"},
		{Key: "Endpoint", Value: p.Endpoint},
		{Key: "PersistentKeepalive", Value: "25"},
	}}
	if p.PSK == "" {
		peer.Keys = append(peer.Keys[:1], peer.Keys[2:]...)
	}
	return &conf.Config{Sections: []*conf.Section{iface, peer}}
}

// NextIP returns the first free IPv4 address in the server's subnet.
func NextIP(c *conf.Config) (netip.Addr, error) {
	iface := c.Interface()
	var prefix netip.Prefix
	used := map[netip.Addr]bool{}
	for _, a := range iface.All("Address") {
		p, err := netip.ParsePrefix(a)
		if err == nil && p.Addr().Is4() {
			prefix = p.Masked()
			used[p.Addr()] = true
			break
		}
	}
	if !prefix.IsValid() {
		return netip.Addr{}, errors.New("server config has no IPv4 Address")
	}
	for _, p := range c.Peers() {
		for _, a := range p.All("AllowedIPs") {
			if pp, err := netip.ParsePrefix(a); err == nil {
				used[pp.Addr()] = true
			}
		}
	}
	last := lastAddr(prefix)
	for a := prefix.Addr().Next(); a.IsValid() && a.Less(last); a = a.Next() {
		if !used[a] {
			return a, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("subnet %s is full", prefix)
}

func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Addr().As4()
	host := 32 - p.Bits()
	for i := 3; i >= 0 && host > 0; i-- {
		n := min(host, 8)
		b[i] |= byte(1<<n - 1)
		host -= n
	}
	return netip.AddrFrom4(b)
}

// GenKeys returns a base64 X25519 private/public key pair.
func GenKeys() (priv, pub string, err error) {
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		return "", "", err
	}
	k[0] &= 248
	k[31] = (k[31] & 127) | 64
	p, err := curve25519.X25519(k[:], curve25519.Basepoint)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(k[:]), base64.StdEncoding.EncodeToString(p), nil
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// shq single-quotes s for POSIX sh.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// sshRunner runs scripts over SSH, escalating via sudo for non-root users.
type sshRunner struct {
	client   *ssh.Client
	sudo     bool
	password string
}

func dial(ctx context.Context, req Request) (*sshRunner, error) {
	var auth []ssh.AuthMethod
	if req.PrivateKey != "" {
		var signer ssh.Signer
		var err error
		if req.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(req.PrivateKey), []byte(req.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(req.PrivateKey))
		}
		if err != nil {
			return nil, fmt.Errorf("private key: %w", err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if req.Password != "" {
		auth = append(auth, ssh.Password(req.Password),
			ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
				ans := make([]string, len(qs))
				for i := range ans {
					ans[i] = req.Password
				}
				return ans, nil
			}))
	}
	cfg := &ssh.ClientConfig{
		User: req.User,
		Auth: auth,
		// The server is entered by hand in the UI; there is no known_hosts store in the container.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}
	addr := net.JoinHostPort(req.Host, strconv.Itoa(req.Port))
	d := net.Dialer{Timeout: 15 * time.Second}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", addr, err)
	}
	cc, chans, reqs, err := ssh.NewClientConn(nc, addr, cfg)
	if err != nil {
		nc.Close()
		if strings.Contains(err.Error(), "unable to authenticate") || strings.Contains(err.Error(), "unexpected message type 51") {
			return nil, errors.New("authentication failed: check user, password or key")
		}
		return nil, fmt.Errorf("ssh handshake: %w", err)
	}
	return &sshRunner{client: ssh.NewClient(cc, chans, reqs), sudo: req.User != "root", password: req.Password}, nil
}

func (s *sshRunner) Close() error { return s.client.Close() }

func (s *sshRunner) Run(ctx context.Context, script string) (string, error) {
	sess, err := s.client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	cmd := "sh -c " + shq(script)
	if s.sudo {
		if s.password != "" {
			// sudo reads the password from stdin; the script itself needs no stdin.
			cmd = "sudo -S -p '' " + cmd
			sess.Stdin = strings.NewReader(s.password + "\n")
		} else {
			cmd = "sudo -n " + cmd
		}
	}
	var stdout, stderr bytes.Buffer
	sess.Stdout, sess.Stderr = &stdout, &stderr
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case <-ctx.Done():
		sess.Signal(ssh.SIGKILL)
		return "", ctx.Err()
	case err := <-done:
		if err != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = err.Error()
			}
			return stdout.String(), errors.New(msg)
		}
		return stdout.String(), nil
	}
}
