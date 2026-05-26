package cli

import (
	"context"
	"encoding/json"
	"fmt"

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
