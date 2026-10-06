// Package geoip keeps per-country IPv4 subnet lists, downloaded from a public source and cached
// on disk, and renders them as a RouterOS script that fills an address list.
package geoip

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// DefaultSource lists the aggregated IPv4 subnets of a country; {cc} is the lower-case country code.
const DefaultSource = "https://www.ipdeny.com/ipblocks/data/aggregated/{cc}-aggregated.zone"

// MaxAge is how long a downloaded list is used before it is downloaded again.
const MaxAge = 24 * time.Hour

const maxDownload = 8 << 20

// Config is what the web UI edits.
type Config struct {
	Countries []string `json:"countries"`        // ISO 3166-1 alpha-2 codes, lower case
	Source    string   `json:"source,omitempty"` // URL template with {cc}; empty means DefaultSource
}

// Status describes one country's list.
type Status struct {
	Country  string    `json:"country"`
	Prefixes int       `json:"prefixes"`
	Updated  time.Time `json:"updated,omitzero"`
	Error    string    `json:"error,omitempty"`
}

// Store is safe for concurrent use.
type Store struct {
	dir  string
	logf func(string, ...any)
	hc   *http.Client

	update sync.Mutex // serializes downloads

	mu      sync.Mutex
	cfg     Config
	lists   map[string][]netip.Prefix
	updated map[string]time.Time
	errs    map[string]string
}

var ccRe = regexp.MustCompile(`^[a-z]{2}$`)

// Normalize validates the config and canonicalizes it.
func (c *Config) Normalize() error {
	var out []string
	for _, cc := range c.Countries {
		cc = strings.ToLower(strings.TrimSpace(cc))
		if cc == "" {
			continue
		}
		if !ccRe.MatchString(cc) {
			return fmt.Errorf("%q is not a two-letter country code", cc)
		}
		if userAssigned(cc) {
			// ipdeny's "zz" covers huge unassigned and reserved ranges: it would bypass almost everything.
			return fmt.Errorf("%q is not a country", cc)
		}
		if !slices.Contains(out, cc) {
			out = append(out, cc)
		}
	}
	c.Countries = out
	c.Source = strings.TrimSpace(c.Source)
	if c.Source == DefaultSource {
		c.Source = ""
	}
	if c.Source != "" {
		u, err := url.Parse(c.Source)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("source: %q is not an http(s) URL", c.Source)
		}
		if !strings.Contains(c.Source, "{cc}") {
			return errors.New("source: the URL must contain {cc}, which is replaced by the country code")
		}
	}
	return nil
}

// userAssigned reports ISO 3166-1 codes that are reserved for private use and name no country.
func userAssigned(cc string) bool {
	return cc == "aa" || cc == "zz" || (cc[0] == 'q' && cc[1] >= 'm') || cc[0] == 'x'
}

func (c Config) url(cc string) string {
	src := c.Source
	if src == "" {
		src = DefaultSource
	}
	return strings.ReplaceAll(src, "{cc}", cc)
}

// Open loads the config and the cached lists from dir.
func Open(dir string, logf func(string, ...any)) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{
		dir: dir, logf: logf,
		hc:      &http.Client{Timeout: 60 * time.Second},
		lists:   map[string][]netip.Prefix{},
		updated: map[string]time.Time{},
		errs:    map[string]string{},
	}
	b, err := os.ReadFile(s.configPath())
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &s.cfg); err != nil {
			return nil, err
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	for _, cc := range s.cfg.Countries {
		f, err := os.Open(s.listPath(cc))
		if err != nil {
			continue
		}
		list, err := parse(f)
		fi, _ := f.Stat()
		f.Close()
		if err != nil {
			logf("geoip: cached list %s: %v", cc, err)
			continue
		}
		s.lists[cc] = list
		s.updated[cc] = fi.ModTime()
	}
	return s, nil
}

func (s *Store) configPath() string        { return filepath.Join(s.dir, "config.json") }
func (s *Store) listPath(cc string) string { return filepath.Join(s.dir, cc+".txt") }

// Config returns the current config.
func (s *Store) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Config{Countries: slices.Clone(s.cfg.Countries), Source: s.cfg.Source}
}

// SetConfig saves c and downloads the lists it is missing. A changed source invalidates every list.
func (s *Store) SetConfig(ctx context.Context, c Config) error {
	if err := c.Normalize(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.configPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.configPath()); err != nil {
		return err
	}

	s.mu.Lock()
	sourceChanged := s.cfg.Source != c.Source
	s.cfg = c
	for cc := range s.lists {
		if !slices.Contains(c.Countries, cc) {
			delete(s.lists, cc)
			os.Remove(s.listPath(cc))
		}
	}
	for cc := range s.updated {
		if sourceChanged || !slices.Contains(c.Countries, cc) {
			delete(s.updated, cc)
		}
	}
	for cc := range s.errs {
		if !slices.Contains(c.Countries, cc) {
			delete(s.errs, cc)
		}
	}
	s.mu.Unlock()
	s.Update(ctx, false)
	return nil
}

// Update downloads the lists that are missing or older than MaxAge, or all of them with force.
// A failed download keeps the previous list and is reported in Status.
func (s *Store) Update(ctx context.Context, force bool) {
	s.update.Lock()
	defer s.update.Unlock()
	cfg := s.Config()
	for _, cc := range cfg.Countries {
		s.mu.Lock()
		fresh := time.Since(s.updated[cc]) < MaxAge
		s.mu.Unlock()
		if fresh && !force {
			continue
		}
		list, err := s.download(ctx, cfg.url(cc))
		if err == nil {
			err = s.writeList(cc, list)
		}
		s.mu.Lock()
		if err != nil {
			s.errs[cc] = err.Error()
			s.logf("geoip: %s: %v", cc, err)
		} else {
			delete(s.errs, cc)
			s.lists[cc] = list
			s.updated[cc] = time.Now()
			s.logf("geoip: %s: %d subnets", cc, len(list))
		}
		s.mu.Unlock()
	}
}

// Run refreshes the lists every hour until ctx is done.
func (s *Store) Run(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		s.Update(ctx, false)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Store) download(ctx context.Context, u string) ([]netip.Prefix, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	list, err := parse(io.LimitReader(resp.Body, maxDownload))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", u, err)
	}
	return list, nil
}

func (s *Store) writeList(cc string, list []netip.Prefix) error {
	var b bytes.Buffer
	for _, p := range list {
		b.WriteString(p.String())
		b.WriteByte('\n')
	}
	tmp := s.listPath(cc) + ".tmp"
	if err := os.WriteFile(tmp, b.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.listPath(cc))
}

// parse reads one IPv4 subnet per line. Blank lines and # comments are skipped; anything else
// (such as an HTML error page) is an error.
func parse(r io.Reader) ([]netip.Prefix, error) {
	var out []netip.Prefix
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if !strings.Contains(line, "/") {
			line += "/32"
		}
		p, err := netip.ParsePrefix(line)
		if err != nil || !p.Addr().Is4() {
			return nil, fmt.Errorf("line %d: %q is not an IPv4 subnet", n, line)
		}
		out = append(out, p.Masked())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("the list is empty")
	}
	return out, nil
}

// Status reports every configured country, in config order.
func (s *Store) Status() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Status, 0, len(s.cfg.Countries))
	for _, cc := range s.cfg.Countries {
		out = append(out, Status{Country: cc, Prefixes: len(s.lists[cc]), Updated: s.updated[cc], Error: s.errs[cc]})
	}
	return out
}

// merge combines lists, dropping duplicates and subnets inside other subnets.
func merge(lists ...[]netip.Prefix) []netip.Prefix {
	var all []netip.Prefix
	for _, l := range lists {
		all = append(all, l...)
	}
	slices.SortFunc(all, func(a, b netip.Prefix) int {
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return a.Bits() - b.Bits()
	})
	var out []netip.Prefix
	for _, p := range all {
		if n := len(out); n > 0 && out[n-1].Contains(p.Addr()) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Subnets returns the merged subnets of all configured countries.
func (s *Store) Subnets() []netip.Prefix {
	s.mu.Lock()
	defer s.mu.Unlock()
	var lists [][]netip.Prefix
	for _, cc := range s.cfg.Countries {
		lists = append(lists, s.lists[cc])
	}
	return merge(lists...)
}

// RSC renders a RouterOS script that replaces the entries of the address list with the subnets.
func RSC(list string, countries []string, subnets []netip.Prefix) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# amneziawg-mikrotik: %s, %d subnets, %s\n",
		strings.ToUpper(strings.Join(countries, " ")), len(subnets), time.Now().UTC().Format(time.RFC3339))
	b.WriteString("/ip/firewall/address-list\n")
	fmt.Fprintf(&b, "remove [find list=%s]\n", list)
	for _, p := range subnets {
		fmt.Fprintf(&b, "add list=%s address=%s\n", list, p)
	}
	return b.Bytes()
}
