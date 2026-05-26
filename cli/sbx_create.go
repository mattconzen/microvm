package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/mattconzen/microvm/backend"
	"github.com/mattconzen/microvm/state"
)

func newCreateCmd(ctx context.Context, app *App, g *GlobalFlags) *cobra.Command {
	var (
		image    string
		cpus     float64
		mem      int
		name     string
		fromSnap string
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new microVM sandbox",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := resolveBackend(app, g)
			if err != nil {
				return err
			}
			if fromSnap != "" {
				if image != "" || cpus != 0 || mem != 0 {
					return fmt.Errorf("--from-snapshot cannot be combined with --image / --cpus / --memory (resource config comes from the resumed sandbox); omit them or use a separate sbx resume instead")
				}
				snap, err := app.Store.GetSnapshot(fromSnap)
				if err != nil {
					return fmt.Errorf("load snapshot: %w", err)
				}
				newID := state.NewSandboxID()
				resumedSb, err := b.Resume(ctx,
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
					backend.SandboxSpec{Name: name, ID: newID},
				)
				if err != nil {
					return fmt.Errorf("create --from-snapshot: %w", err)
				}
				rec := state.Sandbox{
					ID:        newID,
					Provider:  b.Name(),
					SessionID: resumedSb.SessionID,
					Image:     image,
					Name:      name,
					CPUs:      cpus,
					MemoryMB:  mem,
					Mode:      resumedSb.Mode,
					CreatedAt: time.Now(),
					LastUsed:  time.Now(),
					Labels:    map[string]string{"created_from": snap.ID},
				}
				if err := app.Store.PutSandbox(rec); err != nil {
					return err
				}
				return writeSandbox(cmd, g, rec)
			}
			spec := backend.SandboxSpec{
				Image:    image,
				Name:     name,
				CPUs:     cpus,
				MemoryMB: mem,
				FromSnap: fromSnap,
			}
			sb, err := b.Create(ctx, spec)
			if err != nil {
				return err
			}
			// Mint our own ID + session ID. The microVM is provisioned lazily on first exec.
			rec := state.Sandbox{
				ID:        state.NewSandboxID(),
				Provider:  b.Name(),
				SessionID: state.NewSessionID(),
				Image:     sb.Image,
				Name:      sb.Name,
				CPUs:      sb.CPUs,
				MemoryMB:  sb.MemoryMB,
				Mode:      sb.Mode,
				CreatedAt: sb.CreatedAt,
				LastUsed:  sb.CreatedAt,
			}
			if err := app.Store.PutSandbox(rec); err != nil {
				return err
			}
			return writeSandbox(cmd, g, rec)
		},
	}
	cmd.Flags().StringVar(&image, "image", "", "container image (recorded; ignored by AWS backend)")
	cmd.Flags().Float64Var(&cpus, "cpus", 0, "requested vCPUs")
	cmd.Flags().IntVar(&mem, "memory", 0, "requested memory (MB)")
	cmd.Flags().StringVar(&name, "name", "", "human-friendly name")
	cmd.Flags().StringVar(&fromSnap, "from-snapshot", "", "snapshot id to resume from")
	return cmd
}

func writeSandbox(cmd *cobra.Command, g *GlobalFlags, sb state.Sandbox) error {
	if g.Output == "json" {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(sb)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", sb.ID, sb.Provider, sb.Name)
	return nil
}
