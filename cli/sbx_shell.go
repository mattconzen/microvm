package cli

import (
	"context"
	"os"

	"github.com/spf13/cobra"

	"github.com/mattconzen/monorepo/apps/microvm/backend"
)

func newShellCmd(ctx context.Context, app *App, g *GlobalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "shell <id>",
		Aliases: []string{"ssh"},
		Short:   "Open an interactive shell to a sandbox",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, sb, err := resolveBackendForID(app, args[0])
			if err != nil {
				return err
			}
			sbApi := backend.Sandbox{ID: sb.ID, Provider: sb.Provider, SessionID: sb.SessionID}
			return b.Shell(ctx, sbApi, backend.TTY{
				In:  os.Stdin,
				Out: cmd.OutOrStdout(),
			})
		},
	}
	return cmd
}
