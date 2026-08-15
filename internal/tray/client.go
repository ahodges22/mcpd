package tray

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	statusTimeout       = 2 * time.Second
	actionTimeout       = 40 * time.Second
	maxResponseBodySize = 1 << 20
)

type Action string

const (
	ActionReconnect Action = "reconnect"
	ActionAuthorize Action = "authorize"
)

type BackendStatus struct {
	Name              string `json:"name"`
	State             string `json:"state"`
	Label             string `json:"label"`
	RecommendedAction Action `json:"recommended_action,omitempty"`
}

type Status struct {
	Backends []BackendStatus `json:"backends"`
	Serving  int             `json:"serving"`
}

type Client struct {
	baseURL url.URL
	http    *http.Client
}

func NewClient(addr string) (*Client, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || port == "" {
		return nil, fmt.Errorf("invalid tray address %q: expected a loopback host and port", addr)
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		return nil, fmt.Errorf("invalid tray address %q: port must be numeric", addr)
	}
	if !strings.EqualFold(host, "localhost") {
		ip, err := netip.ParseAddr(host)
		if err != nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("invalid tray address %q: host is not loopback", addr)
		}
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Client{
		baseURL: url.URL{Scheme: "http", Host: addr},
		http: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()

	target := c.baseURL
	target.Path = "/api/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return Status{}, fmt.Errorf("build status request: %w", err)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return Status{}, fmt.Errorf("read status: %w", err)
	}
	defer res.Body.Close()

	body, err := boundedBody(res.Body)
	if err != nil {
		return Status{}, fmt.Errorf("read status: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return Status{}, fmt.Errorf("read status: %s", canonicalHTTPStatus(res.StatusCode))
	}

	var status Status
	if err := json.Unmarshal(body, &status); err != nil {
		return Status{}, fmt.Errorf("decode status: %w", err)
	}
	return status, nil
}

func (c *Client) Reconnect(ctx context.Context, name string) error {
	_, err := c.action(ctx, name, "reconnect")
	return err
}

func (c *Client) Authorize(ctx context.Context, name string) (string, error) {
	body, err := c.action(ctx, name, "authorize")
	if err != nil {
		return "", err
	}
	var response struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("decode authorize response: %w", err)
	}
	if response.AuthorizeURL != "" {
		if err := validateAuthorizeURL(response.AuthorizeURL); err != nil {
			return "", err
		}
	}
	return response.AuthorizeURL, nil
}

func (c *Client) action(ctx context.Context, name, operation string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, actionTimeout)
	defer cancel()

	target := c.baseURL.String() + "/api/backends/" + url.PathEscape(name) + "/" + operation
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewBufferString(`{}`))
	if err != nil {
		return nil, fmt.Errorf("build %s request: %w", operation, err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s backend: %w", operation, err)
	}
	defer res.Body.Close()

	body, err := boundedBody(res.Body)
	if err != nil {
		return nil, fmt.Errorf("%s backend: %w", operation, err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("%s backend: %s", operation, canonicalHTTPStatus(res.StatusCode))
	}
	return body, nil
}

func OpenAuthorizeURL(raw string, open func(string) error) error {
	if err := validateAuthorizeURL(raw); err != nil {
		return err
	}
	if open == nil {
		return fmt.Errorf("open authorization URL: no opener configured")
	}
	if err := open(raw); err != nil {
		return fmt.Errorf("open authorization URL: %w", err)
	}
	return nil
}

func validateAuthorizeURL(raw string) error {
	target, err := url.Parse(raw)
	if err != nil || !target.IsAbs() || target.Hostname() == "" {
		return fmt.Errorf("refuse authorization URL: malformed or relative target")
	}
	if strings.EqualFold(target.Scheme, "https") {
		return nil
	}
	if strings.EqualFold(target.Scheme, "http") && loopbackHostname(target.Hostname()) {
		return nil
	}
	return fmt.Errorf("refuse authorization URL: target must use https or loopback http")
}

func loopbackHostname(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

func canonicalHTTPStatus(code int) string {
	if text := http.StatusText(code); text != "" {
		return fmt.Sprintf("%d %s", code, text)
	}
	return strconv.Itoa(code)
}

func boundedBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxResponseBodySize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseBodySize {
		return nil, fmt.Errorf("response body exceeds %d bytes", maxResponseBodySize)
	}
	return body, nil
}
