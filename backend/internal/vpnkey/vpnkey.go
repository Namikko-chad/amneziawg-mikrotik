// Package vpnkey decodes Amnezia share keys ("vpn://...") and plain base64-encoded configs.
//
// Amnezia format: "vpn://" + base64url(qCompress(json)), where qCompress is a 4-byte
// big-endian uncompressed length followed by a zlib stream. Older keys may be uncompressed JSON.
package vpnkey

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ServerCreds are present in "full access" keys exported by the Amnezia app.
type ServerCreds struct {
	Host     string
	Port     int
	User     string
	Password string
}

// Result is either a ready client config or server credentials to provision a client with.
type Result struct {
	Config string       // wg/awg config text
	Server *ServerCreds // set when the key carries SSH access instead of a client config
	Name   string       // description from the key, if any
}

type container struct {
	Container string          `json:"container"`
	AWG       json.RawMessage `json:"awg"`
}

type amneziaKey struct {
	Containers       []container `json:"containers"`
	DefaultContainer string      `json:"defaultContainer"`
	Description      string      `json:"description"`
	DNS1             string      `json:"dns1"`
	DNS2             string      `json:"dns2"`
	HostName         string      `json:"hostName"`
	Port             json.Number `json:"port"`
	UserName         string      `json:"userName"`
	Password         string      `json:"password"`
}

// Decode accepts "vpn://..." keys, base64 of an Amnezia JSON, or base64 of a .conf file.
func Decode(s string) (*Result, error) {
	s = strings.Join(strings.Fields(s), "")
	s = strings.TrimPrefix(s, "vpn://")
	if s == "" {
		return nil, errors.New("empty key")
	}
	raw, err := decodeB64(s)
	if err != nil {
		return nil, fmt.Errorf("base64: %w", err)
	}
	data := raw
	if u, err := qUncompress(raw); err == nil {
		data = u
	}
	if text := strings.TrimSpace(string(data)); !strings.HasPrefix(text, "{") && strings.Contains(text, "[Interface]") {
		return &Result{Config: string(data)}, nil
	}
	var k amneziaKey
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&k); err != nil {
		return nil, fmt.Errorf("not an Amnezia key: %w", err)
	}
	return fromKey(&k)
}

func fromKey(k *amneziaKey) (*Result, error) {
	var lastErr error
	// Prefer the default container, then any amnezia-awg* container.
	ordered := make([]container, 0, len(k.Containers))
	for _, c := range k.Containers {
		if c.Container == k.DefaultContainer {
			ordered = append([]container{c}, ordered...)
		} else {
			ordered = append(ordered, c)
		}
	}
	for _, c := range ordered {
		if !strings.HasPrefix(c.Container, "amnezia-awg") && !strings.HasPrefix(c.Container, "amnezia-wireguard") {
			continue
		}
		cfg, err := lastConfig(c.AWG)
		if err != nil {
			lastErr = err
			continue
		}
		if cfg == "" {
			continue
		}
		cfg = strings.NewReplacer("$PRIMARY_DNS", orDefault(k.DNS1, "1.1.1.1"), "$SECONDARY_DNS", orDefault(k.DNS2, "1.0.0.1")).Replace(cfg)
		return &Result{Config: cfg, Name: k.Description}, nil
	}
	if k.HostName != "" && k.UserName != "" {
		port := 22
		if p, err := strconv.Atoi(k.Port.String()); err == nil && p > 0 {
			port = p
		}
		return &Result{Server: &ServerCreds{Host: k.HostName, Port: port, User: k.UserName, Password: k.Password}, Name: k.Description}, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("key contains no AmneziaWG client config")
}

func lastConfig(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var awg struct {
		LastConfig string `json:"last_config"`
	}
	if err := json.Unmarshal(raw, &awg); err != nil {
		return "", fmt.Errorf("awg section: %w", err)
	}
	if awg.LastConfig == "" {
		return "", nil
	}
	var lc struct {
		Config string `json:"config"`
	}
	if err := json.Unmarshal([]byte(awg.LastConfig), &lc); err != nil {
		return "", fmt.Errorf("last_config: %w", err)
	}
	return lc.Config, nil
}

func qUncompress(b []byte) ([]byte, error) {
	if len(b) < 5 {
		return nil, errors.New("too short")
	}
	r, err := zlib.NewReader(bytes.NewReader(b[4:]))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(io.LimitReader(r, 4<<20))
}

func decodeB64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	// Tolerate mixed alphabets/padding.
	s = strings.NewReplacer("+", "-", "/", "_").Replace(strings.TrimRight(s, "="))
	return base64.RawURLEncoding.DecodeString(s)
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
