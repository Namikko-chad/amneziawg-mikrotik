package routing

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/Namikko-chad/amneziawg-mikrotik/backend/internal/routeros"
)

// fakeRouter keeps ordered menus in memory and honours place-before.
type fakeRouter struct {
	menus  map[string][]routeros.Item
	single map[string]routeros.Item
	next   int
	adds   int
	rms    int
	execs  []string
}

func newFake() *fakeRouter {
	return &fakeRouter{
		menus: map[string][]routeros.Item{},
		single: map[string]routeros.Item{
			"ip/dns":          {"servers": "8.8.8.8"},
			"system/resource": {"version": "7.20", "board-name": "hAP ax3"},
			"system/identity": {"name": "home"},
		},
	}
}

func (f *fakeRouter) seed(path string, items ...routeros.Item) {
	for _, it := range items {
		f.next++
		it[".id"] = fmt.Sprintf("*%X", f.next)
		f.menus[path] = append(f.menus[path], it)
	}
}

func (f *fakeRouter) List(_ context.Context, path string, filter routeros.Item) ([]routeros.Item, error) {
	var out []routeros.Item
	for _, it := range f.menus[path] {
		ok := true
		for k, v := range filter {
			ok = ok && it[k] == v
		}
		if ok {
			out = append(out, clone(it))
		}
	}
	return out, nil
}

func (f *fakeRouter) Get(_ context.Context, path string) (routeros.Item, error) {
	return clone(f.single[path]), nil
}

func (f *fakeRouter) Add(_ context.Context, path string, props routeros.Item) (string, error) {
	f.next++
	f.adds++
	it := clone(props)
	it[".id"] = fmt.Sprintf("*%X", f.next)
	before := it["place-before"]
	delete(it, "place-before")
	for k, v := range it {
		if v == "yes" {
			it[k] = "true"
		} else if v == "no" {
			it[k] = "false"
		}
	}
	m := f.menus[path]
	i := slices.IndexFunc(m, func(x routeros.Item) bool { return x.ID() == before })
	if before == "" || i < 0 {
		f.menus[path] = append(m, it)
	} else {
		f.menus[path] = slices.Insert(m, i, it)
	}
	return it.ID(), nil
}

func (f *fakeRouter) Set(_ context.Context, path, id string, props routeros.Item) error {
	for _, it := range f.menus[path] {
		if it.ID() == id {
			for k, v := range props {
				it[k] = v
			}
			return nil
		}
	}
	return &routeros.Error{Status: 404}
}

func (f *fakeRouter) Remove(_ context.Context, path, id string) error {
	f.rms++
	m := f.menus[path]
	i := slices.IndexFunc(m, func(x routeros.Item) bool { return x.ID() == id })
	if i < 0 {
		return &routeros.Error{Status: 404}
	}
	f.menus[path] = slices.Delete(m, i, i+1)
	return nil
}

func (f *fakeRouter) Run(_ context.Context, path, cmd string, args routeros.Item) error {
	if path == "" && cmd == "execute" {
		f.execs = append(f.execs, args["script"])
		return nil
	}
	if cmd == "set" {
		for k, v := range args {
			f.single[path][k] = v
		}
	}
	return nil
}

func (f *fakeRouter) comments(path string) []string {
	var out []string
	for _, it := range f.menus[path] {
		out = append(out, it["comment"])
	}
	return out
}

var env = Env{
	Container:    netip.MustParseAddr("172.18.0.2"),
	ContainerNet: netip.MustParsePrefix("172.18.0.0/30"),
	Probe:        netip.MustParseAddr("198.18.0.1"),
}

func nolog(string, ...any) {}

func TestNormalize(t *testing.T) {
	p := Plan{Mode: All, LAN: []string{" 192.168.88.1/24 ", ""}, Exclude: []string{"192.168.88.50", "192.168.88.50/32"}}
	if err := p.Normalize(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.LAN, []string{"192.168.88.0/24"}) || !slices.Equal(p.Exclude, []string{"192.168.88.50/32"}) {
		t.Fatalf("got %+v", p)
	}

	p = Plan{Mode: Sites, LAN: []string{"192.168.88.0/24"}, Sites: []string{"YouTube.com.", "203.0.113.0/24"}}
	if err := p.Normalize(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Sites, []string{"youtube.com", "203.0.113.0/24"}) {
		t.Fatalf("sites = %v", p.Sites)
	}

	for _, bad := range []Plan{
		{Mode: "nope"},
		{Mode: All},
		{Mode: Devices, LAN: []string{"192.168.88.0/24"}},
		{Mode: All, LAN: []string{"youtube.com"}},
		{Mode: Sites, LAN: []string{"192.168.88.0/24"}, Sites: []string{"bad host!"}},
		{Mode: Off, DNS: []string{"1.1.1.0/24"}},
	} {
		if err := bad.Normalize(); err == nil {
			t.Errorf("%+v: expected error", bad)
		}
	}
}

func TestApplyAllReplacesManualRules(t *testing.T) {
	f := newFake()
	f.seed("routing/rule",
		routeros.Item{"comment": "user rule", "action": "lookup", "table": "main"},
		routeros.Item{"comment": "awg: LAN via tunnel", "src-address": "192.168.88.0/24", "action": "lookup", "table": Table},
		routeros.Item{"comment": "after", "action": "lookup", "table": "main"},
	)
	f.seed("ip/route", routeros.Item{"dst-address": "0.0.0.0/0", "gateway": "172.18.0.2", "routing-table": Table, "comment": "awg"})
	f.seed("ip/firewall/filter", routeros.Item{"comment": "awg: protect web UI", "action": "drop"})

	p := Plan{Mode: All, LAN: []string{"192.168.88.0/24"}, Exclude: []string{"192.168.88.50/32"}, KillSwitch: true}
	if err := p.Normalize(); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), f, p, env, nil, nolog); err != nil {
		t.Fatal(err)
	}
	want := []string{"user rule", "awg: LAN direct", "awg: container direct", "awg: direct device", "awg: LAN via tunnel", "after"}
	if got := f.comments("routing/rule"); !slices.Equal(got, want) {
		t.Fatalf("rules = %v", got)
	}
	last := f.menus["routing/rule"][4]
	if last["action"] != "lookup-only-in-table" || last["table"] != Table || last["disabled"] != "false" {
		t.Fatalf("tunnel rule = %v", last)
	}
	if got := f.comments("ip/route"); !slices.Equal(got, []string{ProbeComment, "awg"}) {
		t.Fatalf("routes = %v", got)
	}
	if r := f.menus["ip/route"][1]; r["gateway"] != "198.18.0.1" || r["target-scope"] != "11" {
		t.Fatalf("route = %v", r)
	}
	if len(f.menus["routing/table"]) != 1 {
		t.Fatal("routing table not created")
	}
	if got := f.comments("ip/firewall/filter"); !slices.Equal(got, []string{"awg: protect web UI"}) {
		t.Fatalf("filter = %v", got)
	}

	// Applying the same plan again changes nothing.
	f.adds, f.rms = 0, 0
	if _, err := Apply(context.Background(), f, p, env, nil, nolog); err != nil {
		t.Fatal(err)
	}
	if f.adds != 0 || f.rms != 0 {
		t.Fatalf("re-apply: %d adds, %d removes", f.adds, f.rms)
	}
}

func TestApplySitesThenOff(t *testing.T) {
	f := newFake()
	f.seed("ip/firewall/filter",
		routeros.Item{"comment": "defconf: accept in ipsec policy", "action": "accept"},
		routeros.Item{"comment": "defconf: fasttrack", "action": "fasttrack-connection"},
	)
	f.seed("ip/dhcp-client", routeros.Item{"interface": "ether1", "use-peer-dns": "true"})
	f.seed("ip/firewall/address-list", routeros.Item{"list": "vpn", "address": "example.com"})

	p := Plan{Mode: Sites, LAN: []string{"192.168.88.0/24"}, Sites: []string{"youtube.com"}, DNS: []string{"1.1.1.1"}, Paused: true}
	if err := p.Normalize(); err != nil {
		t.Fatal(err)
	}
	noProbe := env
	noProbe.Probe = netip.Addr{}
	saved, err := Apply(context.Background(), f, p, noProbe, nil, nolog)
	if err != nil {
		t.Fatal(err)
	}
	if r := f.menus["ip/route"]; len(r) != 1 || r[0]["gateway"] != "172.18.0.2" || r[0]["check-gateway"] != "ping" {
		t.Fatalf("routes = %v", r)
	}
	if got := f.comments("ip/firewall/filter"); !slices.Equal(got, []string{"defconf: accept in ipsec policy", FilterComment, "defconf: fasttrack"}) {
		t.Fatalf("filter = %v", got)
	}
	if len(f.menus["ip/firewall/mangle"]) != 2 || f.menus["ip/firewall/mangle"][1]["disabled"] != "true" {
		t.Fatalf("mangle = %v", f.menus["ip/firewall/mangle"])
	}
	if got := f.comments("routing/rule"); !slices.Equal(got, []string{"awg: DNS via tunnel", "awg: sites via tunnel"}) {
		t.Fatalf("rules = %v", got)
	}
	if len(f.menus["ip/firewall/address-list"]) != 3 {
		t.Fatalf("address lists = %v", f.menus["ip/firewall/address-list"])
	}
	if f.single["ip/dns"]["servers"] != "1.1.1.1" || f.menus["ip/dhcp-client"][0]["use-peer-dns"] != "no" {
		t.Fatal("DNS not changed")
	}
	if saved == nil || saved.Servers != "8.8.8.8" {
		t.Fatalf("saved = %+v", saved)
	}

	saved, err = Apply(context.Background(), f, Plan{Mode: Off}, env, saved, nolog)
	if err != nil {
		t.Fatal(err)
	}
	if saved != nil {
		t.Fatal("saved DNS not cleared")
	}
	for _, path := range []string{"routing/rule", "ip/route", "ip/firewall/mangle", "routing/table"} {
		if len(f.menus[path]) != 0 {
			t.Errorf("%s left: %v", path, f.menus[path])
		}
	}
	if len(f.menus["ip/firewall/filter"]) != 2 || len(f.menus["ip/firewall/address-list"]) != 1 {
		t.Fatal("removed objects that are not ours")
	}
	if f.single["ip/dns"]["servers"] != "8.8.8.8" || f.menus["ip/dhcp-client"][0]["use-peer-dns"] != "true" {
		t.Fatal("DNS not restored")
	}
}

func TestInspect(t *testing.T) {
	f := newFake()
	f.seed("ip/address",
		routeros.Item{"address": "192.168.88.1/24", "interface": "bridge"},
		routeros.Item{"address": "172.18.0.1/30", "interface": "veth-awg"},
	)
	f.seed("ip/route", routeros.Item{"dst-address": "0.0.0.0/0", "routing-table": Table, "active": "true", "comment": "awg"})
	info, e, err := Inspect(context.Background(), f, env.Container)
	if err != nil {
		t.Fatal(err)
	}
	if e.ContainerNet != env.ContainerNet || info.Identity != "home" || len(info.Networks) != 1 || info.Networks[0].Network != "192.168.88.0/24" {
		t.Fatalf("info = %+v, env = %+v", info, e)
	}
	if info.RouteActive == nil || !*info.RouteActive || info.Managed != 1 {
		t.Fatalf("info = %+v", info)
	}

	_, _, err = Inspect(context.Background(), f, netip.MustParseAddr("10.0.0.2"))
	if err == nil || !strings.Contains(err.Error(), "10.0.0.2") {
		t.Fatalf("err = %v", err)
	}
}

func TestApplyGeo(t *testing.T) {
	f := newFake()
	f.seed("ip/firewall/filter", routeros.Item{"comment": "defconf: fasttrack", "action": "fasttrack-connection"})

	p := Plan{Mode: All, LAN: []string{"192.168.88.0/24"}, ExcludeDst: []string{"203.0.113.0/24"}, BypassGeo: true}
	if err := p.Normalize(); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), f, p, env, nil, nolog); err != nil {
		t.Fatal(err)
	}
	want := []string{"awg: LAN direct", "awg: container direct", "awg: direct destination", "awg: direct countries", "awg: LAN via tunnel"}
	if got := f.comments("routing/rule"); !slices.Equal(got, want) {
		t.Fatalf("rules = %v", got)
	}
	if r := f.menus["routing/rule"][3]; r["routing-mark"] != DirectTable || r["table"] != "main" {
		t.Fatalf("countries rule = %v", r)
	}
	if len(f.menus["routing/table"]) != 2 {
		t.Fatalf("tables = %v", f.menus["routing/table"])
	}
	m := f.menus["ip/firewall/mangle"]
	if len(m) != 2 || m[0]["dst-address-list"] != ListGeo || m[1]["new-routing-mark"] != DirectTable {
		t.Fatalf("mangle = %v", m)
	}
	if got := f.comments("ip/firewall/filter"); !slices.Equal(got, []string{DirectFilter, "defconf: fasttrack"}) {
		t.Fatalf("filter = %v", got)
	}
	sc := f.menus["system/script"]
	if len(sc) != 1 || !strings.Contains(sc[0]["source"], "http://172.18.0.2/lists/geo.rsc") {
		t.Fatalf("script = %v", sc)
	}
	if len(f.menus["system/scheduler"]) != 1 {
		t.Fatal("scheduler not added")
	}
	if !slices.Equal(f.execs, []string{"/system/script/run " + GeoScript}) {
		t.Fatalf("execs = %v", f.execs)
	}

	// Turning the option off removes everything it added, including the list entries.
	f.execs = nil
	p.BypassGeo = false
	if _, err := Apply(context.Background(), f, p, env, nil, nolog); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"ip/firewall/mangle", "system/script", "system/scheduler", "ip/firewall/address-list"} {
		if len(f.menus[path]) != 0 {
			t.Errorf("%s left: %v", path, f.menus[path])
		}
	}
	if len(f.menus["routing/table"]) != 1 || len(f.execs) != 1 || !strings.Contains(f.execs[0], "list="+ListGeo) {
		t.Fatalf("tables = %v, execs = %v", f.menus["routing/table"], f.execs)
	}

	// Countries only apply to the whole-LAN mode.
	p = Plan{Mode: Devices, LAN: []string{"192.168.88.0/24"}, Devices: []string{"192.168.88.5"}, BypassGeo: true}
	if err := p.Normalize(); err != nil || p.BypassGeo {
		t.Fatalf("normalize: %v, %+v", err, p)
	}
}
