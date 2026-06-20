// SPDX-License-Identifier: AGPL-3.0-or-later
//
// Package opnsense is a minimal client for the OPNsense REST API
// (https://<host>/api, HTTPS, HTTP Basic auth where the username is the API
// key and the password is the API secret).
//
// The API is RPC-style: /api/<module>/<controller>/<command>[/<uuid>]. Read
// commands (get, search, getItem/<uuid>) are GET; write commands (addItem,
// setItem/<uuid>, delItem/<uuid>, toggleItem, set, and the per-module
// reconfigure/apply command) are POST with a JSON body. This client is generic
// over that surface — it exposes Get and Post over any /api path so the
// provider's single generic resource can drive the whole API.
package opnsense

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a stateless OPNsense REST client. OPNsense authenticates every
// request independently with an HTTP Basic API-key/secret header (no session,
// no cookie), so the client holds no mutable state and is safe for concurrent
// use; callers may share one Client across resources (the provider does).
type Client struct {
	base string // e.g. https://192.168.7.9/api
	auth string // "Basic <base64(key:secret)>"
	http *http.Client
	// SSH is an optional transport for the config.xml <system> settings that have
	// NO REST API (hostname/domain/timezone/DNS/NTP) — see ssh.go. nil when the
	// provider was configured without SSH; opnsense_system_config requires it.
	SSH *SSHClient
}

// Config configures a Client.
type Config struct {
	// Host is the OPNsense address (host or host:port), no scheme.
	Host string
	// Key / Secret are the OPNsense API credentials (Basic auth: key as
	// username, secret as password).
	Key    string
	Secret string
	// Insecure skips TLS verification (OPNsense ships a self-signed cert; true
	// is the norm on a lab/management network).
	Insecure bool
	// Timeout per request (default 30s).
	Timeout time.Duration
}

// NewClient builds a Client. It does not contact the firewall until the first
// API call.
func NewClient(c Config) *Client {
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: c.Insecure}, //nolint:gosec // self-signed mgmt cert
		MaxIdleConns:    4,
		IdleConnTimeout: 30 * time.Second,
	}
	host := strings.TrimSuffix(strings.TrimPrefix(c.Host, "https://"), "/")
	host = strings.TrimPrefix(host, "http://")
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(c.Key+":"+c.Secret))
	return &Client{
		base: fmt.Sprintf("https://%s/api", host),
		auth: auth,
		http: &http.Client{Timeout: c.Timeout, Transport: tr},
	}
}

// APIError is returned when the firewall responds with a non-2xx status.
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("opnsense %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// NotFound reports whether err is an APIError with a 404 status.
func NotFound(err error) bool {
	var ae *APIError
	if e, ok := err.(*APIError); ok {
		ae = e
	}
	return ae != nil && ae.Status == http.StatusNotFound
}

// do performs one authenticated request. path is relative to /api and must
// start with "/". body may be nil (GET).
func (c *Client) do(method, path string, body []byte) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opnsense %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, &APIError{Method: method, Path: path, Status: resp.StatusCode, Body: string(raw)}
	}
	return raw, nil
}

// Get fetches a resource. path is relative to /api (must start with "/").
func (c *Client) Get(path string) ([]byte, error) { return c.do(http.MethodGet, path, nil) }

// Post executes a write/action command with the given JSON body (may be nil
// for parameterless commands like reconfigure).
func (c *Client) Post(path string, body []byte) ([]byte, error) {
	return c.do(http.MethodPost, path, body)
}
