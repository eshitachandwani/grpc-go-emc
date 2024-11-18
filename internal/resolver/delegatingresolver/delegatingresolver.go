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

// Package delegatingresolver implements the default resolver that creates child
// resolvers to resolver targetURI as well as proxy adress.

package delegatingresolver

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"

	"google.golang.org/grpc/attributes"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/serviceconfig"
)

type delegatingResolver struct {
	target         resolver.Target
	cc             resolver.ClientConn
	isProxy        bool
	targetResolver resolver.Resolver
	ProxyResolver  resolver.Resolver
	targetAddrs    []resolver.Address
	proxyAddrs     []resolver.Address
	proxyURL       *url.URL
	mu             sync.Mutex // Protects the state below

}

type innerClientConn struct {
	*delegatingResolver
	resolverType string
}

var (
	// The following variable will be overwritten in the tests.
	httpProxyFromEnvironment = http.ProxyFromEnvironment
)

func mapAddress(address string) (*url.URL, error) {
	req := &http.Request{
		URL: &url.URL{
			Scheme: "https",
			Host:   address,
		},
	}
	url, err := httpProxyFromEnvironment(req)
	if err != nil {
		return nil, err
	}
	return url, nil
}

func New(target resolver.Target, cc resolver.ClientConn, opts resolver.BuildOptions, targetResolverBuilder resolver.Builder) (resolver.Resolver, error) {
	r := &delegatingResolver{
		target: target,
		cc:     cc,
	}
	var err error
	r.proxyURL, err = mapAddress(target.Endpoint())
	if err != nil {
		return nil, err
	}
	r.isProxy = true
	if r.proxyURL == nil {
		r.isProxy = false
	}
	if !r.isProxy {
		return targetResolverBuilder.Build(target, cc, opts)
	}
	if target.URL.Scheme == "dns" {
		r.targetAddrs = []resolver.Address{{Addr: r.target.Endpoint()}}
	} else {
		r.targetResolver, err = targetURIResolver(target, opts, targetResolverBuilder, r)
	}
	r.ProxyResolver, err = proxyURIResolver(r.proxyURL, opts, r)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func targetURIResolver(target resolver.Target, opts resolver.BuildOptions, targetResolverBuilder resolver.Builder, resolver *delegatingResolver) (resolver.Resolver, error) {
	targetBuilder := targetResolverBuilder
	if targetBuilder == nil {
		return nil, fmt.Errorf("resolver for target scheme %q not found", target.URL.Scheme)
	}
	return targetBuilder.Build(target, &innerClientConn{resolver, "target"}, opts)
}

func proxyURIResolver(proxyURL *url.URL, opts resolver.BuildOptions, presolver *delegatingResolver) (resolver.Resolver, error) {
	scheme := "dns"
	proxyBuilder := resolver.Get(scheme)
	if proxyBuilder == nil {
		return nil, fmt.Errorf("resolver for proxy not found")
	}
	host := "dns:///" + proxyURL.Host
	u, err := url.Parse(host)
	if err != nil {
		return nil, err
	}

	proxyTarget := resolver.Target{URL: *u}
	return proxyBuilder.Build(proxyTarget, &innerClientConn{presolver, "proxy"}, opts)
}

func (r *delegatingResolver) ResolveNow(o resolver.ResolveNowOptions) {
	if r.targetResolver != nil {
		r.targetResolver.ResolveNow(o)
	}
	if r.ProxyResolver != nil {
		r.ProxyResolver.ResolveNow(o)
	}
}

func (r *delegatingResolver) Close() {
	if r.targetResolver != nil {
		r.targetResolver.Close()
	}
	if r.ProxyResolver != nil {
		r.ProxyResolver.Close()
	}
}

// UpdateState intercepts state updates from the childtarget and proxy resolvers.
func (icc *innerClientConn) UpdateState(state resolver.State) error {
	icc.mu.Lock()
	defer icc.mu.Unlock()

	var curState resolver.State
	if icc.resolverType == "target" {
		icc.targetAddrs = state.Addresses
		curState = state
	}
	if icc.resolverType == "proxy" {
		icc.proxyAddrs = state.Addresses
	}

	if len(icc.targetAddrs) == 0 || len(icc.proxyAddrs) == 0 {
		return nil
	}

	var addresses []resolver.Address
	for _, proxyAddr := range icc.proxyAddrs {
		for _, targetAddr := range icc.targetAddrs {
			attr := attributes.New("proxyConnectAddr", targetAddr.Addr)
			if icc.proxyURL.User != nil {
				attr = attr.WithValue("user", icc.proxyURL.User)
			}
			addresses = append(addresses, resolver.Address{
				Addr:       proxyAddr.Addr,
				Attributes: attr,
			})
		}
	}
	// Set the addresses in the current state.
	curState.Addresses = addresses
	return icc.cc.UpdateState(curState)
}

// ReportError intercepts errors from the child DNS resolver.
func (icc *innerClientConn) ReportError(err error) {
	icc.cc.ReportError(err)
}

func (icc *innerClientConn) NewAddress(addrs []resolver.Address) {
}

// ParseServiceConfig parses the provided service config and returns an
// object that provides the parsed config.
func (icc *innerClientConn) ParseServiceConfig(serviceConfigJSON string) *serviceconfig.ParseResult {
	fmt.Printf("config received by delegating resolver : %v \n", serviceConfigJSON)
	if icc.resolverType == "target" {
		return icc.cc.ParseServiceConfig(serviceConfigJSON)
	}
	return nil
}
