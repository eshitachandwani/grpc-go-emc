/*
 *
 * Copyright 2017 gRPC authors.
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

// Package prxyresolver implements the default resolver that creates child
// resolvers to resolver targetURI as well as proxy adress.

package delegatingresolver

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/serviceconfig"
)

type delegatingResolver struct {
	target         resolver.Target
	cc             resolver.ClientConn
	isProxy        bool
	targetResolver resolver.Resolver
	proxyResolver  resolver.Resolver
	targetAddrs    []resolver.Address
	proxyAddrs     []resolver.Address
	ctx            context.Context
	cancel         context.CancelFunc
}

type innerClientConn struct {
	cc           resolver.ClientConn
	parent       *delegatingResolver
	resolverType string
}

var (
	// The following variable will be overwritten in the tests.
	httpProxyFromEnvironment = http.ProxyFromEnvironment
)

func New(target resolver.Target, cc resolver.ClientConn, opts resolver.BuildOptions, targetResolverBuilder resolver.Builder) (resolver.Resolver, error) {
	var err error
	r := &delegatingResolver{
		target: target,
		cc:     cc,
	}
	proxyURI := os.Getenv("HTTPS_PROXY")
	if proxyURI == "" {
		proxyURI = os.Getenv("HTTP_PROXY")
	}
	r.isProxy = true
	if proxyURI == "" {
		r.isProxy = false
	}

	r.targetResolver, err = targetURIResolver(target, cc, opts, targetResolverBuilder, r)

	if err != nil {
		return nil, err
	}
	return r, nil
}

func targetURIResolver(target resolver.Target, cc resolver.ClientConn, opts resolver.BuildOptions, targetResolverBuilder resolver.Builder, resolver *delegatingResolver) (resolver.Resolver, error) {
	targetBuilder := targetResolverBuilder
	if targetBuilder == nil {
		return nil, fmt.Errorf("resolver for target scheme %q not found", target.URL.Scheme)
	}
	return targetBuilder.Build(target, &innerClientConn{cc, resolver, "target"}, opts)
}

func (r *delegatingResolver) ResolveNow(o resolver.ResolveNowOptions) {

	r.targetResolver.ResolveNow(o)
}

func (r *delegatingResolver) Close() {

	r.targetResolver.Close()

}

// UpdateState intercepts state updates from the child DNS resolver.
func (icc *innerClientConn) UpdateState(state resolver.State) error {
	icc.parent.targetAddrs = state.Addresses
	return icc.parent.cc.UpdateState(state)

}

// ReportError intercepts errors from the child DNS resolver.
func (icc *innerClientConn) ReportError(err error) {
	icc.parent.cc.ReportError(err)
}

func (icc *innerClientConn) NewAddress(addrs []resolver.Address) {
	if icc.resolverType == "target" {

		icc.parent.cc.NewAddress(addrs)
	}
}

// ParseServiceConfig parses the provided service config and returns an
// object that provides the parsed config.
func (icc *innerClientConn) ParseServiceConfig(serviceConfigJSON string) *serviceconfig.ParseResult {
	fmt.Printf("config received by delegating resolver : %v \n", serviceConfigJSON)
	if icc.resolverType == "target" {
		return icc.parent.cc.ParseServiceConfig(serviceConfigJSON)
	}
	return nil
}
