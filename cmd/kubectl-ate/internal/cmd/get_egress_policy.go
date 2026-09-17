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

	"github.com/spf13/cobra"

	"github.com/agent-substrate/substrate/cmd/kubectl-ate/internal/printer"
	"github.com/agent-substrate/substrate/internal/ateclient"
	"github.com/agent-substrate/substrate/internal/resources"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

var getEgressPolicyAtespaceFlag string

var getEgressPolicyCmd = &cobra.Command{
	Use:     "egress-policy <actor-name> -a <atespace>",
	Aliases: []string{"egress-policies"},
	Short:   "Display an actor's egress policy",
	Long: `Display the egress policy of an actor.

An actor has at most one egress policy, named "default". An actor with no
policy is denied all egress, and this command reports that the policy was not
found.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		apiClient, err := ateclient.NewClient(ctx, kubeconfig, k8sContext, endpoint, tokenFile, traceEnabled)
		if err != nil {
			return fmt.Errorf("failed to connect to ate-api-server: %w", err)
		}
		defer apiClient.Close()

		actorRef := resources.ActorRef{Atespace: getEgressPolicyAtespaceFlag, Name: args[0]}
		resp, err := apiClient.GetActorEgressPolicy(ctx, &ateapipb.GetActorEgressPolicyRequest{
			Actor: actorRef.ToObjectRef(),
		})
		if err != nil {
			return fmt.Errorf("failed to get egress policy: %w", err)
		}

		return printer.PrintEgressPolicyTo(cmd.OutOrStdout(), resp, outputFmt)
	},
}

func init() {
	getEgressPolicyCmd.Flags().StringVarP(&getEgressPolicyAtespaceFlag, "atespace", "a", "", "Atespace the actor lives in")
	_ = getEgressPolicyCmd.MarkFlagRequired("atespace")
	getCmd.AddCommand(getEgressPolicyCmd)
}
