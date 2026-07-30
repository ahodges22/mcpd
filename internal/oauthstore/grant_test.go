package oauthstore

import "testing"

// The grant state decides whether a 401 may start a browser authorization, and its two
// facts move independently. These are white-box because they are the store's own
// bookkeeping: the orderings below are reachable in production but not stageable
// through the HTTP surface, since they turn on which of two concurrent things wrote
// first.
func TestAUserRequestOutranksALateRefreshFailure(t *testing.T) {
	// Reachable ordering: AllowAuthorization drops the cached handler but the live
	// transport still holds the old wrapper, so an in-flight request on the session
	// being torn down can fail its refresh after the user has clicked. Clobbering the
	// request there answers the click with a refusal to authorize.
	s := New(t.TempDir(), "http://127.0.0.1:1/oauth/callback", Hooks{})
	s.rejectGrant("notion")
	s.AllowAuthorization("notion")
	s.rejectGrant("notion")

	if !s.permitAuthorization("notion") {
		t.Error("a refresh failure cancelled the authorization the user asked for")
	}
}

func TestARecoveredGrantStopsBlockingAuthorization(t *testing.T) {
	// A transient refusal must not outlive itself: nothing clears the rejection except
	// a token the provider honours, and without that one blip blocks automatic
	// re-authorization for the life of the process.
	s := New(t.TempDir(), "http://127.0.0.1:1/oauth/callback", Hooks{})
	s.rejectGrant("notion")
	if s.permitAuthorization("notion") {
		t.Fatal("a rejected grant authorized itself with no user request")
	}

	s.grantWorks("notion")
	if !s.permitAuthorization("notion") {
		t.Error("a grant the provider honoured again is still blocked, so a blip is permanent")
	}
}
