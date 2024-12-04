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

package delegatingresolver

import (
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/internal"
	"google.golang.org/grpc/internal/grpctest"
	_ "google.golang.org/grpc/resolver/dns" // To register dns resolver.
)

type s struct {
	grpctest.Tester
}

func Test(t *testing.T) {
	grpctest.RunSubTests(t, s{})
}

const (
	targetTestAddr = "test.com"
	envProxyAddr   = "proxytest.com"
)

// overrideHTTPSProxyFromEnvironment function overwrites HTTPSProxyFromEnvironment and
// returns a function to restore the default values.
func overrideHTTPSProxyFromEnvironment(hpfe func(req *http.Request) (*url.URL, error)) func() {
	internal.HTTPSProxyFromEnvironmentForTesting = hpfe
	return func() {
		internal.HTTPSProxyFromEnvironmentForTesting = nil
	}
}

// Tests that the parsedURLForProxy function correctly resolves the proxy URL
// for a given target address. Tests all the possible output cases.
func (s) TestParsedURLForProxyEnv(t *testing.T) {
	err := errors.New("invalid proxy url")
	tests := []struct {
		name     string
		hpfeFunc func(req *http.Request) (*url.URL, error)
		wantURL  *url.URL
		wantErr  error
	}{
		{
			name: "valid proxy url and nil error",
			hpfeFunc: func(_ *http.Request) (*url.URL, error) {
				return &url.URL{
					Scheme: "https",
					Host:   "proxy.example.com",
				}, nil
			},
			wantURL: &url.URL{
				Scheme: "https",
				Host:   "proxy.example.com",
			},
			wantErr: nil,
		},
		{
			name: "invalid proxy url and non-nil error",
			hpfeFunc: func(_ *http.Request) (*url.URL, error) {
				return &url.URL{
					Scheme: "https",
					Host:   "notproxy.example.com",
				}, err
			},
			wantURL: nil,
			wantErr: err,
		},
		{
			name: "nil proxy url and nil error",
			hpfeFunc: func(_ *http.Request) (*url.URL, error) {
				return nil, nil
			},
			wantURL: nil,
			wantErr: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer overrideHTTPSProxyFromEnvironment(tt.hpfeFunc)()
			got, err := parsedURLForProxy(targetTestAddr)
			if err != tt.wantErr {
				t.Errorf("parsedProxyURLForProxy(%v) failed with error :%v, want %v\n", targetTestAddr, err, tt.wantErr)
			}
			if !cmp.Equal(got, tt.wantURL) {
				t.Fatalf("parsedProxyURLForProxy(%v) = %v, want %v\n", targetTestAddr, got, tt.wantURL)
			}
		})
	}
}
