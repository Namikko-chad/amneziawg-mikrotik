// Package conf parses and serializes AmneziaWG / WireGuard INI-style configs.
package conf

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// KV is a single "Key = Value" line. Order is preserved.
type KV struct {
	Key   string
	Value string
}

// Section is an [Interface] or [Peer] block.
type Section struct {
	Name string
	Keys []KV
}

// Get returns the first value for key (case-insensitive).
func (s *Section) Get(key string) string {
	for _, kv := range s.Keys {
		if strings.EqualFold(kv.Key, key) {
			return kv.Value
		}
	}
	return ""
}

// All returns every value for key, splitting comma-separated lists.
func (s *Section) All(key string) []string {
	var out []string
	for _, kv := range s.Keys {
		if !strings.EqualFold(kv.Key, key) {
			continue
		}
		for _, v := range strings.Split(kv.Value, ",") {
			if v = strings.TrimSpace(v); v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

// Set replaces the first occurrence of key or appends it.
func (s *Section) Set(key, value string) {
	for i, kv := range s.Keys {
		if strings.EqualFold(kv.Key, key) {
			s.Keys[i].Value = value
			return
		}
	}
	s.Keys = append(s.Keys, KV{key, value})
}

// Config is a whole parsed file.
type Config struct {
	Sections []*Section
}

// Interface returns the [Interface] section or nil.
func (c *Config) Interface() *Section {
	for _, s := range c.Sections {
		if strings.EqualFold(s.Name, "Interface") {
			return s
		}
	}
	return nil
}

// Peers returns all [Peer] sections.
func (c *Config) Peers() []*Section {
	var out []*Section
	for _, s := range c.Sections {
		if strings.EqualFold(s.Name, "Peer") {
			out = append(out, s)
		}
	}
	return out
}

// Parse reads a config, dropping comments and blank lines.
func Parse(text string) (*Config, error) {
	c := &Config{}
	var cur *Section
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		l := sc.Text()
		if i := strings.IndexByte(l, '#'); i >= 0 {
			l = l[:i]
		}
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]") {
			cur = &Section{Name: strings.TrimSpace(l[1 : len(l)-1])}
			c.Sections = append(c.Sections, cur)
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected 'Key = Value'", line)
		}
		if cur == nil {
			return nil, fmt.Errorf("line %d: key outside of section", line)
		}
		cur.Keys = append(cur.Keys, KV{strings.TrimSpace(k), strings.TrimSpace(v)})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return c, nil
}

// String serializes the config back to text.
func (c *Config) String() string {
	var b strings.Builder
	for i, s := range c.Sections {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "[%s]\n", s.Name)
		for _, kv := range s.Keys {
			fmt.Fprintf(&b, "%s = %s\n", kv.Key, kv.Value)
		}
	}
	return b.String()
}

// quickKeys are handled by our own "awg-quick" logic and are not understood by `awg setconf`.
var quickKeys = map[string]bool{
	"address": true, "dns": true, "mtu": true, "table": true, "saveconfig": true,
	"preup": true, "postup": true, "predown": true, "postdown": true,
}

// Strip returns the config without wg-quick-only keys, suitable for `awg setconf`.
func (c *Config) Strip() string {
	out := &Config{}
	for _, s := range c.Sections {
		ns := &Section{Name: s.Name}
		for _, kv := range s.Keys {
			if strings.EqualFold(s.Name, "Interface") && quickKeys[strings.ToLower(kv.Key)] {
				continue
			}
			ns.Keys = append(ns.Keys, kv)
		}
		out.Sections = append(out.Sections, ns)
	}
	return out.String()
}

// Validate checks the minimum required for a client config.
func (c *Config) Validate() error {
	iface := c.Interface()
	if iface == nil {
		return errors.New("missing [Interface] section")
	}
	if err := checkKey("Interface.PrivateKey", iface.Get("PrivateKey")); err != nil {
		return err
	}
	addrs := iface.All("Address")
	if len(addrs) == 0 {
		return errors.New("missing Interface.Address")
	}
	for _, a := range addrs {
		if _, err := netip.ParsePrefix(a); err != nil {
			if _, err := netip.ParseAddr(a); err != nil {
				return fmt.Errorf("bad Address %q", a)
			}
		}
	}
	peers := c.Peers()
	if len(peers) == 0 {
		return errors.New("missing [Peer] section")
	}
	for _, p := range peers {
		if err := checkKey("Peer.PublicKey", p.Get("PublicKey")); err != nil {
			return err
		}
		if psk := p.Get("PresharedKey"); psk != "" {
			if err := checkKey("Peer.PresharedKey", psk); err != nil {
				return err
			}
		}
		if p.Get("Endpoint") == "" {
			return errors.New("missing Peer.Endpoint")
		}
	}
	return nil
}

func checkKey(name, v string) error {
	if v == "" {
		return fmt.Errorf("missing %s", name)
	}
	if b, err := base64.StdEncoding.DecodeString(v); err != nil || len(b) != 32 {
		return fmt.Errorf("%s is not a valid base64 32-byte key", name)
	}
	return nil
}

// Masked returns the config text with secrets hidden.
func (c *Config) Masked() string {
	out := &Config{}
	for _, s := range c.Sections {
		ns := &Section{Name: s.Name}
		for _, kv := range s.Keys {
			if strings.EqualFold(kv.Key, "PrivateKey") || strings.EqualFold(kv.Key, "PresharedKey") {
				kv.Value = "(hidden)"
			}
			ns.Keys = append(ns.Keys, kv)
		}
		out.Sections = append(out.Sections, ns)
	}
	return out.String()
}
