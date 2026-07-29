// Command oauthprobe answers the Phase 0 questions in the mcpd design spec:
// does the target MCP server support metadata discovery, Dynamic Client
// Registration, and a plain-HTTP loopback redirect URI?
//
// Discovery starts from the 401 WWW-Authenticate challenge rather than a guessed
// well-known path, because that is the path the SDK's AuthorizationCodeHandler
// itself takes.
//
// Q4 (is a refresh token actually issued) needs an interactive grant and is
// recorded in Task 14, not here.
//
//	go run ./cmd/oauthprobe -server https://mcp.notion.com/mcp
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

var (
	serverURL = flag.String("server", "https://mcp.notion.com/mcp", "MCP server URL to probe")
	register  = flag.Bool("register", false, "actually perform DCR (creates a client at the provider)")
	redirect  = flag.String("redirect", "http://127.0.0.1:7420/oauth/callback", "loopback redirect URI to test")
)

func main() {
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hc := &http.Client{Timeout: 20 * time.Second}

	if err := probe(ctx, hc); err != nil {
		fmt.Fprintf(os.Stderr, "\nPROBE FAILED: %v\n", err)
		os.Exit(1)
	}
}

func probe(ctx context.Context, hc *http.Client) error {
	// Q1a: an unauthenticated request must yield a 401 carrying a resource_metadata
	// pointer. This is the discovery entry point.
	metaURL, err := challengeMetadataURL(ctx, hc)
	if err != nil {
		return err
	}
	fmt.Printf("=== Q1a: WWW-Authenticate challenge ===\nresource_metadata: %s\n", metaURL)

	// Q1b: protected-resource metadata. Note GetProtectedResourceMetadata verifies
	// that the returned "resource" field equals resourceURL, so both are passed.
	prm, err := oauthex.GetProtectedResourceMetadata(ctx, metaURL, *serverURL, hc)
	if err != nil {
		return fmt.Errorf("protected-resource metadata: %w", err)
	}
	dump("Q1b: protected-resource metadata", prm)
	if len(prm.AuthorizationServers) == 0 {
		return fmt.Errorf("no authorization_servers advertised; cannot continue")
	}
	issuer := prm.AuthorizationServers[0]

	// Q2: authorization-server metadata. GetAuthServerMeta also verifies PKCE support.
	asmURL := issuer + "/.well-known/oauth-authorization-server"
	asm, err := oauthex.GetAuthServerMeta(ctx, asmURL, issuer, hc)
	if err != nil {
		return fmt.Errorf("authorization-server metadata: %w", err)
	}
	fmt.Printf("\n=== Q2: authorization-server metadata ===\n")
	fmt.Printf("issuer:                            %s\n", asm.Issuer)
	fmt.Printf("authorization_endpoint:            %s\n", asm.AuthorizationEndpoint)
	fmt.Printf("token_endpoint:                    %s\n", asm.TokenEndpoint)
	fmt.Printf("registration_endpoint:             %s\n", orAbsent(asm.RegistrationEndpoint))
	fmt.Printf("grant_types_supported:             %v\n", asm.GrantTypesSupported)
	fmt.Printf("code_challenge_methods_supported:  %v\n", asm.CodeChallengeMethodsSupported)

	if asm.RegistrationEndpoint == "" {
		fmt.Printf("\n=== Q3: DCR ===\nUNSUPPORTED: no registration_endpoint.\n" +
			"Consequence: Task 10 must use PreregisteredClient.\n")
		return nil
	}

	// Q3: does DCR accept a plain-HTTP loopback redirect URI? This creates a real
	// client at the provider, so it is opt-in.
	if !*register {
		fmt.Printf("\n=== Q3: DCR ===\nSKIPPED (pass -register to create a client at %s)\n",
			asm.RegistrationEndpoint)
		return nil
	}
	res, err := oauthex.RegisterClient(ctx, asm.RegistrationEndpoint,
		&oauthex.ClientRegistrationMetadata{
			ClientName:              "mcpd (feasibility probe)",
			RedirectURIs:            []string{*redirect},
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			ResponseTypes:           []string{"code"},
			TokenEndpointAuthMethod: "none",
		}, hc)
	if err != nil {
		return fmt.Errorf("dynamic client registration with redirect %q: %w", *redirect, err)
	}
	fmt.Printf("\n=== Q3: DCR ===\nACCEPTED redirect %q\nclient_id: %s\nauth method: %s\n",
		*redirect, res.ClientID, orAbsent(res.TokenEndpointAuthMethod))
	return nil
}

// challengeMetadataURL sends an unauthenticated initialize and extracts the
// resource_metadata parameter from the 401's WWW-Authenticate header.
func challengeMetadataURL(ctx context.Context, hc *http.Client) (string, error) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
		`{"protocolVersion":"2025-06-18","capabilities":{},` +
		`"clientInfo":{"name":"oauthprobe","version":"0"}}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, *serverURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("initialize: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		return "", fmt.Errorf("expected 401 to start discovery, got %d", resp.StatusCode)
	}
	challenges, err := oauthex.ParseWWWAuthenticate(resp.Header.Values("WWW-Authenticate"))
	if err != nil {
		return "", fmt.Errorf("parse WWW-Authenticate: %w", err)
	}
	for _, c := range challenges {
		if u := c.Params["resource_metadata"]; u != "" {
			return u, nil
		}
	}
	return "", fmt.Errorf("401 carried no resource_metadata parameter: %v", challenges)
}

func dump(label string, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Printf("\n=== %s ===\n%s\n", label, b)
}

func orAbsent(s string) string {
	if s == "" {
		return "(absent)"
	}
	return s
}
