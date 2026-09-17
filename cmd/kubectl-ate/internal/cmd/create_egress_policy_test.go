// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

func hostnameRule(patterns ...string) *ateapipb.EgressRule {
	return &ateapipb.EgressRule{Hostnames: &ateapipb.HostnameRule{Patterns: patterns}}
}

func cidrRule(cidrs ...string) *ateapipb.EgressRule {
	return &ateapipb.EgressRule{Cidrs: &ateapipb.CIDRRule{Cidrs: cidrs}}
}

func TestBuildEgressPolicy(t *testing.T) {
	tests := []struct {
		name      string
		hostnames []string
		cidrs     []string
		all       bool
		want      []*ateapipb.EgressRule
		wantErr   bool
	}{
		{
			name: "allow everything",
			all:  true,
			want: []*ateapipb.EgressRule{{All: &emptypb.Empty{}}},
		},
		{
			name:      "one hostname rule with several patterns",
			hostnames: []string{"api.example.com,*.internal.example.com"},
			want:      []*ateapipb.EgressRule{hostnameRule("api.example.com", "*.internal.example.com")},
		},
		{
			name:      "each flag occurrence is its own rule, in order",
			hostnames: []string{"first.example.com", "second.example.com"},
			want: []*ateapipb.EgressRule{
				hostnameRule("first.example.com"),
				hostnameRule("second.example.com"),
			},
		},
		{
			name:  "cidr rule",
			cidrs: []string{"10.244.0.0/16,192.0.2.0/24"},
			want:  []*ateapipb.EgressRule{cidrRule("10.244.0.0/16", "192.0.2.0/24")},
		},
		{
			// Hostnames before cidrs before all: the policy is
			// first-match-wins, so a catch-all must not shadow the narrow
			// rules given alongside it.
			name:      "hostnames precede cidrs, and all comes last",
			hostnames: []string{"api.example.com"},
			cidrs:     []string{"10.244.0.0/16"},
			all:       true,
			want: []*ateapipb.EgressRule{
				hostnameRule("api.example.com"),
				cidrRule("10.244.0.0/16"),
				{All: &emptypb.Empty{}},
			},
		},
		{
			name:      "values are trimmed",
			hostnames: []string{"api.example.com, other.example.com"},
			want:      []*ateapipb.EgressRule{hostnameRule("api.example.com", "other.example.com")},
		},
		{name: "no rules at all is an error", wantErr: true},
		{name: "empty value from a stray comma", hostnames: []string{"api.example.com,"}, wantErr: true},
		{name: "empty cidr value", cidrs: []string{",10.244.0.0/16"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildEgressPolicy(tt.hostnames, tt.cidrs, tt.all)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("buildEgressPolicy(%v, %v, %v) succeeded, want an error", tt.hostnames, tt.cidrs, tt.all)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildEgressPolicy(%v, %v, %v): %v", tt.hostnames, tt.cidrs, tt.all, err)
			}
			if diff := cmp.Diff(tt.want, got.GetRules(), protocmp.Transform()); diff != "" {
				t.Errorf("rules differ (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNormalizeEgressPolicyMetadata(t *testing.T) {
	tests := []struct {
		name     string
		policy   *ateapipb.EgressPolicy
		atespace string
		want     *ateapipb.ResourceMetadata
	}{
		{
			name:     "absent metadata is filled in",
			policy:   &ateapipb.EgressPolicy{},
			atespace: "demo",
			want:     &ateapipb.ResourceMetadata{Atespace: "demo", Name: "default"},
		},
		{
			name:     "empty fields are filled in",
			policy:   &ateapipb.EgressPolicy{Metadata: &ateapipb.ResourceMetadata{}},
			atespace: "demo",
			want:     &ateapipb.ResourceMetadata{Atespace: "demo", Name: "default"},
		},
		{
			// A manifest naming a different atespace is left alone so the
			// server rejects the mismatch, rather than being silently
			// rewritten to whatever -a said.
			name:     "values from a manifest are preserved",
			policy:   &ateapipb.EgressPolicy{Metadata: &ateapipb.ResourceMetadata{Atespace: "other", Name: "default"}},
			atespace: "demo",
			want:     &ateapipb.ResourceMetadata{Atespace: "other", Name: "default"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			normalizeEgressPolicyMetadata(tt.policy, tt.atespace)
			if diff := cmp.Diff(tt.want, tt.policy.GetMetadata(), protocmp.Transform()); diff != "" {
				t.Errorf("metadata differs (-want +got):\n%s", diff)
			}
		})
	}
}

func TestEgressPolicyFromManifest(t *testing.T) {
	const manifest = `
metadata:
  atespace: demo
  name: default
rules:
- hostnames:
    patterns: ["api.example.com"]
- cidrs:
    cidrs: ["10.244.0.0/16"]
`
	got, err := egressPolicyFromManifest([]byte(manifest))
	if err != nil {
		t.Fatalf("egressPolicyFromManifest: %v", err)
	}
	want := &ateapipb.EgressPolicy{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "demo", Name: "default"},
		Rules: []*ateapipb.EgressRule{
			hostnameRule("api.example.com"),
			cidrRule("10.244.0.0/16"),
		},
	}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("policy differs (-want +got):\n%s", diff)
	}
}

func TestEgressPolicyFromManifestRejectsBadInput(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
	}{
		{name: "empty document", manifest: ""},
		{name: "not YAML", manifest: "\tnot: [valid"},
		// Strict parsing: a typo must not silently drop the rule.
		{name: "unknown field", manifest: "rules:\n- hostname:\n    patterns: [\"a.example.com\"]\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := egressPolicyFromManifest([]byte(tt.manifest)); err == nil {
				t.Errorf("egressPolicyFromManifest(%q) succeeded, want an error", tt.manifest)
			}
		})
	}
}
