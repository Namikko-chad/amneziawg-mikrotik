package tunnel

import (
	"strings"
	"testing"

	"github.com/Namikko-chad/amneziawg-mikrotik/backend/internal/conf"
)

func TestParseDump(t *testing.T) {
	dump := "cHJpdg==\tcHVi\t0\t4\t10\t50\t57\t112\t0\t0\t11\t22\t33\t44\t<r 2><b 0x85>\t(null)\t(null)\t(null)\t(null)\t(none)\t0\t0\t0\t0\t0\t0\toff\toff\toff\n" +
		"U0VSVkVS\tcHNr\t203.0.113.5:41234\t0.0.0.0/0,::/0\t1759750000\t1024\t2048\t25\n"
	p := ParseDump(dump)
	if len(p) != 1 {
		t.Fatalf("peers = %d", len(p))
	}
	got := p[0]
	if got.Endpoint != "203.0.113.5:41234" || got.LastHandshake != 1759750000 || got.RxBytes != 1024 || got.TxBytes != 2048 {
		t.Fatalf("got %+v", got)
	}
	if len(ParseDump("")) != 0 {
		t.Fatal("expected no peers")
	}
}

func TestParseRouteDev(t *testing.T) {
	for in, want := range map[string]string{
		"default via 172.17.0.1 dev veth-awg \n":         "veth-awg",
		"default via 172.18.0.1 dev eth0 proto static\n": "eth0",
		"": "",
	} {
		if got := ParseRouteDev(in); got != want {
			t.Fatalf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestDecide(t *testing.T) {
	up := func(n int) health { return health{ok: n} }
	down := func(n int) health { return health{fail: n} }
	for _, c := range []struct {
		name            string
		using           int
		primary, backup health
		want            int
	}{
		{"primary healthy", 0, up(5), up(5), 0},
		{"primary flaps once", 0, down(1), up(5), 0},
		{"primary down", 0, down(failAfter), up(1), 1},
		{"both down stays", 0, down(failAfter), down(failAfter), 0},
		{"primary back too early", 1, up(failbackAfter - 1), up(5), 1},
		{"failback", 1, up(failbackAfter), up(5), 0},
		{"backup down, primary up", 1, up(1), down(failAfter), 0},
		{"backup down, primary down", 1, down(9), down(failAfter), 1},
	} {
		if got := decide(c.using, c.primary, c.backup); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestRoutes(t *testing.T) {
	c, err := conf.Parse("[Peer]\nAllowedIPs = 0.0.0.0/0, ::/0, 10.1.2.3/16\n[Peer]\nAllowedIPs = 10.1.0.0/16\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(Routes(c), " "); got != "default 10.1.0.0/16" {
		t.Fatalf("got %q", got)
	}
}
