package cli

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	"github.com/mattconzen/monorepo/apps/microvm/backend"
	"github.com/mattconzen/monorepo/apps/microvm/state"
)

func newResumeCmd(ctx context.Context, app *App, g *GlobalFlags) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "resume <snapshot-id>",
		Short: "Resume a sandbox from a snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			snap, err := app.Store.GetSnapshot(args[0])
			if err != nil {
				return err
			}
			b, err := app.Registry.Get(snap.Provider)
			if err != nil {
				return err
			}
			sb, err := b.Resume(ctx,
				backend.Snapshot{
					ID:              snap.ID,
					SandboxID:       snap.SandboxID,
					Provider:        snap.Provider,
					TargetSessionID: snap.TargetSessionID,
					Kind:            snap.Kind,
					Name:            snap.Name,
				},
				backend.SandboxSpec{Name: name},
			)
			if err != nil {
				return err
			}
			rec := state.Sandbox{
				ID:        state.NewSandboxID(),
				Provider:  b.Name(),
				SessionID: sb.SessionID,
				Name:      name,
				CreatedAt: time.Now(),
				LastUsed:  time.Now(),
				Labels:    map[string]string{"resumed_from": snap.ID},
			}
			if err := app.Store.PutSandbox(rec); err != nil {
				return err
			}
			return writeSandbox(cmd, g, rec)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "new sandbox name")
	return cmd
}
