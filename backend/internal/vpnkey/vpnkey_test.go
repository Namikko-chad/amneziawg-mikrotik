package vpnkey

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

const clientConf = "[Interface]\nPrivateKey = a\nAddress = 10.8.1.2/32\nDNS = $PRIMARY_DNS, $SECONDARY_DNS\nJc = 4\n\n[Peer]\nPublicKey = b\nEndpoint = 1.2.3.4:443\nAllowedIPs = 0.0.0.0/0\n"

func qCompress(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, uint32(len(b)))
	buf.Write(hdr)
	w, _ := zlib.NewWriterLevel(&buf, 8)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAmneziaKey(t *testing.T) {
	last := string(mustJSON(t, map[string]any{"config": clientConf, "hostName": "1.2.3.4"}))
	key := map[string]any{
		"containers": []any{
			map[string]any{"container": "amnezia-openvpn"},
			map[string]any{"container": "amnezia-awg", "awg": map[string]any{"last_config": last, "port": "443"}},
		},
		"defaultContainer": "amnezia-awg",
		"description":      "My server",
		"dns1":             "8.8.8.8",
		"dns2":             "8.8.4.4",
		"hostName":         "1.2.3.4",
	}
	s := "vpn://" + base64.RawURLEncoding.EncodeToString(qCompress(t, mustJSON(t, key)))
	r, err := Decode(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Config, "DNS = 8.8.8.8, 8.8.4.4") || !strings.Contains(r.Config, "Jc = 4") {
		t.Fatalf("bad config:\n%s", r.Config)
	}
	if r.Name != "My server" {
		t.Fatalf("name = %q", r.Name)
	}
}

func TestUncompressedJSONKey(t *testing.T) {
	last := string(mustJSON(t, map[string]any{"config": clientConf}))
	key := map[string]any{"containers": []any{map[string]any{"container": "amnezia-awg", "awg": map[string]any{"last_config": last}}}}
	r, err := Decode("vpn://" + base64.URLEncoding.EncodeToString(mustJSON(t, key)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Config, "DNS = 1.1.1.1, 1.0.0.1") {
		t.Fatalf("default DNS not applied:\n%s", r.Config)
	}
}

func TestFullAccessKey(t *testing.T) {
	key := map[string]any{"containers": []any{map[string]any{"container": "amnezia-awg"}}, "hostName": "5.6.7.8", "port": 2222, "userName": "root", "password": "pw"}
	r, err := Decode("vpn://" + base64.RawURLEncoding.EncodeToString(qCompress(t, mustJSON(t, key))))
	if err != nil {
		t.Fatal(err)
	}
	if r.Server == nil || r.Server.Host != "5.6.7.8" || r.Server.Port != 2222 || r.Server.User != "root" {
		t.Fatalf("server = %+v", r.Server)
	}
}

func TestPlainBase64Conf(t *testing.T) {
	r, err := Decode(base64.StdEncoding.EncodeToString([]byte(clientConf)))
	if err != nil {
		t.Fatal(err)
	}
	if r.Config != clientConf {
		t.Fatalf("got %q", r.Config)
	}
}

func TestGarbage(t *testing.T) {
	if _, err := Decode("vpn://!!!"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := Decode(base64.StdEncoding.EncodeToString([]byte("hello"))); err == nil {
		t.Fatal("expected error")
	}
}
