// Package routeros is a minimal client for the RouterOS 7 REST API (/rest on the www or www-ssl service).
package routeros

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Item is one RouterOS object. The REST API returns every value as a string.
type Item map[string]string

// ID returns the object's internal id (e.g. "*1A").
func (it Item) ID() string { return it[".id"] }

type Client struct {
	base       string
	user, pass string
	hc         *http.Client
}

// New creates a client for baseURL (e.g. "http://172.18.0.1"). insecure skips TLS certificate
// verification, which is needed for the router's self-signed www-ssl certificate.
func New(baseURL, user, pass string, insecure bool) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid router URL %q", baseURL)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &Client{
		base: strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/") + "/rest/",
		user: user, pass: pass,
		hc: &http.Client{Transport: tr, Timeout: 20 * time.Second},
	}, nil
}

// Host returns host[:port] of the router URL.
func (c *Client) Host() string {
	u, _ := url.Parse(c.base)
	return u.Host
}

// Error is an error response from the router.
type Error struct {
	Status  int
	Message string
	Detail  string
}

func (e *Error) Error() string {
	switch {
	case e.Status == http.StatusUnauthorized:
		return "router: wrong user or password"
	case e.Detail != "":
		return "router: " + e.Detail
	case e.Message != "":
		return fmt.Sprintf("router: %s (%d)", e.Message, e.Status)
	}
	return fmt.Sprintf("router: HTTP %d", e.Status)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.user, c.pass)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("router: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("router: %w", err)
	}
	if resp.StatusCode >= 300 {
		e := &Error{Status: resp.StatusCode}
		var eb struct {
			Message string `json:"message"`
			Detail  string `json:"detail"`
		}
		if json.Unmarshal(data, &eb) == nil {
			e.Message, e.Detail = eb.Message, eb.Detail
		}
		return e
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return decode(data, out)
}

// decode unmarshals data into *Item or *[]Item, converting non-string values to strings.
func decode(data []byte, out any) error {
	conv := func(m map[string]any) Item {
		it := make(Item, len(m))
		for k, v := range m {
			switch v := v.(type) {
			case string:
				it[k] = v
			case nil:
			default:
				it[k] = fmt.Sprint(v)
			}
		}
		return it
	}
	switch o := out.(type) {
	case *[]Item:
		var raw []map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("router: unexpected response: %w", err)
		}
		*o = make([]Item, len(raw))
		for i, m := range raw {
			(*o)[i] = conv(m)
		}
	case *Item:
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("router: unexpected response: %w", err)
		}
		*o = conv(raw)
	default:
		return json.Unmarshal(data, out)
	}
	return nil
}

// List returns all objects under path (e.g. "routing/rule") whose properties equal filter.
func (c *Client) List(ctx context.Context, path string, filter Item) ([]Item, error) {
	q := url.Values{}
	for k, v := range filter {
		q.Set(k, v)
	}
	p := path
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	var out []Item
	return out, c.do(ctx, http.MethodGet, p, nil, &out)
}

// Get returns a single-object menu such as "ip/dns" or "system/resource".
func (c *Client) Get(ctx context.Context, path string) (Item, error) {
	var out Item
	return out, c.do(ctx, http.MethodGet, path, nil, &out)
}

// Add creates an object and returns its id.
func (c *Client) Add(ctx context.Context, path string, props Item) (string, error) {
	var out Item
	if err := c.do(ctx, http.MethodPut, path, props, &out); err != nil {
		return "", err
	}
	return out.ID(), nil
}

// Set changes properties of an object.
func (c *Client) Set(ctx context.Context, path, id string, props Item) error {
	return c.do(ctx, http.MethodPatch, path+"/"+url.PathEscape(id), props, nil)
}

// Remove deletes an object.
func (c *Client) Remove(ctx context.Context, path, id string) error {
	return c.do(ctx, http.MethodDelete, path+"/"+url.PathEscape(id), nil, nil)
}

// Run executes a menu command, e.g. Run(ctx, "ip/dns", "set", Item{"servers": "1.1.1.1"}).
func (c *Client) Run(ctx context.Context, path, cmd string, args Item) error {
	if args == nil {
		args = Item{}
	}
	return c.do(ctx, http.MethodPost, path+"/"+cmd, args, nil)
}
