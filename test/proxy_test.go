/*
 *
 * Copyright 2024 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/internal/envconfig"
	"google.golang.org/grpc/internal/resolver/delegatingresolver"
	"google.golang.org/grpc/internal/stubserver"
	"google.golang.org/grpc/internal/testutils"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	testpb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
)

const (
	unresProxyURI = "example.com"

// defaultTestTimeout = 10 * time.Second
)

// overwriteAndRestore overwrite function httpProxyFromEnvironment and
// returns a function to restore the default values.
func overwrite(hpfe func(req *http.Request) (*url.URL, error)) func() {
	backHPFE := delegatingresolver.HTTPSProxyFromEnvironment
	delegatingresolver.HTTPSProxyFromEnvironment = hpfe
	return func() {
		delegatingresolver.HTTPSProxyFromEnvironment = backHPFE
	}
}

func overwriteAndRestoreProxyEnv(proxyAddr string) func() {
	origHTTPSProxy := envconfig.HTTPSProxy
	envconfig.HTTPSProxy = proxyAddr
	return func() { envconfig.HTTPSProxy = origHTTPSProxy }
}

func createAndStartBackendServer(t *testing.T) string {
	backend := &stubserver.StubServer{
		EmptyCallF: func(context.Context, *testpb.Empty) (*testpb.Empty, error) { return &testpb.Empty{}, nil },
	}
	if err := backend.StartServer(); err != nil {
		t.Fatalf("Failed to start backend: %v", err)
	}
	t.Logf("Started TestService backend at: %q", backend.Address)
	t.Cleanup(func() { backend.Stop() })
	return backend.Address
}

// TestGrpcDialWithProxy tests grpc.Dial with no resolver mentioned in targetURI
// (i.e it uses passthrough) with a proxy.
func (s) TestGrpcDialWithProxy(t *testing.T) {
	// Set up a channel to receive signals from OnClientResolution.
	resolutionCh := make(chan bool, 1)
	// Overwrite OnClientResolution to send a signal to the channel.
	origOnClientResolution := delegatingresolver.OnClientResolution // Access using package name
	delegatingresolver.OnClientResolution = func(int) {             // Access using package name
		resolutionCh <- true
	}
	t.Cleanup(func() { delegatingresolver.OnClientResolution = origOnClientResolution }) // Restore using package name
	//Create and start a backend server
	backendAddr := createAndStartBackendServer(t)
	proxyLis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	errCh := make(chan error, 1)
	doneCh := make(chan struct{})
	defer func() { close(errCh) }()
	reqCheck := func(req *http.Request) error {
		if req.Method != http.MethodConnect {
			return fmt.Errorf("unexpected Method %q, want %q", req.Method, http.MethodConnect)
		}
		if req.URL.Host != unresProxyURI {
			return fmt.Errorf("unexpected URL.Host %q, want %q", req.URL.Host, unresProxyURI)
		}
		return nil
	}
	fmt.Printf("backendaddr sent: %v\n", backendAddr)
	p := testutils.NewProxyServer(proxyLis, reqCheck, errCh, doneCh, backendAddr, false, func() {})
	t.Cleanup(func() { p.Stop() })

	//set proxy env
	defer overwriteAndRestoreProxyEnv("proxyexample.com")()
	// Overwrite the function in the test and restore them in defer.
	hpfe := func(req *http.Request) (*url.URL, error) {
		if req.URL.Host == unresProxyURI {
			return &url.URL{
				Scheme: "https",
				Host:   proxyLis.Addr().String(), //emchandwani: return unresolved string here, and add manual resolver to check the correct resolution
			}, nil
		}
		return nil, nil
	}
	defer overwrite(hpfe)()
	// Dial to proxy server.
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	fmt.Printf("env config: %v\n", os.Getenv("HTTPS_PROXY"))
	conn, err := grpc.Dial(unresProxyURI, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.Dial failed: %v", err)
	}
	defer conn.Close()
	// Send an RPC to the backend through the proxy.
	client := testpb.NewTestServiceClient(conn)
	if _, err := client.EmptyCall(ctx, &testpb.Empty{}); err != nil {
		t.Fatalf("EmptyCall failed: %v", err)
	}
	// Check if the proxy encountered any errors.
	select {
	case err := <-errCh:
		t.Fatalf("proxy server encountered an error: %v", err)
	case <-doneCh:
		t.Logf("proxy server succeeded")
	}
	select {
	case <-resolutionCh:
		// Success: OnClientResolution was called.
	default:
		t.Error("Client side resolution should be called but isn't")
	}
}

// setupDNS unregisters the DNS resolver and registers a manual resolver for the
// same scheme. This allows the test to mock the DNS resolution for the proxy resolver.
func setupDNS(t *testing.T) *manual.Resolver {
	mr := manual.NewBuilderWithScheme("dns")
	dnsResolverBuilder := resolver.Get("dns")

	resolver.Register(mr)

	t.Cleanup(func() { resolver.Register(dnsResolverBuilder) })
	return mr
}

// TestGrpcDialWithProxy tests grpc.Dial with dns resolver mentioned in targetURI
// (i.e it uses passthrough) with a proxy. DNS resolution should be done, to be compatible with the old behaviour.
func (s) TestGrpcDialWithProxyandResolution(t *testing.T) {

	//Create and start a backend server
	backendAddr := createAndStartBackendServer(t)

	proxyLis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	errCh := make(chan error, 1)
	doneCh := make(chan struct{})
	defer func() { close(errCh) }()
	reqCheck := func(req *http.Request) error {
		if req.Method != http.MethodConnect {
			return fmt.Errorf("unexpected Method %q, want %q", req.Method, http.MethodConnect)
		}
		//emchandwani: TODO :Change to the correct string
		if req.URL.Host != unresProxyURI {
			fmt.Printf("unexpected URL.Host %q, want %q", req.URL.Host, backendAddr)

			return fmt.Errorf("unexpected URL.Host %q, want %q", req.URL.Host, backendAddr)
		}
		return nil
	}
	fmt.Printf("backendaddr sent: %v\n", backendAddr)
	p := testutils.NewProxyServer(proxyLis, reqCheck, errCh, doneCh, backendAddr, false, func() {})
	t.Cleanup(func() { p.Stop() })

	// set proxy env variable
	defer overwriteAndRestoreProxyEnv("proxyexample.com")()

	// Overwrite the function in the test and restore them in defer.
	hpfe := func(req *http.Request) (*url.URL, error) {
		if req.URL.Host == unresProxyURI {
			return &url.URL{
				Scheme: "https",
				Host:   "proxyURL.com", //returning unresolved string here, and add manual resolver to check the correct resolution
			}, nil
		}
		return nil, nil
	}
	defer overwrite(hpfe)()

	mrProxy := setupDNS(t)
	// Dial to proxy server.
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	fmt.Printf("env config: %v\n", os.Getenv("HTTPS_PROXY"))
	conn, err := grpc.Dial(unresProxyURI, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.Dial failed: %v", err)
	}
	// mrTarget.UpdateState(resolver.State{
	// 	Addresses: []resolver.Address{
	// 		{Addr: backendAddr},
	// 	}})

	mrProxy.UpdateState(resolver.State{
		Addresses: []resolver.Address{
			{Addr: proxyLis.Addr().String()},
		}})
	defer conn.Close()
	// Send an RPC to the backend through the proxy.
	client := testpb.NewTestServiceClient(conn)
	if _, err := client.EmptyCall(ctx, &testpb.Empty{}); err != nil {
		t.Errorf("EmptyCall failed: %v", err)
	}
	// Check if the proxy encountered any errors.
	select {
	case err := <-errCh:
		t.Fatalf("proxy server encountered an error: %v", err)
	default:
		t.Logf("proxy server succeeded")
	}

}

// TestGrpcNewClientWithProxy tests grpc.NewClient with no resolver mentioned in targetURI with a proxy
func (s) TestGrpcNewClientWithProxy(t *testing.T) {
	// Set up a channel to receive signals from OnClientResolution.
	resolutionCh := make(chan bool, 1)

	// Overwrite OnClientResolution to send a signal to the channel.
	origOnClientResolution := delegatingresolver.OnClientResolution // Access using package name
	delegatingresolver.OnClientResolution = func(int) {             // Access using package name
		resolutionCh <- true
	}
	t.Cleanup(func() { delegatingresolver.OnClientResolution = origOnClientResolution }) // Restore using package name
	//Create and start a backend server
	backendAddr := createAndStartBackendServer(t)
	proxyLis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	errCh := make(chan error, 1)
	doneCh := make(chan struct{})
	defer func() { close(errCh) }()
	reqCheck := func(req *http.Request) error {
		if req.Method != http.MethodConnect {
			return fmt.Errorf("unexpected Method %q, want %q", req.Method, http.MethodConnect)
		}
		if req.URL.Host != unresProxyURI {
			return fmt.Errorf("unexpected URL.Host %q, want %q", req.URL.Host, backendAddr)
		}
		return nil
	}
	fmt.Printf("backendaddr sent: %v\n", backendAddr)
	p := testutils.NewProxyServer(proxyLis, reqCheck, errCh, doneCh, backendAddr, false, func() {})
	t.Cleanup(func() { p.Stop() })

	// set proxy env variable
	defer overwriteAndRestoreProxyEnv("proxyexample.com")()

	// Overwrite the function in the test and restore them in defer.
	hpfe := func(req *http.Request) (*url.URL, error) {
		if req.URL.Host == unresProxyURI {
			return &url.URL{
				Scheme: "https",
				Host:   proxyLis.Addr().String(), //emchandwani: return unresolved string here, and add manual resolver to check the correct resolution
			}, nil
		}
		return nil, nil
	}
	defer overwrite(hpfe)()

	// Dial to proxy server.
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	fmt.Printf("env config: %v\n", os.Getenv("HTTPS_PROXY"))
	conn, err := grpc.NewClient(unresProxyURI, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.Dial failed: %v", err)
	}
	defer conn.Close()
	// Send an RPC to the backend through the proxy.
	client := testpb.NewTestServiceClient(conn)
	if _, err := client.EmptyCall(ctx, &testpb.Empty{}); err != nil {
		t.Errorf("EmptyCall failed: %v", err) //t.Errorf
	}
	// Check if the proxy encountered any errors.
	//emchandwani : put in defer block where channel is created.
	select {
	case err := <-errCh:
		t.Fatalf("proxy server encountered an error: %v", err)
	default:
		t.Logf("proxy server succeeded")
	}
	select {
	case <-resolutionCh:
		// Success: OnClientResolution was called.
		t.Error("Client side resolution called")
	default:
	}

}

// TestGrpcNewClientWithProxyAndCustomResolver tests grpc.NewClient with
// resolver other than "dns" mentioned in targetURI with a proxy.
func (s) TestGrpcNewClientWithProxyAndCustomResolver(t *testing.T) {
	// Set up a channel to receive signals from OnClientResolution.
	resolutionCh := make(chan bool, 1)

	// Overwrite OnClientResolution to send a signal to the channel.
	origOnClientResolution := delegatingresolver.OnClientResolution // Access using package name
	delegatingresolver.OnClientResolution = func(int) {             // Access using package name
		resolutionCh <- true
	}
	t.Cleanup(func() { delegatingresolver.OnClientResolution = origOnClientResolution }) // Restore using package name
	//Create and start a backend server
	r := manual.NewBuilderWithScheme("whatever")
	resolver.Register(r)
	backendAddr := createAndStartBackendServer(t)
	proxyLis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	errCh := make(chan error, 1)
	doneCh := make(chan struct{})
	defer func() { close(errCh) }()
	reqCheck := func(req *http.Request) error {
		if req.Method != http.MethodConnect {
			return fmt.Errorf("unexpected Method %q, want %q", req.Method, http.MethodConnect)
		}
		if req.URL.Host != backendAddr {
			return fmt.Errorf("unexpected URL.Host %q, want %q", req.URL.Host, backendAddr)
		}
		return nil
	}
	fmt.Printf("backendaddr sent: %v\n", backendAddr)
	p := testutils.NewProxyServer(proxyLis, reqCheck, errCh, doneCh, backendAddr, true, func() {})
	t.Cleanup(func() { p.Stop() })

	// set proxy env variable
	defer overwriteAndRestoreProxyEnv("proxyexample.com")()

	// Overwrite the function in the test and restore them in defer.
	hpfe := func(req *http.Request) (*url.URL, error) {
		fmt.Printf("req.URL.Host: %v\n", req.URL.Host)
		if req.URL.Host == unresProxyURI {
			return &url.URL{
				Scheme: "https",
				Host:   proxyLis.Addr().String(),
			}, nil
		}
		return nil, nil
	}
	defer overwrite(hpfe)()
	r.InitialState(resolver.State{Addresses: []resolver.Address{{Addr: backendAddr}}})
	// mrTarget := manual.NewBuilderWithScheme("whatever")
	dopts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		// grpc.WithResolvers(r),
	}
	// Dial to proxy server.
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	// At this point, the resolver has not returned any addresses to the channel.
	// This RPC must block until the context expires.

	fmt.Printf("env config: %v\n", os.Getenv("HTTPS_PROXY"))
	conn, err := grpc.NewClient(r.Scheme()+":///"+unresProxyURI, dopts...)
	if err != nil {
		t.Fatalf("grpc.NewClient() failed: %v", err)
	}

	t.Cleanup(func() { conn.Close() })

	client := testgrpc.NewTestServiceClient(conn)
	// sCtx, sCancel := context.WithTimeout(context.Background(), defaultTestShortTimeout)
	// defer sCancel()
	// r.UpdateState(resolver.State{Addresses: []resolver.Address{{Addr: backendAddr}}})

	if _, err := client.EmptyCall(ctx, &testpb.Empty{}); err != nil {
		t.Errorf("EmptyCall() failed: %v", err)
	}
	// Check if the proxy encountered any errors.
	select {
	case err := <-errCh:
		t.Fatalf("proxy server encountered an error: %v", err)
	default:
		t.Logf("proxy server succeeded")
	}
	// Check if client-side resolution signal was sent to the channel.
	select {
	case <-resolutionCh:
		// Success: OnClientResolution was called.
	default:
		t.Error("Client side resolution should be called but isn't")
	}
}

// TestGrpcNewClientWithProxyAndTargetResoltionEnabled tests grpc.NewClient with
// default resolver i.e. "dns" with dialoption for target resolution on client
// enabled.
func TestGrpcNewClientWithProxyAndTargetResoltionEnabled(t *testing.T) {
	//Create and start a backend server
	// Set up a channel to receive signals from OnClientResolution.
	resolutionCh := make(chan bool, 1)
	proxyScheme := delegatingresolver.ProxyScheme
	delegatingresolver.ProxyScheme = "whatever"
	t.Cleanup(func() {
		delegatingresolver.ProxyScheme = proxyScheme
	})
	// Overwrite OnClientResolution to send a signal to the channel.
	origOnClientResolution := delegatingresolver.OnClientResolution // Access using package name
	delegatingresolver.OnClientResolution = func(int) {             // Access using package name
		resolutionCh <- true
	}
	t.Cleanup(func() { delegatingresolver.OnClientResolution = origOnClientResolution }) // Restore using package name
	backendAddr := createAndStartBackendServer(t)

	proxyLis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	errCh := make(chan error, 1)
	doneCh := make(chan struct{})
	defer func() { close(errCh) }()
	reqCheck := func(req *http.Request) error {
		if req.Method != http.MethodConnect {
			return fmt.Errorf("unexpected Method %q, want %q", req.Method, http.MethodConnect)
		}
		if req.URL.Host != backendAddr {
			return fmt.Errorf("unexpected URL.Host %q, want %q", req.URL.Host, backendAddr)
		}
		return nil
	}
	fmt.Printf("backendaddr sent: %v\n", backendAddr)
	fmt.Printf("proxy addr : %v\n", proxyLis.Addr().String())
	p := testutils.NewProxyServer(proxyLis, reqCheck, errCh, doneCh, backendAddr, true, func() {})
	t.Cleanup(func() { p.Stop() })
	pr := manual.NewBuilderWithScheme("whatever")
	resolver.Register(pr)
	pr.InitialState(resolver.State{Addresses: []resolver.Address{{Addr: proxyLis.Addr().String()}}})
	r := setupDNS(t)
	r.InitialState(resolver.State{Addresses: []resolver.Address{{Addr: backendAddr}}})
	// set proxy env variable
	defer overwriteAndRestoreProxyEnv("proxyexample.com")()

	// Overwrite the function in the test and restore them in defer.
	hpfe := func(req *http.Request) (*url.URL, error) {
		fmt.Printf("req.URL.Host: %v\n", req.URL.Host)
		if req.URL.Host == unresProxyURI {
			return &url.URL{
				Scheme: "https",
				Host:   "proxyLis.Addr().String()",
			}, nil
		}
		return nil, nil
	}
	defer overwrite(hpfe)()

	// mrTarget := manual.NewBuilderWithScheme("whatever")
	dopts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithTargetResolutionEnabled(),
		// grpc.WithResolvers(r),
	}
	// Dial to proxy server.
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	// At this point, the resolver has not returned any addresses to the channel.
	// This RPC must block until the context expires.

	fmt.Printf("env config: %v\n", os.Getenv("HTTPS_PROXY"))
	conn, err := grpc.NewClient(unresProxyURI, dopts...)
	if err != nil {
		t.Fatalf("grpc.NewClient() failed: %v", err)
	}

	t.Cleanup(func() { conn.Close() })

	client := testgrpc.NewTestServiceClient(conn)
	// sCtx, sCancel := context.WithTimeout(context.Background(), defaultTestShortTimeout)
	// defer sCancel()
	// r.UpdateState(resolver.State{Addresses: []resolver.Address{{Addr: backendAddr}}})

	if _, err := client.EmptyCall(ctx, &testpb.Empty{}); err != nil {
		t.Errorf("EmptyCall() failed: %v", err)
	}
	// Check if the proxy encountered any errors.
	select {
	case err := <-errCh:
		t.Fatalf("proxy server encountered an error: %v", err)
	default:
		t.Logf("proxy server succeeded")
	}
	select {
	case <-resolutionCh:
		// Success: OnClientResolution was called.
	default:
		t.Error("Client side resolution should be called but isn't")
	}
}

// TestGrpcNewClientWithNoProxy tests grpc.NewClient with grpc.WithNoProxy() set.
func (s) TestGrpcNewClientWithNoProxy(t *testing.T) {
	// Set up a channel to receive signals from OnClientResolution.
	delegatingCh := make(chan bool, 1)

	// Overwrite OnClientResolution to send a signal to the channel.
	origOnDelegatingResolverCalled := delegatingresolver.OnDelegatingResolverCalled // Access using package name
	delegatingresolver.OnDelegatingResolverCalled = func(int) {                     // Access using package name
		delegatingCh <- true
	}
	t.Cleanup(func() { delegatingresolver.OnDelegatingResolverCalled = origOnDelegatingResolverCalled }) // Restore using package name
	// Create a manual resolver to control address resolution.  We don't expect this to be used.
	// mr := manual.NewBuilderWithScheme("test")
	backendAddr := createAndStartBackendServer(t)
	proxyLis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	errCh := make(chan error, 1)
	doneCh := make(chan struct{})
	defer func() { close(errCh) }()
	reqCheck := func(req *http.Request) error {
		if req.Method != http.MethodConnect {
			return fmt.Errorf("unexpected Method %q, want %q", req.Method, http.MethodConnect)
		}
		if req.URL.Host != backendAddr {
			return fmt.Errorf("unexpected URL.Host %q, want %q", req.URL.Host, backendAddr)
		}
		return nil
	}
	fmt.Printf("backendaddr sent: %v\n", backendAddr)
	fmt.Printf("proxy addr : %v\n", proxyLis.Addr().String())
	proxyStarted := make(chan struct{})
	p := testutils.NewProxyServer(proxyLis, reqCheck, errCh, doneCh, backendAddr, false, func() { close(proxyStarted) }) // Signal that the proxy has started})
	t.Cleanup(func() { p.Stop() })
	// Set a proxy environment variable. This should be ignored because of WithNoProxy().
	restoreProxyEnv := overwriteAndRestoreProxyEnv("envProxyAddr")
	defer restoreProxyEnv()

	// Overwrite HTTPSProxyFromEnvironment. We don't expect this to be called
	// because WithNoProxy() should bypass delegating resolver which
	// unconditionally calls HTTPSProxyfromenvironment .
	hpfe := func(req *http.Request) (*url.URL, error) {
		t.Errorf("Unexpected call to HTTPSProxyFromEnvironment") // Fail if this is called.
		return &url.URL{
			Scheme: "https",
			Host:   "envProxyAddr",
		}, nil
	}
	defer overwrite(hpfe)()

	dopts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithNoProxy(), // Disable proxy.
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	// If we pass unresolved address to
	// grpc.NewClient then dns resolver will call mapAddress under it, so we
	// have already added an error check above in the overwrite for
	// HTTPSProxyFromEnvironment to fail, if that is called.
	conn, err := grpc.NewClient(backendAddr, dopts...)
	if err != nil {
		t.Fatalf("grpc.NewClient() failed: %v", err)
	}
	defer conn.Close()

	// Send an RPC.  If this succeeds, it means we successfully bypassed the proxy.
	client := testgrpc.NewTestServiceClient(conn)
	if _, err := client.EmptyCall(ctx, &testpb.Empty{}); err != nil {
		t.Fatalf("EmptyCall() failed: %v", err)
	}

	// Check if target resolver's UpdateState was called.  It should NOT have been
	// called, because the resolver should not have been built with WithNoProxy.
	select {
	case <-proxyStarted:
		t.Error("Proxy Server was dialled")
	default:
	}
	// Check if client-side resolution signal was sent to the channel.
	select {
	case <-delegatingCh:
		t.Error("delagting resolver called but it shouldnt.")
		// Success: OnClientResolution was called.
	default:

	}
}

// TestGrpcNewClientWithContextDialer tests grpc.NewClient with
// grpc.WithContextDialer() set.
func (s) TestGrpcNewClientWithContextDialer(t *testing.T) {
	delegatingCh := make(chan bool, 1)

	// Overwrite OnClientResolution to send a signal to the channel.
	origOnDelegatingResolverCalled := delegatingresolver.OnDelegatingResolverCalled // Access using package name
	delegatingresolver.OnDelegatingResolverCalled = func(int) {                     // Access using package name
		delegatingCh <- true
	}
	t.Cleanup(func() { delegatingresolver.OnDelegatingResolverCalled = origOnDelegatingResolverCalled })
	backendAddr := createAndStartBackendServer(t)
	proxyLis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	errCh := make(chan error, 1)
	doneCh := make(chan struct{})
	defer func() { close(errCh) }()
	reqCheck := func(req *http.Request) error {
		if req.Method != http.MethodConnect {
			return fmt.Errorf("unexpected Method %q, want %q", req.Method, http.MethodConnect)
		}
		if req.URL.Host != backendAddr {
			return fmt.Errorf("unexpected URL.Host %q, want %q", req.URL.Host, backendAddr)
		}
		return nil
	}
	fmt.Printf("backendaddr sent: %v\n", backendAddr)
	fmt.Printf("proxy addr : %v\n", proxyLis.Addr().String())
	proxyStarted := make(chan struct{})
	p := testutils.NewProxyServer(proxyLis, reqCheck, errCh, doneCh, backendAddr, true, func() { close(proxyStarted) })
	t.Cleanup(func() { p.Stop() })
	// Set a proxy environment variable.  This should be ignored.
	restoreProxyEnv := overwriteAndRestoreProxyEnv("example.com")
	defer restoreProxyEnv()

	// Overwrite HTTPSProxyFromEnvironment.  We don't expect this to be called.
	hpfe := func(req *http.Request) (*url.URL, error) {
		t.Error("Unexpected call to HTTPSProxyFromEnvironment")
		return &url.URL{
			Scheme: "https",
			Host:   "example.com",
		}, nil
	}
	defer overwrite(hpfe)()

	// Create a custom dialer that directly dials the backend.  We'll use this
	// to bypass any proxy logic.
	dialerCalled := make(chan bool, 1)
	customDialer := func(ctx context.Context, addr string) (net.Conn, error) {
		dialerCalled <- true
		return net.Dial("tcp", backendAddr)
	}

	dopts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(customDialer),
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	//Here, we are passing backendAddr to grpc.NewClient(), but dialer will
	//override this behaviour as per the logic below.
	conn, err := grpc.NewClient(backendAddr, dopts...)
	if err != nil {
		t.Fatalf("grpc.NewClient() failed: %v", err)
	}
	defer conn.Close()

	client := testgrpc.NewTestServiceClient(conn)
	if _, err := client.EmptyCall(ctx, &testpb.Empty{}); err != nil {
		t.Fatalf("EmptyCall() failed: %v", err)
	}
	select {
	case <-dialerCalled:
	default:
		t.Errorf("custom dialer was not called by grpc.NewClient()")
	}
	select {
	case <-proxyStarted:
		t.Error("Proxy Server was dialled")
	default:
	}
	select {
	case <-delegatingCh:
		t.Error("delagting resolver called but it shouldnt.")
		// Success: OnClientResolution was called.
	default:

	}
}
