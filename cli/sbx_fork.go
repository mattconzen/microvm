package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/mattconzen/microvm/backend"
	"github.com/mattconzen/microvm/state"
)

func newForkCmd(ctx context.Context, app *App, g *GlobalFlags) *cobra.Command {
	var (
		name     string
		snapName string
	)
	cmd := &cobra.Command{
		Use:   "fork <id>",
		Short: "Snapshot a sandbox and immediately resume into a new one",
		Long: `Fork is shorthand for sbx snapshot + sbx resume. Use it to branch a
sandbox: the source sandbox keeps running unchanged, and a new sandbox is
created whose workspace starts as a copy of the source at the snapshot point.

The intermediate snapshot is persisted so you can see it with sbx snapshots
or use it to re-fork later.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, sb, err := resolveBackendForID(app, args[0])
			if err != nil {
				return err
			}
			sbApi := backend.Sandbox{ID: sb.ID, Provider: sb.Provider, SessionID: sb.SessionID, Mode: sb.Mode}

			// 1) Snapshot the source.
			snapID := state.NewSnapshotID()
			snap, err := b.Snapshot(ctx, sbApi, backend.SnapshotSpec{ID: snapID, Name: snapName})
			if err != nil {
				return fmt.Errorf("fork: snapshot source: %w", err)
			}
			snapRec := state.Snapshot{
				ID:              snapID,
				SandboxID:       sb.ID,
				Provider:        b.Name(),
				TargetSessionID: snap.TargetSessionID,
				Kind:            snap.Kind,
				Mode:            snap.Mode,
				Locator:         snap.Locator,
				Name:            snap.Name,
				CreatedAt:       snap.CreatedAt,
			}
			if err := app.Store.PutSnapshot(snapRec); err != nil {
				return fmt.Errorf("fork: persist snapshot: %w", err)
			}

			// 2) Resume into a fresh sandbox.
			newID := state.NewSandboxID()
			forkedSb, err := b.Resume(ctx,
				backend.Snapshot{
					ID:              snapRec.ID,
					SandboxID:       snapRec.SandboxID,
					Provider:        snapRec.Provider,
					TargetSessionID: snapRec.TargetSessionID,
					Kind:            snapRec.Kind,
					Mode:            snapRec.Mode,
					Locator:         snapRec.Locator,
					Name:            snapRec.Name,
				},
				backend.SandboxSpec{Name: name, ID: newID},
			)
			if err != nil {
				return fmt.Errorf("fork: resume into new sandbox: %w", err)
			}

			rec := state.Sandbox{
				ID:        newID,
				Provider:  b.Name(),
				SessionID: forkedSb.SessionID,
				Name:      name,
				Mode:      forkedSb.Mode,
				CreatedAt: time.Now(),
				LastUsed:  time.Now(),
				Labels:    map[string]string{"forked_from": sb.ID, "via_snapshot": snapRec.ID},
			}
			if err := app.Store.PutSandbox(rec); err != nil {
				return err
			}
			return writeSandbox(cmd, g, rec)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "name for the new (forked) sandbox")
	cmd.Flags().StringVar(&snapName, "snapshot-name", "", "name for the intermediate snapshot (default: auto)")
	return cmd
}
