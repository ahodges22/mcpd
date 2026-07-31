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

// pending is one authorization waiting on the browser.
type pending struct {
	server string
	url    string
	result chan *auth.AuthorizationResult
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
		p := &pending{server: server, url: args.URL, result: make(chan *auth.AuthorizationResult, 1)}
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
		case res := <-p.result:
			slog.Info("authorization code received", "server", server)
			return res, nil
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
	slog.Info("callback matched an outstanding authorization", "server", p.server)
	p.result <- &auth.AuthorizationResult{Code: code, State: state, Iss: iss}
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
