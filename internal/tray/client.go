package tray

import (
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
		return Status{}, fmt.Errorf("read status: %s", res.Status)
	}

	var status Status
	if err := json.Unmarshal(body, &status); err != nil {
		return Status{}, fmt.Errorf("decode status: %w", err)
	}
	return status, nil
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
