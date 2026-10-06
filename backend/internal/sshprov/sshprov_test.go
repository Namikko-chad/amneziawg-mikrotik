package sshprov

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/curve25519"

	"github.com/Namikko-chad/amneziawg-mikrotik/backend/internal/conf"
)

const serverConf = `[Interface]
PrivateKey = c2VydmVy
Address = 10.8.1.0/24
ListenPort = 41234
Jc = 4
Jmin = 10
Jmax = 50
S1 = 57
S2 = 112
H1 = 11
H2 = 22
H3 = 33
H4 = 44

[Peer]
PublicKey = cDE=
PresharedKey = cHNr
AllowedIPs = 10.8.1.1/32

[Peer]
PublicKey = cDI=
PresharedKey = cHNr
AllowedIPs = 10.8.1.3/32
`

// fakeServer emulates the subset of shell commands provision() issues.
type fakeServer struct {
	files map[string]string
	cmds  []string
}

var (
	reExec   = regexp.MustCompile(`^docker exec -i amnezia-awg2 (?:sh|bash) -c '(.*)'$`)
	reCat    = regexp.MustCompile(`^cat (\S+)`)
	reWrite  = regexp.MustCompile(`^echo (\S+) \| base64 -d (>>?) (\S+)$`)
	reUnquot = strings.NewReplacer(`'\''`, `'`)
)

func (f *fakeServer) Run(_ context.Context, script string) (string, error) {
	f.cmds = append(f.cmds, script)
	if strings.HasPrefix(script, "docker ps") {
		return "amnezia-dns\namnezia-awg2\n", nil
	}
	m := reExec.FindStringSubmatch(script)
	if m == nil {
		return "", fmt.Errorf("unexpected: %s", script)
	}
	inner := reUnquot.Replace(m[1])
	if strings.Contains(inner, "syncconf") {
		return "", nil
	}
	if w := reWrite.FindStringSubmatch(inner); w != nil {
		b, _ := base64.StdEncoding.DecodeString(w[1])
		if w[2] == ">>" {
			f.files[w[3]] += string(b)
		} else {
			f.files[w[3]] = string(b)
		}
		return "", nil
	}
	if c := reCat.FindStringSubmatch(inner); c != nil {
		v, ok := f.files[c[1]]
		if !ok && !strings.Contains(inner, "|| true") {
			return "", fmt.Errorf("no such file %s", c[1])
		}
		return v, nil
	}
	return "", fmt.Errorf("unexpected inner: %s", inner)
}

func TestProvision(t *testing.T) {
	f := &fakeServer{files: map[string]string{
		awgDir + "/awg0.conf":                       serverConf,
		awgDir + "/wireguard_server_public_key.key": "U0VSVkVSUFVCU0VSVkVSUFVCU0VSVkVSUFVCU0VSVkU=\n",
		awgDir + "/wireguard_psk.key":               "cHNrUFNLcHNrUFNLcHNrUFNLcHNrUFNLcHNrUFNLMTI=\n",
	}}
	out, err := provision(context.Background(), f, Request{Host: "203.0.113.5", ClientName: "router"}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	c, err := conf.Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	ifc, peer := c.Interface(), c.Peers()[0]
	if got := ifc.Get("Address"); got != "10.8.1.2/32" {
		t.Fatalf("Address = %s", got)
	}
	for k, v := range map[string]string{"Jc": "4", "S2": "112", "H4": "44"} {
		if ifc.Get(k) != v {
			t.Fatalf("%s = %q", k, ifc.Get(k))
		}
	}
	if ifc.Get("ListenPort") != "" {
		t.Fatal("ListenPort copied to client")
	}
	if peer.Get("Endpoint") != "203.0.113.5:41234" || peer.Get("PublicKey") != "U0VSVkVSUFVCU0VSVkVSUFVCU0VSVkVSUFVCU0VSVkU=" || peer.Get("PresharedKey") != "cHNrUFNLcHNrUFNLcHNrUFNLcHNrUFNLcHNrUFNLMTI=" {
		t.Fatalf("bad peer:\n%s", out)
	}

	// Server config got the new peer with the matching public key.
	srv, err := conf.Parse(f.files[awgDir+"/awg0.conf"])
	if err != nil {
		t.Fatal(err)
	}
	peers := srv.Peers()
	if len(peers) != 3 || peers[2].Get("AllowedIPs") != "10.8.1.2/32" {
		t.Fatalf("server conf:\n%s", f.files[awgDir+"/awg0.conf"])
	}
	priv, _ := base64.StdEncoding.DecodeString(ifc.Get("PrivateKey"))
	pub, _ := curve25519.X25519(priv, curve25519.Basepoint)
	if peers[2].Get("PublicKey") != base64.StdEncoding.EncodeToString(pub) {
		t.Fatal("server peer public key does not match client private key")
	}

	var table []map[string]any
	if err := json.Unmarshal([]byte(f.files[awgDir+"/clientsTable"]), &table); err != nil {
		t.Fatal(err)
	}
	if len(table) != 1 || table[0]["clientId"] != peers[2].Get("PublicKey") ||
		table[0]["userData"].(map[string]any)["clientName"] != "router" {
		t.Fatalf("clientsTable = %v", table)
	}

	var synced bool
	for _, c := range f.cmds {
		if strings.Contains(c, "awg syncconf awg0 <(awg-quick strip /opt/amnezia/awg/awg0.conf)") {
			synced = true
		}
	}
	if !synced {
		t.Fatalf("syncconf not called: %v", f.cmds)
	}
}

func TestNextIPFull(t *testing.T) {
	c, _ := conf.Parse("[Interface]\nAddress = 10.0.0.1/30\n[Peer]\nAllowedIPs = 10.0.0.2/32\n")
	if _, err := NextIP(c); err == nil {
		t.Fatal("expected full subnet")
	}
}
