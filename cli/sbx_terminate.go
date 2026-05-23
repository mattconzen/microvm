package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mattconzen/monorepo/apps/microvm/backend"
)

func newTerminateCmd(ctx context.Context, app *App, g *GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "terminate <id>",
		Aliases: []string{"rm", "delete"},
		Short:   "Terminate a sandbox and remove it from local state",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, sb, err := resolveBackendForID(app, args[0])
			if err != nil {
				return err
			}
			sbApi := backend.Sandbox{ID: sb.ID, Provider: sb.Provider, SessionID: sb.SessionID}
			if err := b.Terminate(ctx, sbApi); err != nil {
				return err
			}
			if err := app.Store.DeleteSandbox(sb.ID); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "terminated %s\n", sb.ID)
			return nil
		},
	}
}
