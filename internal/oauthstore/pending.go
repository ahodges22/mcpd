package oauthstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// pendingWindow bounds how long a code fetch waits for the user. It is a
// backstop: the fetch runs inside a backend's handshake, so that backend's
// connect timeout is what usually ends the wait first.
const pendingWindow = 5 * time.Minute

// ErrNoPending reports a callback that matches no outstanding authorization,
// which is what a forged, stale or replayed one looks like.
var ErrNoPending = errors.New("no outstanding authorization for this state")

// ErrIssuerMismatch reports a callback claiming to come from an authorization server other
// than the one the user was sent to, which is the mix-up RFC 9207's iss parameter exists to
// detect. The code is refused rather than delivered.
var ErrIssuerMismatch = errors.New("the authorization response names a different authorization server")

// pending is one authorization waiting on the browser.
type pending struct {
	server string
	url    string
	// origin is the scheme and host the user was sent to, which is what a returned iss is
	// checked against.
	origin string
	// issAdvertised is whether the authorization server's own metadata said it would return
	// an iss parameter, and issKnown whether that metadata could be read at all. Both are
	// resolved when the authorization is published, because the callback that needs them
	// arrives on a route with no idea which provider it belongs to.
	issAdvertised bool
	issKnown      bool
	result        chan outcome
}

// outcome is what the callback route hands the waiting handshake: a code, or the reason the
// callback was refused. Carrying the refusal matters because the state has already been
// consumed by then, so a fetch left waiting would sit there until its budget expired and
// report a timeout instead of the reason.
type outcome struct {
	result *auth.AuthorizationResult
	err    error
}

// forwardedIss decides what iss to hand the SDK, and refuses a callback that names the wrong
// authorization server.
//
// The SDK requires iss to be present exactly when the authorization server advertised
// support for it, which is what RFC 9207 says. One real provider sends iss without
// advertising it, and the SDK then rejects a consent the user has already given: the code
// arrives, the authorization is refused before any exchange, and the flow silently starts
// over. Presenting no iss to the SDK in that case is sound because the check it would have
// performed is done here first, against the origin the user was actually sent to, which is
// the property RFC 9207 provides. A mismatch is refused outright, which is stricter than the
// SDK is for a non-advertising server: it never sees the code at all.
func (p *pending) forwardedIss(iss string) (string, error) {
	if iss == "" {
		return "", nil
	}
	if !sameOrigin(iss, p.origin) {
		return "", fmt.Errorf("%w: %s claims %q but the user was sent to %s",
			ErrIssuerMismatch, p.server, iss, p.origin)
	}
	if p.issKnown && !p.issAdvertised {
		slog.Info("the provider returned an iss it never advertised; verified here and not forwarded",
			"server", p.server, "iss", iss)
		return "", nil
	}
	return iss, nil
}

// sameOrigin compares scheme and host, which is what identifies an authorization server. A
// path difference is not a different server: the issuer is an origin and the authorization
// endpoint lives under it.
func sameOrigin(rawIssuer, origin string) bool {
	parsed, err := url.Parse(rawIssuer)
	if err != nil {
		return false
	}
	return originOf(parsed) == origin
}

func originOf(u *url.URL) string { return u.Scheme + "://" + u.Host }

// issSupport reads whether the authorization server behind an authorization URL says it
// returns an iss parameter. Unreadable metadata leaves it unknown, and an unknown answer
// forwards whatever the provider sent, which is the behaviour that predates this check.
func (s *Store) issSupport(ctx context.Context, authURL *url.URL) (known, advertised bool) {
	issuer := originOf(authURL)
	asm, err := auth.GetAuthServerMetadata(ctx, issuer, s.client)
	if err != nil || asm == nil {
		slog.Debug("could not read authorization server metadata", "issuer", issuer, "error", err)
		return false, false
	}
	return true, asm.AuthorizationResponseIssParameterSupported
}

// fetchCode publishes the authorization URL, then blocks until the browser comes
// back through the callback route.
//
// It returns promptly when ctx is done, and that is what keeps the kill switch
// working: the fetch runs inside Backend.connect, which holds the backend's
// lifecycle lock for its whole duration, and Registry.Disable reaches this
// goroutine only by cancelling that context before it takes that lock.
func (s *Store) fetchCode(server string) auth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		parsed, err := url.Parse(args.URL)
		if err != nil {
			return nil, fmt.Errorf("parse authorization URL for %s: %w", server, err)
		}
		state := parsed.Query().Get("state")
		if state == "" {
			return nil, fmt.Errorf("authorization URL for %s carries no state", server)
		}
		p := &pending{
			server: server, url: args.URL, origin: originOf(parsed),
			result: make(chan outcome, 1),
		}
		// Resolved before the authorization is published, because the callback route that
		// needs the answer knows only a state nonce and cannot work out which provider it
		// belongs to. It is one request to the host the user is about to be sent to anyway.
		p.issKnown, p.issAdvertised = s.issSupport(ctx, parsed)
		// Recorded before the authorization is published, not after: publishing is what
		// releases anyone waiting for the URL, so a health record written afterwards can
		// be read stale by the very page the URL was published for.
		s.needsAuth(server, "authorize at "+args.URL)
		s.publish(state, p)
		// The provider's host and the deadline, never the URL: it carries the state nonce
		// and the PKCE challenge, and this log is the one place that would put them
		// somewhere the journal keeps them.
		deadline, hasDeadline := ctx.Deadline()
		slog.Info("authorization waiting on the browser",
			"server", server, "provider", parsed.Host,
			"budget", budgetOf(deadline, hasDeadline))

		timer := time.NewTimer(pendingWindow)
		defer timer.Stop()
		select {
		case out := <-p.result:
			if out.err != nil {
				s.withdraw(state)
				return nil, out.err
			}
			slog.Info("authorization code received", "server", server)
			return out.result, nil
		case <-ctx.Done():
			s.withdraw(state)
			// Logged as a warning because it is nearly always something the user can act
			// on: the handshake budget ran out while they were still at the provider, or a
			// lifecycle transition cancelled it underneath them.
			slog.Warn("authorization abandoned before the browser came back",
				"server", server, "cause", ctx.Err())
			return nil, fmt.Errorf("authorization for %s was abandoned: %w", server, ctx.Err())
		case <-timer.C:
			s.withdraw(state)
			slog.Warn("authorization timed out with no callback", "server", server, "after", pendingWindow)
			return nil, fmt.Errorf("authorization for %s timed out after %s with no callback", server, pendingWindow)
		}
	}
}

// Deliver hands a callback's authorization code to the fetch waiting on state.
// The state is consumed here and nowhere else, so a replayed callback matches
// nothing and is refused.
func (s *Store) Deliver(state, code, iss string) error {
	if state == "" || code == "" {
		return ErrNoPending
	}
	s.mu.Lock()
	p := s.pending[state]
	delete(s.pending, state)
	s.mu.Unlock()
	if p == nil {
		// The common cause is an authorization that was abandoned while the user was at the
		// provider, so the code they came back with belongs to a request that no longer
		// exists. Neither the state nor the code is logged.
		slog.Warn("callback matched no outstanding authorization; the request it belongs to is gone")
		return ErrNoPending
	}
	forwarded, err := p.forwardedIss(iss)
	if err != nil {
		slog.Warn("callback refused", "server", p.server, "error", err)
		// Handed to the waiting handshake as well as returned here, so it fails with this
		// reason now rather than with a timeout once its budget runs out. The state is
		// already consumed either way, so there is nothing left to retry against.
		p.result <- outcome{err: err}
		return err
	}
	slog.Info("callback matched an outstanding authorization", "server", p.server)
	p.result <- outcome{result: &auth.AuthorizationResult{Code: code, State: state, Iss: forwarded}}
	return nil
}

// budgetOf renders how long the handshake will keep waiting, so a log line says whether the
// window the user has is a machine-sized one or a person-sized one.
func budgetOf(deadline time.Time, ok bool) string {
	if !ok {
		return "unbounded"
	}
	return time.Until(deadline).Round(time.Second).String()
}

// Pending reports the URL an outstanding authorization for server is waiting on. The
// authenticate action polls it, because it has to wait for either this or the backend
// coming up and only one of those two can be subscribed to. Polling also means
// pressing the button twice hands back the same URL rather than cancelling the
// authorization the user is part way through.
func (s *Store) Pending(server string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingLocked(server)
}

func (s *Store) pendingLocked(server string) (string, bool) {
	for _, p := range s.pending {
		if p.server == server {
			return p.url, true
		}
	}
	return "", false
}

func (s *Store) publish(state string, p *pending) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[state] = p
}

func (s *Store) withdraw(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, state)
}
