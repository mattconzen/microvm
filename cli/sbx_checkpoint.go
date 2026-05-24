package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mattconzen/microvm/backend"
)

func newCheckpointCmd(ctx context.Context, app *App, g *GlobalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "checkpoint <id>",
		Short: "Promote tier-1 cache (promote/) into tier-2 workspace (cache-promoted/)",
		Long: `In tiered mode, the cache tier (/var/microvm/cache/<sandbox_id>/) is fast
but ephemeral. To include cache contents in the next snapshot, place them
under the cache's promote/ subdirectory and run this command. The shellagent
rsyncs promote/ into <workspace>/cache-promoted/ before the next snapshot.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, sb, err := resolveBackendForID(app, args[0])
			if err != nil {
				return err
			}
			sbApi := backend.Sandbox{ID: sb.ID, Provider: sb.Provider, SessionID: sb.SessionID, Mode: sb.Mode}
			if err := b.Checkpoint(ctx, sbApi); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "checkpointed %s\n", sb.ID)
			return nil
		},
	}
	return cmd
}
