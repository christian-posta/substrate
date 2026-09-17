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

var deleteEgressPolicyAtespaceFlag string

var deleteEgressPolicyCmd = &cobra.Command{
	Use:   "egress-policy <actor-name> -a <atespace>",
	Short: "Delete an actor's egress policy",
	Long: `Delete the egress policy of an actor.

The gateway denies by default, so deleting the policy denies the actor all
egress rather than restoring unrestricted access. Deleting the actor deletes
its policy too.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		apiClient, err := ateclient.NewClient(ctx, kubeconfig, k8sContext, endpoint, tokenFile, traceEnabled)
		if err != nil {
			return fmt.Errorf("failed to connect to ate-api-server: %w", err)
		}
		defer apiClient.Close()

		actorRef := resources.ActorRef{Atespace: deleteEgressPolicyAtespaceFlag, Name: args[0]}
		resp, err := apiClient.DeleteActorEgressPolicy(ctx, &ateapipb.DeleteActorEgressPolicyRequest{
			Actor: actorRef.ToObjectRef(),
		})
		if err != nil {
			return fmt.Errorf("failed to delete egress policy: %w", err)
		}

		return printer.PrintEgressPolicyTo(cmd.OutOrStdout(), resp, outputFmt)
	},
}

func init() {
	deleteEgressPolicyCmd.Flags().StringVarP(&deleteEgressPolicyAtespaceFlag, "atespace", "a", "", "Atespace the actor lives in")
	_ = deleteEgressPolicyCmd.MarkFlagRequired("atespace")
	deleteCmd.AddCommand(deleteEgressPolicyCmd)
}
