package geoip

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func nolog(string, ...any) {}

func TestNormalize(t *testing.T) {
	c := Config{Countries: []string{" RU", "by", "ru", ""}, Source: DefaultSource}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.Countries, ",") != "ru,by" || c.Source != "" {
		t.Fatalf("got %+v", c)
	}
	for _, bad := range []Config{
		{Countries: []string{"rus"}},
		{Countries: []string{"r1"}},
		{Countries: []string{"zz"}},
		{Countries: []string{"xk"}},
		{Source: "ftp://example.com/{cc}"},
		{Source: "https://example.com/list.txt"},
	} {
		if err := bad.Normalize(); err == nil {
			t.Errorf("%+v: expected error", bad)
		}
	}
}

func TestParse(t *testing.T) {
	list, err := parse(strings.NewReader("# header\n1.2.3.0/24\n\n5.6.7.8 # host\n10.1.2.3/8\n"))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(list) != "[1.2.3.0/24 5.6.7.8/32 10.0.0.0/8]" {
		t.Fatalf("list = %v", list)
	}
	for _, bad := range []string{"", "# only comments\n", "<html>\n", "2001:db8::/32\n"} {
		if _, err := parse(strings.NewReader(bad)); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestMerge(t *testing.T) {
	p := func(s string) netip.Prefix { return netip.MustParsePrefix(s) }
	got := merge(
		[]netip.Prefix{p("10.0.0.0/8"), p("192.168.1.0/24")},
		[]netip.Prefix{p("10.1.0.0/16"), p("192.168.0.0/24"), p("192.168.1.0/24")},
	)
	if fmt.Sprint(got) != "[10.0.0.0/8 192.168.0.0/24 192.168.1.0/24]" {
		t.Fatalf("merge = %v", got)
	}
}

func TestStore(t *testing.T) {
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		switch r.URL.Path {
		case "/ru.zone":
			fmt.Fprint(w, "1.0.0.0/24\n2.0.0.0/16\n")
		case "/by.zone":
			fmt.Fprint(w, "3.0.0.0/24\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	s, err := Open(dir, nolog)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.SetConfig(ctx, Config{Countries: []string{"ru", "kz"}, Source: srv.URL + "/{cc}.zone"}); err != nil {
		t.Fatal(err)
	}
	st := s.Status()
	if len(st) != 2 || st[0].Prefixes != 2 || st[0].Error != "" || st[1].Error == "" {
		t.Fatalf("status = %+v", st)
	}

	// Adding a country downloads only that one.
	if err := s.SetConfig(ctx, Config{Countries: []string{"ru", "by"}, Source: srv.URL + "/{cc}.zone"}); err != nil {
		t.Fatal(err)
	}
	if hits["/ru.zone"] != 1 || hits["/by.zone"] != 1 {
		t.Fatalf("hits = %v", hits)
	}
	if got := fmt.Sprint(s.Subnets()); got != "[1.0.0.0/24 2.0.0.0/16 3.0.0.0/24]" {
		t.Fatalf("subnets = %v", got)
	}

	// The lists survive a restart without downloading again.
	s2, err := Open(dir, nolog)
	if err != nil {
		t.Fatal(err)
	}
	s2.Update(ctx, false)
	if len(s2.Subnets()) != 3 || hits["/ru.zone"] != 1 {
		t.Fatalf("reopen: %v, hits %v", s2.Subnets(), hits)
	}
	s2.Update(ctx, true)
	if hits["/ru.zone"] != 2 {
		t.Fatalf("forced update: hits %v", hits)
	}
}

func TestRSC(t *testing.T) {
	out := string(RSC("awg-geo", []string{"ru"}, []netip.Prefix{netip.MustParsePrefix("1.0.0.0/24")}))
	want := "/ip/firewall/address-list\nremove [find list=awg-geo]\nadd list=awg-geo address=1.0.0.0/24\n"
	if !strings.HasPrefix(out, "# amneziawg-mikrotik: RU, 1 subnets") || !strings.HasSuffix(out, want) {
		t.Fatalf("rsc = %q", out)
	}
}
