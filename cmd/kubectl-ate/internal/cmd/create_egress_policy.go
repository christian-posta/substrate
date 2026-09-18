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
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/emptypb"
	"sigs.k8s.io/yaml"

	"github.com/agent-substrate/substrate/cmd/kubectl-ate/internal/printer"
	"github.com/agent-substrate/substrate/internal/ateclient"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

var (
	createEgressPolicyAtespaceFlag  string
	createEgressPolicyHostnameFlags []string
	createEgressPolicyCIDRFlags     []string
	createEgressPolicyAllFlag       bool
	createEgressPolicyFilenameFlag  string
)

var createEgressPolicyCmd = &cobra.Command{
	Use:   "egress-policy <actor-name> -a <atespace>",
	Short: "Create or replace an actor's egress policy",
	Long: `Create the egress policy for an actor.

The egress gateway denies by default, so an actor without a policy is refused.
How completely depends on the dataplane: Envoy denies at the CONNECT, so such an
actor gets no tunnel at all, while agentgateway evaluates policy only on routes
it is attached to and still permits TLS-passthrough and opaque-TCP egress.

Rules are evaluated in order and the first match decides. They are built
--hostnames rules first, then --cidrs, then --all, regardless of the order the
flags appear on the command line, because the shell hands them over grouped by
flag rather than interleaved. Use -f when a different order matters.

Each --hostnames or --cidrs occurrence becomes one rule, and --all appends a
match-everything rule. --hostnames takes DNS names, optionally with a "*" in
place of the complete leftmost label; --cidrs takes canonical IPv4 or IPv6
prefixes. Neither matches on port.

A CONNECT the gateway cannot read is decided by address alone, so an actor that
reaches its destination by IP needs a --cidrs or --all rule even when a
--hostnames rule names the same host.

Alternatively, -f takes a protojson-shaped EgressPolicy document, as printed by
"kubectl ate get egress-policy <actor> -a <atespace> -o yaml".

An actor has at most one egress policy, named "default". This command replaces
an existing policy rather than failing, so it is safe to re-run.`,
	Example: `  # Let the actor reach anything (the pre-policy behavior).
  kubectl ate create egress-policy egress-demo -a ate-demo-egress --all

  # Allow one hostname, and the cluster's pod CIDR by address.
  kubectl ate create egress-policy egress-demo -a ate-demo-egress \
    --hostnames api.example.com,'*.internal.example.com' --cidrs 10.244.0.0/16

  # From a manifest.
  kubectl ate create egress-policy egress-demo -a ate-demo-egress -f policy.yaml`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		actorRef := resources.ActorRef{Atespace: createEgressPolicyAtespaceFlag, Name: args[0]}

		var policy *ateapipb.EgressPolicy
		if createEgressPolicyFilenameFlag != "" {
			data, err := readFileOrStdin(cmd.InOrStdin(), createEgressPolicyFilenameFlag)
			if err != nil {
				return err
			}
			policy, err = egressPolicyFromManifest(data)
			if err != nil {
				return fmt.Errorf("failed to parse egress policy manifest %q: %w", createEgressPolicyFilenameFlag, err)
			}
		} else {
			var err error
			policy, err = buildEgressPolicy(createEgressPolicyHostnameFlags, createEgressPolicyCIDRFlags, createEgressPolicyAllFlag)
			if err != nil {
				return err
			}
		}
		// The server requires the policy's own metadata to name the parent
		// atespace and the reserved policy name, so fill in anything the
		// caller left out rather than bouncing the request.
		normalizeEgressPolicyMetadata(policy, actorRef.Atespace)

		ctx := cmd.Context()
		apiClient, err := ateclient.NewClient(ctx, kubeconfig, k8sContext, endpoint, tokenFile, traceEnabled)
		if err != nil {
			return fmt.Errorf("failed to connect to ate-api-server: %w", err)
		}
		defer apiClient.Close()

		resp, err := apiClient.CreateActorEgressPolicy(ctx, &ateapipb.CreateActorEgressPolicyRequest{
			Actor:        actorRef.ToObjectRef(),
			EgressPolicy: policy,
		})
		if status.Code(err) == codes.AlreadyExists {
			// Update is a full replacement and needs the current UID and
			// version as preconditions, so read the policy back before
			// writing over it.
			existing, getErr := apiClient.GetActorEgressPolicy(ctx, &ateapipb.GetActorEgressPolicyRequest{
				Actor: actorRef.ToObjectRef(),
			})
			if getErr != nil {
				return fmt.Errorf("failed to read the existing egress policy: %w", getErr)
			}
			policy.Metadata = existing.GetMetadata()
			resp, err = apiClient.UpdateActorEgressPolicy(ctx, &ateapipb.UpdateActorEgressPolicyRequest{
				Actor:        actorRef.ToObjectRef(),
				EgressPolicy: policy,
			})
			if err != nil {
				return fmt.Errorf("failed to replace egress policy: %w", err)
			}
		} else if err != nil {
			return fmt.Errorf("failed to create egress policy: %w", err)
		}

		return printer.PrintEgressPolicyTo(cmd.OutOrStdout(), resp, outputFmt)
	},
}

// buildEgressPolicy turns the rule flags into an EgressPolicy, preserving the
// order the flags were given in: the policy is first-match-wins, so the order
// is part of what the caller asked for.
func buildEgressPolicy(hostnames, cidrs []string, all bool) (*ateapipb.EgressPolicy, error) {
	if len(hostnames) == 0 && len(cidrs) == 0 && !all {
		return nil, fmt.Errorf("no rules given: pass at least one of --hostnames, --cidrs or --all, or -f to supply a manifest")
	}

	rules := make([]*ateapipb.EgressRule, 0, len(hostnames)+len(cidrs)+1)
	for _, patterns := range hostnames {
		values, err := splitRuleValues("--hostnames", patterns)
		if err != nil {
			return nil, err
		}
		rules = append(rules, &ateapipb.EgressRule{Hostnames: &ateapipb.HostnameRule{Patterns: values}})
	}
	for _, prefixes := range cidrs {
		values, err := splitRuleValues("--cidrs", prefixes)
		if err != nil {
			return nil, err
		}
		rules = append(rules, &ateapipb.EgressRule{Cidrs: &ateapipb.CIDRRule{Cidrs: values}})
	}
	if all {
		// Last, so a narrower rule given alongside it still decides first.
		// cobra reports flags by name, not by position, so --all cannot be
		// ordered against the others the way repeated flags order among
		// themselves.
		rules = append(rules, &ateapipb.EgressRule{All: &emptypb.Empty{}})
	}

	return &ateapipb.EgressPolicy{Rules: rules}, nil
}

// egressPolicyName is the only name an actor's egress policy may have.
const egressPolicyName = "default"

// splitRuleValues splits one occurrence of a rule flag into its values. The
// flags are StringArray rather than StringSlice precisely so that cobra does
// not split them first: one occurrence has to stay one rule, because a rule is
// the unit that carries effects and the unit that first-match-wins picks.
func splitRuleValues(flag, raw string) ([]string, error) {
	values := strings.Split(raw, ",")
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			return nil, fmt.Errorf("%s has an empty value in %q", flag, raw)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s needs at least one value", flag)
	}
	return out, nil
}

// egressPolicyFromManifest parses a single protojson-shaped YAML or JSON
// document into an EgressPolicy. Parsing is strict: unknown fields are an
// error, so typos don't silently drop rules.
func egressPolicyFromManifest(data []byte) (*ateapipb.EgressPolicy, error) {
	jsonData, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	if string(jsonData) == "null" {
		return nil, fmt.Errorf("manifest is empty")
	}
	policy := &ateapipb.EgressPolicy{}
	if err := protojson.Unmarshal(jsonData, policy); err != nil {
		return nil, err
	}
	return policy, nil
}

// normalizeEgressPolicyMetadata fills in the atespace and name the server
// requires. A manifest that already names them keeps its values, so a
// mismatch is reported by the server rather than silently rewritten here.
func normalizeEgressPolicyMetadata(policy *ateapipb.EgressPolicy, atespace string) {
	if policy.GetMetadata() == nil {
		policy.Metadata = &ateapipb.ResourceMetadata{}
	}
	if policy.Metadata.GetAtespace() == "" {
		policy.Metadata.Atespace = atespace
	}
	if policy.Metadata.GetName() == "" {
		policy.Metadata.Name = egressPolicyName
	}
}

func init() {
	createEgressPolicyCmd.Flags().StringVarP(&createEgressPolicyAtespaceFlag, "atespace", "a", "", "Atespace the actor lives in")
	_ = createEgressPolicyCmd.MarkFlagRequired("atespace")
	createEgressPolicyCmd.Flags().StringArrayVar(&createEgressPolicyHostnameFlags, "hostnames", nil, "One hostname rule, as a comma-separated list of DNS patterns. Repeat the flag to add more rules.")
	createEgressPolicyCmd.Flags().StringArrayVar(&createEgressPolicyCIDRFlags, "cidrs", nil, "One CIDR rule, as a comma-separated list of prefixes. Repeat the flag to add more rules.")
	createEgressPolicyCmd.Flags().BoolVar(&createEgressPolicyAllFlag, "all", false, "Append a rule matching every destination")
	createEgressPolicyCmd.Flags().StringVarP(&createEgressPolicyFilenameFlag, "filename", "f", "", "Manifest file holding a single protojson-shaped EgressPolicy document; use - for stdin")
	createEgressPolicyCmd.MarkFlagsMutuallyExclusive("filename", "hostnames")
	createEgressPolicyCmd.MarkFlagsMutuallyExclusive("filename", "cidrs")
	createEgressPolicyCmd.MarkFlagsMutuallyExclusive("filename", "all")
	createCmd.AddCommand(createEgressPolicyCmd)
}
