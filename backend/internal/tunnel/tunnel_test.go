package tunnel

import "testing"

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
