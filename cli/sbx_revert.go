package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/mattconzen/microvm/backend"
)

func newRevertCmd(ctx context.Context, app *App, g *GlobalFlags) *cobra.Command {
	var snapID string
	cmd := &cobra.Command{
		Use:   "revert <id> --snapshot <snap-id>",
		Short: "Restore a sandbox's workspace from a prior snapshot, in place",
		Long: `Revert overwrites the sandbox's workspace with the contents of the
named snapshot. The sandbox's id, session id, name, and cache tier (in
tiered mode) are unchanged; only the snapshottable workspace tier is
restored.

This is destructive: anything written to the workspace since the snapshot
is lost. Use sbx fork instead if you want to keep the current state too.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if snapID == "" {
				return fmt.Errorf("--snapshot is required")
			}
			b, sb, err := resolveBackendForID(app, args[0])
			if err != nil {
				return err
			}
			snap, err := app.Store.GetSnapshot(snapID)
			if err != nil {
				return fmt.Errorf("load snapshot: %w", err)
			}
			// Defensive: don't revert sandbox A to a snapshot of sandbox B.
			// (The mode-mismatch check in the backend covers the harder case
			// of cross-runtime snapshots; this is just a per-sandbox sanity
			// check.)
			if snap.SandboxID != sb.ID {
				return fmt.Errorf(
					"snapshot %s is of sandbox %s, not %s; refusing cross-sandbox revert",
					snap.ID, snap.SandboxID, sb.ID,
				)
			}

			// Resume targeting the existing sandbox id. The shellagent's
			// mirror-exact restore overwrites sessions/<id>/ in place; same
			// session id keeps the runtime sticky.
			_, err = b.Resume(ctx,
				backend.Snapshot{
					ID:              snap.ID,
					SandboxID:       snap.SandboxID,
					Provider:        snap.Provider,
					TargetSessionID: snap.TargetSessionID,
					Kind:            snap.Kind,
					Mode:            snap.Mode,
					Locator:         snap.Locator,
					Name:            snap.Name,
				},
				backend.SandboxSpec{Name: sb.Name, ID: sb.ID},
			)
			if err != nil {
				return fmt.Errorf("revert: %w", err)
			}

			// Stamp last-used and a label so the revert is auditable.
			sb.LastUsed = time.Now()
			if sb.Labels == nil {
				sb.Labels = map[string]string{}
			}
			sb.Labels["reverted_to"] = snap.ID
			if err := app.Store.PutSandbox(sb); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "reverted %s to %s\n", sb.ID, snap.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&snapID, "snapshot", "", "snapshot id to restore from (required)")
	_ = cmd.MarkFlagRequired("snapshot")
	return cmd
}
