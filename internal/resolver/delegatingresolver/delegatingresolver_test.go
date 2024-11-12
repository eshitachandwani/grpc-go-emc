package delegatingresolver

import (
	"testing"

	"google.golang.org/grpc/internal/testutils"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
	"google.golang.org/grpc/serviceconfig"
)

// TestDelegatingResolverNoProxy verifies the behavior of the delegating resolver when no proxy is configured.
func TestDelegatingResolverNoProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "")               // Explicitely set proxy enviornment to empty to mimic no proxy enviornment set
	mr := manual.NewBuilderWithScheme("test") // Set up a manual resolver to control the address resolution.
	target := "test:///localhost:1234"

	stateCh := make(chan resolver.State, 1)
	updateStateF := func(s resolver.State) error {
		select {
		case stateCh <- s:
		default:
		}
		return nil
	}

	errCh := make(chan error, 1)
	reportErrorF := func(err error) {
		select {
		case errCh <- err:
		default:
		}
	}

	tcc := &testutils.ResolverClientConn{Logger: t, UpdateStateF: updateStateF, ReportErrorF: reportErrorF}
	// Create a delegating resolver with no proxy configuration
	dr, err := New(resolver.Target{URL: *testutils.MustParseURL(target)}, tcc, resolver.BuildOptions{}, mr)
	if err != nil || dr == nil {
		t.Fatalf("Failed to create delegating resolver: %v", err)
	}

	// Update the manual resolver with a test address.
	mr.UpdateState(resolver.State{Addresses: []resolver.Address{{Addr: "test-addr"}}, ServiceConfig: &serviceconfig.ParseResult{}})

	// Verify that the delegating resolver outputs the same address.
	select {
	case state := <-stateCh:
		if len(state.Addresses) != 1 || state.Addresses[0].Addr != "test-addr" {
			t.Errorf("Unexpected address from delegating resolver: %v, want [test-addr]", state.Addresses)
		}
	default:
	}
}
