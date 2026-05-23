package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mattconzen/microvm/backend"
	"github.com/mattconzen/microvm/state"
)

func newSnapshotCmd(ctx context.Context, app *App, g *GlobalFlags) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "snapshot <id>",
		Short: "Snapshot a sandbox (AWS: session alias)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, sb, err := resolveBackendForID(app, args[0])
			if err != nil {
				return err
			}
			sbApi := backend.Sandbox{ID: sb.ID, Provider: sb.Provider, SessionID: sb.SessionID}
			snap, err := b.Snapshot(ctx, sbApi, name)
			if err != nil {
				return err
			}
			rec := state.Snapshot{
				ID:              state.NewSnapshotID(),
				SandboxID:       sb.ID,
				Provider:        b.Name(),
				TargetSessionID: snap.TargetSessionID,
				Kind:            snap.Kind,
				Name:            snap.Name,
				CreatedAt:       snap.CreatedAt,
			}
			if err := app.Store.PutSnapshot(rec); err != nil {
				return err
			}
			if g.Output == "json" {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rec)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", rec.ID, rec.Kind, rec.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "snapshot name")
	return cmd
}
