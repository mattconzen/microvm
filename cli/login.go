package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mattconzen/microvm/backend"
)

func newLoginCmd(ctx context.Context, app *App, g *GlobalFlags) *cobra.Command {
	opts := backend.LoginOpts{}
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Validate cloud credentials and register the AgentCore runtime",
		Long: `Validate cloud credentials and persist the AgentCore runtime ARN to config.

For first-time setup you must build and push the shell-agent container yourself
(see apps/microvm/shellagent/Dockerfile) and create an AgentCore runtime from
it (aws bedrock-agentcore-control create-agent-runtime). Then run:

  microvm login --runtime-arn arn:aws:bedrock-agentcore:...:runtime/microvm-shell

Subsequent invocations re-validate the runtime is still reachable.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := resolveBackend(app, g)
			if err != nil {
				return err
			}
			if err := b.Login(ctx, opts); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "login ok")
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.Region, "region", "", "AWS region (e.g. us-east-1)")
	cmd.Flags().StringVar(&opts.RuntimeArn, "runtime-arn", "", "Bedrock AgentCore runtime ARN to bind")
	cmd.Flags().StringVar(&opts.ImageDigest, "image-digest", "", "ECR image digest the runtime points at (for drift detection)")
	cmd.Flags().BoolVar(&opts.Rebuild, "rebuild", false, "force re-binding (does not push images)")
	return cmd
}
