package appmeta

import (
	"net/url"
	"testing"
)

func TestClaimURLIdentity(t *testing.T) {
	t.Parallel()
	claim := &url.URL{
		Scheme: ClaimURLScheme,
		Host:   ClaimURLHost,
	}
	if got, want := claim.String(), "axon-pulse://claim"; got != want {
		t.Fatalf("claim URL identity = %q, want %q", got, want)
	}
}
