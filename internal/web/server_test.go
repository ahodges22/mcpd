package web

import (
	"context"
	"testing"
	"time"

	"github.com/ahodges22/mcpd/internal/testfake"
)

// Only one of the two ways a wait can end says anything about the stored grant, and telling
// them apart is what stops the authorize route destroying an authorization in progress.
//
// This is the distinction that took a real backend down. The wait's context is the request's,
// so a browser navigating to the consent screen in the same tab cancels it. Reading that as a
// failed handshake made the route discard the grant and reconnect, and that reconnect's
// teardown abandoned the authorization the user was about to complete: the code their browser
// came back with then matched no outstanding request, leaving a live pending URL beside an
// "abandoned: context canceled" error from two different attempts. Our own budget expiring is
// different and must still retry, because that is how a genuinely unusable stored grant is
// recovered from in a single click.
func TestOnlyOurOwnBudgetExpiringIsEvidenceAgainstTheStoredGrant(t *testing.T) {
	h := newHarness(t, testfake.New("alpha", tool("kubectl_logs")))
	b, ok := h.reg.Get("alpha")
	if !ok {
		t.Fatal("alpha is not registered")
	}

	// Cancelled, as a request the browser abandoned is.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, got := h.server.awaitAuthorization(cancelled, b, "alpha", b.Generation()); got != authorizeUnfinished {
		t.Errorf("an abandoned request = %v, want authorizeUnfinished: retrying on this tears down a live authorization", got)
	}

	// Expired, as our own budget is when the provider is merely slow.
	expired, stop := context.WithTimeout(t.Context(), time.Nanosecond)
	defer stop()
	<-expired.Done()
	if _, got := h.server.awaitAuthorization(expired, b, "alpha", b.Generation()); got != authorizeUnusable {
		t.Errorf("our own budget expiring = %v, want authorizeUnusable: a grant that never works would need a second click", got)
	}
}

// A handshake the user is waiting on in a browser gets a budget a person can meet. Bounding it
// by the ordinary dial budget abandons the authorization while the consent screen is still
// open, and a provider that makes the user log in first needs minutes rather than the sixty
// seconds a backend declaring no timeout would otherwise allow.
func TestAnInteractiveHandshakeIsGivenAHumanSizedBudget(t *testing.T) {
	h := newHarness(t, testfake.New("alpha", tool("kubectl_logs")))
	b, ok := h.reg.Get("alpha")
	if !ok {
		t.Fatal("alpha is not registered")
	}
	ordinary := b.ConnectTimeout()

	b.ExpectAuthorization()
	interactive := b.ConnectTimeout()

	if interactive <= ordinary {
		t.Errorf("interactive budget %s is not longer than the ordinary %s", interactive, ordinary)
	}
	if interactive < 2*time.Minute {
		t.Errorf("interactive budget is %s, which is not long enough for a login and a consent", interactive)
	}
}
