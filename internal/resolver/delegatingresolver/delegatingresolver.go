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

// Package delegatingresolver implements a resolver capable of resolving both
// target URIs and proxy addresses.
package delegatingresolver

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"

	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/internal"
	"google.golang.org/grpc/internal/proxyattributes"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/serviceconfig"
)

var (
	// HTTPSProxyFromEnvironment will be used and overwritten in the tests.
	httpProxyFromEnvironmentFunc = http.ProxyFromEnvironment

	logger = grpclog.Component("delegating-resolver")
)

// delegatingResolver manages both target URI and proxy address resolution by
// delegating these tasks to separate child resolvers. Essentially, it acts as
// a middleman between the gRPC ClientConn and the child resolvers.
//
// It implements the [resolver.Resolver] interface.
type delegatingResolver struct {
	target         resolver.Target     // parsed target URI to be resolved
	cc             resolver.ClientConn // gRPC ClientConn
	targetResolver resolver.Resolver   // resolver for the target URI, based on its scheme
	proxyResolver  resolver.Resolver   // resolver for the proxy URI; nil if no proxy is configured

	mu             sync.Mutex          // protects access to the resolver state and addresses during updates
	targetAddrs    []resolver.Address  // resolved or unresolved target addresses, depending on proxy configuration
	targetEndpoint []resolver.Endpoint // resolved target endpoint
	proxyAddrs     []resolver.Address  // resolved proxy addresses; empty if no proxy is configured
	proxyEndpoint  []resolver.Endpoint // resolved proxy endpoints; empty if no proxy is configured

	proxyURL            *url.URL // proxy URL, derived from proxy environment and target
	targetResolverReady bool     // indicates if an update from the target resolver has been received
	proxyResolverReady  bool     // indicates if an update from the proxy resolver has been received
}

// parsedURLForProxy determines the proxy URL for the given address based on
// the environment. It can return the following:
//   - nil URL, nil error: No proxy is configured or the address is excluded
//     using the `NO_PROXY` environment variable or if req.URL.Host is
//     "localhost" (with or without // a port number)
//   - nil URL, non-nil error: An error occurred while retrieving the proxy URL.
//   - non-nil URL, nil error: A proxy is configured, and the proxy URL was
//     retrieved successfully without any errors.
func parsedURLForProxy(address string) (*url.URL, error) {
	proxyFunc := httpProxyFromEnvironmentFunc
	if pf := internal.HTTPSProxyFromEnvironmentForTesting; pf != nil {
		proxyFunc = pf.(func(req *http.Request) (*url.URL, error))
	}

	req := &http.Request{URL: &url.URL{
		Scheme: "https",
		Host:   address,
	}}
	url, err := proxyFunc(req)
	if err != nil {
		return nil, err
	}
	return url, nil
}

// New creates a new delegating resolver that can create up to two child
// resolvers:
//   - one to resolve the proxy address specified using the supported
//     environment variables. This uses the registered resolver for the "dns"
//     scheme.
//   - one to resolve the target URI using the resolver specified by the scheme
//     in the target URI or specified by the user using the WithResolvers dial
//     option. As a special case, if the target URI's scheme is "dns" and a
//     proxy is specified using the supported environment variables, the target
//     URI's path portion is used as the resolved address unless target
//     resolution is enabled using the dial option.
func New(target resolver.Target, cc resolver.ClientConn, opts resolver.BuildOptions, targetResolverBuilder resolver.Builder, targetResolutionEnabled bool) (resolver.Resolver, error) {
	r := &delegatingResolver{
		target: target,
		cc:     cc,
	}

	var err error
	r.proxyURL, err = parsedURLForProxy(target.Endpoint())
	if err != nil {
		return nil, fmt.Errorf("delegating_resolver: failed to determine proxy URL for target %s: %v", target, err)
	}

	// proxy is not configured or proxy address excluded using `NO_PROXY` env
	// var, so only target resolver is used.
	if r.proxyURL == nil {
		return targetResolverBuilder.Build(target, cc, opts)
	}

	if logger.V(2) {
		logger.Info("Proxy URL detected : %s", r.proxyURL)
	}

	// When the scheme is 'dns' and target resolution on client is not enabled,
	// resolution should be handled by the proxy, not the client. Therefore, we
	// bypass the target resolver and store the unresolved target address.
	if target.URL.Scheme == "dns" && !targetResolutionEnabled {
		r.targetAddrs = []resolver.Address{{Addr: target.Endpoint()}}
		r.targetResolverReady = true
	} else {
		wcc := &wrappingClientConn{
			parent:       r,
			resolverType: targetResolverType,
		}
		if r.targetResolver, err = targetResolverBuilder.Build(target, wcc, opts); err != nil {
			return nil, fmt.Errorf("delegating_resolver: unable to build the resolver for target %s: %v", target, err)
		}
	}

	if r.proxyResolver, err = r.proxyURIResolver(opts); err != nil {
		return nil, fmt.Errorf("delegating_resolver: failed to build resolver for proxy URL %q: %v", r.proxyURL, err)
	}
	return r, nil
}

// proxyURIResolver creates a resolver for resolving proxy URIs using the
// "dns" scheme. It adjusts the proxyURL to conform to the "dns:///" format and
// builds a resolver with a wrappingClientConn to capture resolved addresses.
func (r *delegatingResolver) proxyURIResolver(opts resolver.BuildOptions) (resolver.Resolver, error) {
	proxyBuilder := resolver.Get("dns")
	if proxyBuilder == nil {
		panic(fmt.Sprintln("delegating_resolver: resolver for proxy not found for scheme dns"))
	}
	r.proxyURL.Scheme = "dns"
	r.proxyURL.Path = "/" + r.proxyURL.Host
	r.proxyURL.Host = "" // Clear the Host field to conform to the "dns:///" format

	proxyTarget := resolver.Target{URL: *r.proxyURL}
	wcc := &wrappingClientConn{
		parent:       r,
		resolverType: proxyResolverType,
	}
	return proxyBuilder.Build(proxyTarget, wcc, opts)
}

func (r *delegatingResolver) ResolveNow(o resolver.ResolveNowOptions) {
	if r.targetResolver != nil {
		r.targetResolver.ResolveNow(o)
	}
	if r.proxyResolver != nil {
		r.proxyResolver.ResolveNow(o)
	}
}

func (r *delegatingResolver) Close() {
	if r.targetResolver != nil {
		r.targetResolver.Close()
	}
	if r.proxyResolver != nil {
		r.proxyResolver.Close()
	}
}

// generateCombinedAddressesLocked creates a list of combined addresses by
// pairing each proxy address with every target address. For each pair, it
// generates a new [resolver.Address] using the proxy address, and adding the
// target address as the attribute along with user info.
func (r *delegatingResolver) generateCombinedAddressesLocked() ([]resolver.Address, []resolver.Endpoint) {
	var addresses []resolver.Address
	for _, proxyAddr := range r.proxyAddrs {
		for _, targetAddr := range r.targetAddrs {
			newAddr := resolver.Address{Addr: proxyAddr.Addr}
			newAddr = proxyattributes.Populate(newAddr, proxyattributes.Options{
				User:        r.proxyURL.User,
				ConnectAddr: targetAddr.Addr,
			})
			addresses = append(addresses, newAddr)
		}
	}

	var endpoints []resolver.Endpoint
	for _, proxyEndpt := range r.proxyEndpoint {
		proxyAddr := proxyEndpt.Addresses[0].Addr
		for _, endpt := range r.targetEndpoint {
			var addrs []resolver.Address
			for _, targetAddr := range endpt.Addresses {
				newAddr := resolver.Address{Addr: proxyAddr}
				newAddr = proxyattributes.Populate(newAddr, proxyattributes.Options{
					User:        r.proxyURL.User,
					ConnectAddr: targetAddr.Addr,
				})
				addrs = append(addrs, newAddr)
			}
			endpoints = append(endpoints, resolver.Endpoint{Addresses: addrs})
		}
	}
	return addresses, endpoints
}

// resolverType is an enum representing the type of resolver (target or proxy).
type resolverType int

const (
	targetResolverType resolverType = iota
	proxyResolverType
)

// wrappingClientConn serves as an intermediary between the parent ClientConn
// and the child resolvers created here. It implements the resolver.ClientConn
// interface and is passed in that capacity to the child resolvers.
//
// Its primary function is to aggregate addresses returned by the child
// resolvers before passing them to the parent ClientConn. Any errors returned
// by the child resolvers are propagated verbatim to the parent ClientConn.
type wrappingClientConn struct {
	parent       *delegatingResolver
	resolverType resolverType // represents the type of resolver (target or proxy)
}

// UpdateState processes updates from the target or proxy resolver. It logs
// received addresses, and combines addresses from both resolvers once updates
// from both are received, sending the final state to the parent ClientConn.
func (wcc *wrappingClientConn) UpdateState(state resolver.State) error {
	wcc.parent.mu.Lock()
	defer wcc.parent.mu.Unlock()

	var curState resolver.State
	if wcc.resolverType == targetResolverType {
		if logger.V(2) {
			logger.Infof("Addresses received from target resolver: %v", state.Addresses)
		}
		wcc.parent.targetAddrs = state.Addresses
		wcc.parent.targetEndpoint = state.Endpoints
		wcc.parent.targetResolverReady = true
		// Update curState to include other state information, such as the
		// service config, provided by the target resolver. This ensures
		// curState contains all necessary information when passed to
		// UpdateState. The state update is only sent after both the target and
		// proxy resolvers have sent their updates, and curState has been
		// updated with the combined addresses.
		curState = state
	}
	// The proxy resolver is always "dns," so only the address will be
	// received, not an endpoint.
	if wcc.resolverType == proxyResolverType {
		if logger.V(2) {
			logger.Infof("Addresses received from proxy resolver: %s", state.Addresses)
		}
		wcc.parent.proxyAddrs = state.Addresses
		wcc.parent.proxyEndpoint = state.Endpoints
		wcc.parent.proxyResolverReady = true
	}

	// Proceed only if updates from both resolvers have been received.
	if !wcc.parent.targetResolverReady || !wcc.parent.proxyResolverReady {
		return nil
	}
	curState.Addresses, curState.Endpoints = wcc.parent.generateCombinedAddressesLocked()
	return wcc.parent.cc.UpdateState(curState)
}

// ReportError intercepts errors from the child resolvers and passes them to ClientConn.
func (wcc *wrappingClientConn) ReportError(err error) {
	wcc.parent.cc.ReportError(err)
}

// NewAddress intercepts the new resolved address from the child resolvers and
// passes them to ClientConn.
func (wcc *wrappingClientConn) NewAddress(addrs []resolver.Address) {
	wcc.UpdateState(resolver.State{Addresses: addrs})
}

// ParseServiceConfig parses the provided service config and returns an
// object that provides the parsed config.
func (wcc *wrappingClientConn) ParseServiceConfig(serviceConfigJSON string) *serviceconfig.ParseResult {
	return wcc.parent.cc.ParseServiceConfig(serviceConfigJSON)
}
