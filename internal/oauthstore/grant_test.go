package oauthstore

import (
	"testing"

	"github.com/ahodges/mcpd/internal/testfake"
)

// White-box because this is the store's own bookkeeping: the ordering below is
// reachable in production but not stageable through the HTTP surface, since it turns on
// which of two concurrent things wrote first.
func TestAUserRequestOutranksALateRefreshFailure(t *testing.T) {
	// Reachable ordering: AllowAuthorization drops the cached handler but the live
	// transport still holds the old wrapper, so an in-flight request on the session
	// being torn down can fail its refresh after the user has clicked. Clobbering the
	// request there answers the click with a refusal to authorize.
	s := New(t.TempDir(), "http://127.0.0.1:1/oauth/callback", testfake.PermissiveDeclarations{}, Hooks{})
	s.rejectGrant("notion")
	s.AllowAuthorization("notion")
	s.rejectGrant("notion")

	if !s.permitAuthorization("notion") {
		t.Error("a refresh failure cancelled the authorization the user asked for")
	}
}
