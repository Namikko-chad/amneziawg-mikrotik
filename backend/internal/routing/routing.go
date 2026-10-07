// Package routing turns a routing plan (which LAN traffic goes through the tunnel) into RouterOS
// objects and applies it to the router.
//
// The objects match the manual setup in docs/mikrotik-setup.md: routing rules and mangle rules with
// comments starting with "awg", the to-awg routing table and the routes in it. Applying a plan
// replaces all of them, including ones added by hand.
package routing

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/Namikko-chad/amneziawg-mikrotik/backend/internal/routeros"
)

type Mode string

const (
	Off     Mode = "off"
	All     Mode = "all"     // whole LAN, with exceptions
	Devices Mode = "devices" // only selected devices
	Sites   Mode = "sites"   // only selected destinations
)

const (
	Table          = "to-awg"
	DirectTable    = "awg-direct" // routing mark for connections that bypass the tunnel
	ProbeComment   = "awg: probe"
	FilterComment  = "awg: sites skip fasttrack"
	DirectFilter   = "awg: direct skip fasttrack"
	ListLAN        = "awg-lan"
	ListSites      = "awg-vpn"
	ListGeo        = "awg-geo" // filled by GeoScript from the container's country list
	GeoScript      = "awg-geo"
	GeoPath        = "/lists/geo.rsc"
	connMark       = "awg-conn"
	directMark     = "awg-direct"
	commentPrefix  = "awg"
	routeComment   = "awg"
	mangleComment  = "awg"
	addrComment    = "awg"
	defaultRouteTo = "0.0.0.0/0"
)

// Plan is the desired routing setup. It is stored by the backend and edited in the web UI.
type Plan struct {
	Mode       Mode     `json:"mode"`
	LAN        []string `json:"lan"`         // LAN subnets
	Exclude    []string `json:"exclude"`     // mode all: devices or subnets that never use the tunnel
	ExcludeDst []string `json:"exclude_dst"` // mode all: destinations that never use the tunnel
	BypassGeo  bool     `json:"bypass_geo"`  // mode all: the container's country list never uses the tunnel
	Devices    []string `json:"devices"`     // mode devices
	Sites      []string `json:"sites"`       // mode sites: domains, addresses or subnets
	KillSwitch bool     `json:"kill_switch"` // block instead of going direct while the tunnel is down
	Paused     bool     `json:"paused"`      // rules stay in place but are disabled
	DNS        []string `json:"dns"`         // resolvers to use and to reach through the tunnel; empty = leave DNS alone
}

// Env describes the container as seen from the router.
type Env struct {
	Container    netip.Addr   // container address on the veth, the gateway for the tunnel
	ContainerNet netip.Prefix // veth subnet
	// Probe, if set, is an address inside the container that answers pings only while the tunnel
	// is up. The route's check-gateway then also detects a tunnel that is down; without it, only
	// a stopped container is detected.
	Probe netip.Addr
}

// SavedDNS is the router's DNS setup before the plan changed it, so it can be restored.
type SavedDNS struct {
	Servers string            `json:"servers"`
	PeerDNS map[string]string `json:"peer_dns"` // dhcp-client id -> use-peer-dns
}

// Normalize validates the plan and canonicalizes its addresses.
func (p *Plan) Normalize() error {
	switch p.Mode {
	case "":
		p.Mode = Off
	case Off, All, Devices, Sites:
	default:
		return fmt.Errorf("unknown mode %q", p.Mode)
	}
	var err error
	fields := []struct {
		name  string
		v     *[]string
		sites bool
	}{
		{"LAN subnets", &p.LAN, false},
		{"excluded devices", &p.Exclude, false},
		{"excluded destinations", &p.ExcludeDst, false},
		{"devices", &p.Devices, false},
		{"sites", &p.Sites, true},
		{"DNS servers", &p.DNS, false},
	}
	for _, f := range fields {
		if *f.v, err = normList(*f.v, f.sites); err != nil {
			return fmt.Errorf("%s: %w", f.name, err)
		}
	}
	for _, d := range p.DNS {
		if strings.Contains(d, "/") && !strings.HasSuffix(d, "/32") {
			return fmt.Errorf("DNS servers: %s is a subnet", d)
		}
	}
	if p.Mode != All {
		p.BypassGeo = false
	}
	if p.Mode == Off {
		return nil
	}
	if len(p.LAN) == 0 {
		return errors.New("enter at least one LAN subnet")
	}
	if p.Mode == Devices && len(p.Devices) == 0 {
		return errors.New("enter at least one device")
	}
	if p.Mode == Sites && len(p.Sites) == 0 {
		return errors.New("enter at least one site")
	}
	return nil
}

var domainRe = regexp.MustCompile(`^(?i)[a-z0-9_]([a-z0-9_-]{0,62}\.)*[a-z0-9-]{1,63}\.?$`)

// normList trims, dedups and canonicalizes IPv4 addresses and subnets. Hosts become a.b.c.d/32.
// With domains set, domain names are accepted as well.
func normList(in []string, domains bool) ([]string, error) {
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		v, err := normAddr(s)
		if err != nil {
			if !domains || !domainRe.MatchString(s) || !strings.Contains(s, ".") {
				return nil, err
			}
			v = strings.TrimSuffix(strings.ToLower(s), ".")
		}
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out, nil
}

func normAddr(s string) (string, error) {
	if !strings.Contains(s, "/") {
		s += "/32"
	}
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is4() {
		return "", fmt.Errorf("%q is not an IPv4 address or subnet", strings.TrimSuffix(s, "/32"))
	}
	return p.Masked().String(), nil
}

// section is one RouterOS menu whose objects the plan owns.
type section struct {
	path    string
	owned   func(routeros.Item) bool
	ordered bool // rule order matters: new objects take the place of the old ones
	named   bool // names are unique: the old objects must go before the new ones are added
}

var sections = []section{
	{path: "ip/firewall/address-list", owned: func(it routeros.Item) bool {
		return it["list"] == ListLAN || it["list"] == ListSites
	}},
	{path: "ip/route", owned: func(it routeros.Item) bool {
		return it["routing-table"] == Table || it["comment"] == ProbeComment
	}},
	{path: "routing/rule", ordered: true, owned: func(it routeros.Item) bool {
		return strings.HasPrefix(it["comment"], commentPrefix)
	}},
	{path: "ip/firewall/mangle", ordered: true, owned: func(it routeros.Item) bool {
		return strings.HasPrefix(it["comment"], commentPrefix)
	}},
	{path: "ip/firewall/filter", ordered: true, owned: func(it routeros.Item) bool {
		return it["comment"] == FilterComment || it["comment"] == DirectFilter
	}},
	{path: "system/script", named: true, owned: func(it routeros.Item) bool { return it["name"] == GeoScript }},
	{path: "system/scheduler", named: true, owned: func(it routeros.Item) bool { return it["name"] == GeoScript }},
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// Build returns the objects the plan needs, by menu path, in order.
func Build(p Plan, e Env) map[string][]routeros.Item {
	out := map[string][]routeros.Item{}
	if p.Mode == Off {
		return out
	}
	add := func(path string, it routeros.Item) { out[path] = append(out[path], it) }
	tunnelAction := "lookup"
	if p.KillSwitch {
		tunnelAction = "lookup-only-in-table"
	}
	paused := yesNo(p.Paused)

	if e.Probe.IsValid() {
		add("ip/route", routeros.Item{
			"dst-address": e.Probe.String() + "/32", "gateway": e.Container.String(),
			"scope": "10", "comment": ProbeComment,
		})
		add("ip/route", routeros.Item{
			"dst-address": defaultRouteTo, "gateway": e.Probe.String(), "target-scope": "11",
			"routing-table": Table, "check-gateway": "ping", "comment": routeComment,
		})
	} else {
		add("ip/route", routeros.Item{
			"dst-address": defaultRouteTo, "gateway": e.Container.String(),
			"routing-table": Table, "check-gateway": "ping", "comment": routeComment,
		})
	}

	rule := func(comment string, kv ...string) {
		it := routeros.Item{"comment": comment}
		for i := 0; i+1 < len(kv); i += 2 {
			it[kv[i]] = kv[i+1]
		}
		add("routing/rule", it)
	}
	direct := func(match, addr, comment string) {
		rule(comment, match, addr, "action", "lookup-only-in-table", "table", "main")
	}
	viaTunnel := func(match, addr, comment string) {
		rule(comment, match, addr, "action", tunnelAction, "table", Table, "disabled", paused)
	}

	lanList := func() {
		for _, l := range p.LAN {
			add("ip/firewall/address-list", routeros.Item{"list": ListLAN, "address": l, "comment": addrComment})
		}
	}
	// markConns gives connections from the LAN to dstList a routing mark.
	markConns := func(dstList, connMark, routingMark, disabled string) {
		add("ip/firewall/mangle", routeros.Item{
			"chain": "prerouting", "src-address-list": ListLAN, "dst-address-list": dstList,
			"connection-mark": "no-mark", "action": "mark-connection", "new-connection-mark": connMark,
			"passthrough": "yes", "comment": mangleComment, "disabled": disabled,
		})
		add("ip/firewall/mangle", routeros.Item{
			"chain": "prerouting", "src-address-list": ListLAN, "connection-mark": connMark,
			"action": "mark-routing", "new-routing-mark": routingMark, "passthrough": "no",
			"comment": mangleComment, "disabled": disabled,
		})
	}
	// FastTrack skips mangle, so marked connections must not be fast-tracked. This mirrors the
	// defconf "accept established,related" rule for them, placed before the fasttrack rule.
	skipFasttrack := func(connMark, comment string) {
		add("ip/firewall/filter", routeros.Item{
			"chain": "forward", "connection-state": "established,related", "connection-mark": connMark,
			"action": "accept", "comment": comment,
		})
	}

	for _, d := range p.DNS {
		viaTunnel("dst-address", d, "awg: DNS via tunnel")
	}
	switch p.Mode {
	case All, Devices:
		for _, l := range p.LAN {
			direct("dst-address", l, "awg: LAN direct")
		}
		direct("dst-address", e.ContainerNet.String(), "awg: container direct")
		if p.Mode == Devices {
			for _, d := range p.Devices {
				viaTunnel("src-address", d, "awg: device via tunnel")
			}
			break
		}
		for _, d := range p.Exclude {
			direct("src-address", d, "awg: direct device")
		}
		for _, d := range p.ExcludeDst {
			direct("dst-address", d, "awg: direct destination")
		}
		if p.BypassGeo {
			direct("routing-mark", DirectTable, "awg: direct countries")
			lanList()
			markConns(ListGeo, directMark, DirectTable, "no")
			skipFasttrack(directMark, DirectFilter)
			addGeoScript(add, e)
		}
		for _, l := range p.LAN {
			viaTunnel("src-address", l, "awg: LAN via tunnel")
		}
	case Sites:
		viaTunnel("routing-mark", Table, "awg: sites via tunnel")
		lanList()
		for _, s := range p.Sites {
			add("ip/firewall/address-list", routeros.Item{"list": ListSites, "address": s, "comment": addrComment})
		}
		markConns(ListSites, connMark, Table, paused)
		skipFasttrack(connMark, FilterComment)
	}
	return out
}

// geoPolicy lets the script download a file, import it and remove it.
const geoPolicy = "ftp,read,write,test"

// addGeoScript adds the router script that loads the country list from the container into
// ListGeo, and a daily schedule for it. A failed download leaves the previous list in place.
func addGeoScript(add func(string, routeros.Item), e Env) {
	file := GeoScript + ".rsc"
	src := fmt.Sprintf(`:do {
	/tool/fetch url="http://%s%s" dst-path=%s
	/import file-name=%s
} on-error={ :log warning "%s: cannot load the country list from the container" }
:do { /file/remove %s } on-error={}`, e.Container, GeoPath, file, file, GeoScript, file)
	add("system/script", routeros.Item{
		"name": GeoScript, "source": src, "policy": geoPolicy, "comment": "awg: country list",
	})
	add("system/scheduler", routeros.Item{
		"name": GeoScript, "start-time": "04:30:00", "interval": "1d",
		"on-event": "/system/script/run " + GeoScript, "policy": geoPolicy, "comment": "awg: country list",
	})
}

// API is the subset of the RouterOS client used here.
type API interface {
	List(ctx context.Context, path string, filter routeros.Item) ([]routeros.Item, error)
	Get(ctx context.Context, path string) (routeros.Item, error)
	Add(ctx context.Context, path string, props routeros.Item) (string, error)
	Set(ctx context.Context, path, id string, props routeros.Item) error
	Remove(ctx context.Context, path, id string) error
	Run(ctx context.Context, path, cmd string, args routeros.Item) error
}

// Apply makes the router match the plan. saved is the DNS setup recorded by an earlier Apply;
// the returned value must be stored even when err is not nil.
func Apply(ctx context.Context, api API, p Plan, e Env, saved *SavedDNS, logf func(string, ...any)) (*SavedDNS, error) {
	want := Build(p, e)
	order := sections
	tables := []string{Table}
	if p.BypassGeo {
		tables = append(tables, DirectTable)
	}
	if p.Mode == Off {
		// Remove the rules before the routes they point at.
		order = slices.Clone(sections)
		slices.Reverse(order)
		tables = nil
	}
	for _, t := range tables {
		if err := ensureTable(ctx, api, t, logf); err != nil {
			return saved, err
		}
	}
	hadGeo, err := api.List(ctx, "system/script", routeros.Item{"name": GeoScript})
	if err != nil {
		return saved, err
	}
	for _, s := range order {
		if err := reconcile(ctx, api, s, want[s.path], logf); err != nil {
			return saved, fmt.Errorf("%s: %w", s.path, err)
		}
	}
	saved, err = applyDNS(ctx, api, p.DNS, saved, logf)
	if err != nil {
		return saved, fmt.Errorf("dns: %w", err)
	}
	switch {
	case p.BypassGeo:
		if err := LoadGeo(ctx, api, logf); err != nil {
			return saved, err
		}
	case len(hadGeo) > 0:
		// The list can hold thousands of entries: let the router remove them itself.
		logf("router: removing address list %s", ListGeo)
		if err := execute(ctx, api, fmt.Sprintf("/ip/firewall/address-list/remove [find list=%s]", ListGeo)); err != nil {
			return saved, err
		}
	}
	for _, t := range []string{Table, DirectTable} {
		if !slices.Contains(tables, t) {
			if err := removeTable(ctx, api, t, logf); err != nil {
				return saved, err
			}
		}
	}
	return saved, nil
}

// LoadGeo makes the router reload the country list from the container now, in the background.
func LoadGeo(ctx context.Context, api API, logf func(string, ...any)) error {
	logf("router: loading the country list into %s", ListGeo)
	return execute(ctx, api, "/system/script/run "+GeoScript)
}

// execute starts a script on the router without waiting for it to finish.
func execute(ctx context.Context, api API, script string) error {
	return api.Run(ctx, "", "execute", routeros.Item{"script": script})
}

func ensureTable(ctx context.Context, api API, name string, logf func(string, ...any)) error {
	have, err := api.List(ctx, "routing/table", routeros.Item{"name": name})
	if err != nil {
		return err
	}
	if len(have) > 0 {
		return nil
	}
	logf("router: adding routing table %s", name)
	_, err = api.Add(ctx, "routing/table", routeros.Item{"name": name, "fib": ""})
	return err
}

func removeTable(ctx context.Context, api API, name string, logf func(string, ...any)) error {
	have, err := api.List(ctx, "routing/table", routeros.Item{"name": name})
	if err != nil {
		return err
	}
	for _, t := range have {
		logf("router: removing routing table %s", name)
		if err := api.Remove(ctx, "routing/table", t.ID()); err != nil {
			return err
		}
	}
	return nil
}

// reconcile replaces the owned objects in s with want. Objects that already match are left
// alone. New objects are added before the old ones are removed, so traffic is never left without
// a rule; in ordered menus they take the place of the old ones. Named objects are replaced the
// other way round.
func reconcile(ctx context.Context, api API, s section, want []routeros.Item, logf func(string, ...any)) error {
	all, err := api.List(ctx, s.path, nil)
	if err != nil {
		return err
	}
	var owned []routeros.Item
	for _, it := range all {
		if it["dynamic"] != "true" && s.owned(it) {
			owned = append(owned, it)
		}
	}
	if matches(owned, want, s.ordered) {
		return nil
	}

	placeBefore := ""
	if len(owned) > 0 && s.ordered {
		placeBefore = owned[0].ID()
	}
	if s.path == "ip/firewall/filter" && len(want) > 0 {
		// Must go before FastTrack. Without a fasttrack rule it is not needed.
		placeBefore = ""
		for _, it := range all {
			if it["action"] == "fasttrack-connection" && it["disabled"] != "true" {
				placeBefore = it.ID()
				break
			}
		}
		if placeBefore == "" {
			want = nil
		}
	}

	remove := func() error {
		for _, it := range owned {
			if err := api.Remove(ctx, s.path, it.ID()); err != nil {
				var re *routeros.Error
				if errors.As(err, &re) && re.Status == 404 {
					continue
				}
				return fmt.Errorf("remove %s: %w", describe(it), err)
			}
		}
		return nil
	}
	if s.named {
		if err := remove(); err != nil {
			return err
		}
	}
	for _, it := range want {
		props := clone(it)
		if placeBefore != "" {
			props["place-before"] = placeBefore
		}
		if _, err := api.Add(ctx, s.path, props); err != nil {
			return fmt.Errorf("add %s: %w", describe(it), err)
		}
	}
	if !s.named {
		if err := remove(); err != nil {
			return err
		}
	}
	if len(owned)+len(want) > 0 {
		logf("router: %s: %d removed, %d added", s.path, len(owned), len(want))
	}
	return nil
}

func clone(it routeros.Item) routeros.Item {
	out := make(routeros.Item, len(it)+1)
	for k, v := range it {
		out[k] = v
	}
	return out
}

func describe(it routeros.Item) string {
	for _, k := range []string{"comment", "address", "dst-address"} {
		if v := it[k]; v != "" {
			return fmt.Sprintf("%q", v)
		}
	}
	return it.ID()
}

// matches reports whether the router objects have already the wanted properties.
func matches(have, want []routeros.Item, ordered bool) bool {
	if len(have) != len(want) {
		return false
	}
	used := make([]bool, len(have))
	for i, w := range want {
		found := false
		for j, h := range have {
			if used[j] || (ordered && j != i) {
				continue
			}
			if same(w, h) {
				used[j], found = true, true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func same(want, have routeros.Item) bool {
	for k, v := range want {
		hv, ok := have[k]
		if !ok && k == "disabled" {
			hv = "false"
		}
		if norm(v) != norm(hv) {
			return false
		}
	}
	return true
}

func norm(v string) string {
	v = strings.ToLower(strings.TrimSuffix(v, "/32"))
	switch v {
	case "yes":
		return "true"
	case "no":
		return "false"
	}
	return v
}

// applyDNS points the router at the plan's resolvers, remembering the previous setup, or restores
// it when the plan has none.
func applyDNS(ctx context.Context, api API, servers []string, saved *SavedDNS, logf func(string, ...any)) (*SavedDNS, error) {
	clients, err := api.List(ctx, "ip/dhcp-client", nil)
	if err != nil {
		return saved, err
	}
	if len(servers) == 0 {
		if saved == nil {
			return nil, nil
		}
		logf("router: restoring DNS servers %q", saved.Servers)
		if err := api.Run(ctx, "ip/dns", "set", routeros.Item{"servers": saved.Servers}); err != nil {
			return saved, err
		}
		for _, c := range clients {
			if v, ok := saved.PeerDNS[c.ID()]; ok {
				if err := api.Set(ctx, "ip/dhcp-client", c.ID(), routeros.Item{"use-peer-dns": v}); err != nil {
					return saved, err
				}
			}
		}
		api.Run(ctx, "ip/dns/cache", "flush", nil)
		return nil, nil
	}

	if saved == nil {
		dns, err := api.Get(ctx, "ip/dns")
		if err != nil {
			return nil, err
		}
		saved = &SavedDNS{Servers: dns["servers"], PeerDNS: map[string]string{}}
		for _, c := range clients {
			saved.PeerDNS[c.ID()] = c["use-peer-dns"]
		}
	}
	var plain []string
	for _, s := range servers {
		plain = append(plain, strings.TrimSuffix(s, "/32"))
	}
	dns, err := api.Get(ctx, "ip/dns")
	if err != nil {
		return saved, err
	}
	changed := false
	if dns["servers"] != strings.Join(plain, ",") {
		logf("router: DNS servers %s", strings.Join(plain, ","))
		if err := api.Run(ctx, "ip/dns", "set", routeros.Item{"servers": strings.Join(plain, ",")}); err != nil {
			return saved, err
		}
		changed = true
	}
	for _, c := range clients {
		if c["use-peer-dns"] != "false" {
			if err := api.Set(ctx, "ip/dhcp-client", c.ID(), routeros.Item{"use-peer-dns": "no"}); err != nil {
				return saved, err
			}
			changed = true
		}
	}
	if changed {
		api.Run(ctx, "ip/dns/cache", "flush", nil)
	}
	return saved, nil
}

// Network is an IPv4 subnet configured on the router.
type Network struct {
	Network   string `json:"network"`
	Interface string `json:"interface"`
}

// Info is the router state shown in the web UI.
type Info struct {
	Identity     string    `json:"identity"`
	Version      string    `json:"version"`
	Board        string    `json:"board"`
	Container    string    `json:"container"`
	ContainerNet string    `json:"container_net"`
	Networks     []Network `json:"networks"`
	RouteActive  *bool     `json:"route_active"` // nil when the to-awg route does not exist
	Managed      int       `json:"managed"`      // objects on the router that a plan would replace
}

// Inspect reads the router state and works out the container's veth subnet.
func Inspect(ctx context.Context, api API, container netip.Addr) (Info, Env, error) {
	var info Info
	env := Env{Container: container}
	res, err := api.Get(ctx, "system/resource")
	if err != nil {
		return info, env, err
	}
	info.Version, info.Board = res["version"], res["board-name"]
	if id, err := api.Get(ctx, "system/identity"); err == nil {
		info.Identity = id["name"]
	}
	info.Container = container.String()

	addrs, err := api.List(ctx, "ip/address", nil)
	if err != nil {
		return info, env, err
	}
	for _, a := range addrs {
		p, err := netip.ParsePrefix(a["address"])
		if err != nil || a["disabled"] == "true" {
			continue
		}
		if p.Contains(container) {
			env.ContainerNet = p.Masked()
			continue
		}
		info.Networks = append(info.Networks, Network{Network: p.Masked().String(), Interface: a["interface"]})
	}
	if !env.ContainerNet.IsValid() {
		return info, env, fmt.Errorf("no router address contains the container address %s: is this the router running the container?", container)
	}
	info.ContainerNet = env.ContainerNet.String()

	routes, err := api.List(ctx, "ip/route", routeros.Item{"routing-table": Table})
	if err != nil {
		return info, env, err
	}
	for _, r := range routes {
		if r["dst-address"] == defaultRouteTo {
			active := r["active"] == "true"
			info.RouteActive = &active
		}
	}
	for _, s := range sections {
		items, err := api.List(ctx, s.path, nil)
		if err != nil {
			return info, env, err
		}
		for _, it := range items {
			if it["dynamic"] != "true" && s.owned(it) {
				info.Managed++
			}
		}
	}
	return info, env, nil
}
