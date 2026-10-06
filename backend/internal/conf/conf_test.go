package conf

import (
	"strings"
	"testing"
)

const sample = `[Interface]
# comment
PrivateKey = YUFhQWJCYkJjQ2NDZERkRGVFZUVmRmZGRmdHZ0doSGg=
Address = 10.8.1.2/32
DNS = 1.1.1.1, 1.0.0.1
Jc = 4
Jmin = 10
Jmax = 50
S1 = 12
H1 = 1234

[Peer]
PublicKey = cHViUFVCcHViUFVCcHViUFVCcHViUFVCcHViUFVCMTI=
PresharedKey = cHNrUFNLcHNrUFNLcHNrUFNLcHNrUFNLcHNrUFNLMTI=
AllowedIPs = 0.0.0.0/0, ::/0
Endpoint = 1.2.3.4:51820
PersistentKeepalive = 25
`

func TestRoundtrip(t *testing.T) {
	c, err := Parse(sample)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c2, err := Parse(c.String())
	if err != nil {
		t.Fatal(err)
	}
	if c.String() != c2.String() {
		t.Fatalf("roundtrip mismatch:\n%s\n---\n%s", c, c2)
	}
	if got := c.Interface().Get("jc"); got != "4" {
		t.Fatalf("Jc = %q", got)
	}
	if got := c.Peers()[0].All("AllowedIPs"); len(got) != 2 || got[1] != "::/0" {
		t.Fatalf("AllowedIPs = %v", got)
	}
}

func TestStrip(t *testing.T) {
	c, _ := Parse(sample)
	s := c.Strip()
	for _, k := range []string{"Address", "DNS"} {
		if strings.Contains(s, k+" =") {
			t.Fatalf("%s not stripped:\n%s", k, s)
		}
	}
	for _, k := range []string{"PrivateKey", "Jc", "H1", "Endpoint", "PresharedKey"} {
		if !strings.Contains(s, k+" =") {
			t.Fatalf("%s lost:\n%s", k, s)
		}
	}
}

func TestMasked(t *testing.T) {
	c, _ := Parse(sample)
	m := c.Masked()
	if strings.Contains(m, "YUFhQWJCYkJjQ2NDZERkRGVFZUVmRmZGRmdHZ0doSGg=") || strings.Contains(m, "cHNrUFNL") {
		t.Fatalf("secret leaked:\n%s", m)
	}
}

func TestValidateErrors(t *testing.T) {
	for _, bad := range []string{
		"",
		"[Interface]\nPrivateKey = x\n",
		"[Interface]\nPrivateKey = x\nAddress = 10.0.0.2/32\n",
		"[Interface]\nPrivateKey = x\nAddress = nope\n[Peer]\nPublicKey = y\nEndpoint = a:1\n",
		"[Interface]\nPrivateKey = c2hvcnQ=\nAddress = 10.0.0.2/32\n[Peer]\nPublicKey = c2hvcnQ=\nEndpoint = a:1\n",
	} {
		c, err := Parse(bad)
		if err == nil {
			err = c.Validate()
		}
		if err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}
