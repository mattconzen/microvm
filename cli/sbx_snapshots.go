package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mattconzen/microvm/state"
)

func newSnapshotsCmd(ctx context.Context, app *App, g *GlobalFlags) *cobra.Command {
	var sandboxID string
	cmd := &cobra.Command{
		Use:   "snapshots",
		Short: "List local snapshots",
		RunE: func(cmd *cobra.Command, args []string) error {
			rows, err := app.Store.ListSnapshots()
			if err != nil {
				return err
			}
			if sandboxID != "" {
				rows = filterSnapshotsBySandbox(rows, sandboxID)
			}
			sort.Slice(rows, func(i, j int) bool { return rows[i].CreatedAt.Before(rows[j].CreatedAt) })
			if g.Output == "json" {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSANDBOX\tPROVIDER\tKIND\tNAME\tCREATED")
			for _, r := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					r.ID, r.SandboxID, r.Provider, r.Kind, r.Name, r.CreatedAt.Format("2006-01-02 15:04:05"))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&sandboxID, "sandbox", "", "only show snapshots for this sandbox id")
	return cmd
}

func filterSnapshotsBySandbox(in []state.Snapshot, sandboxID string) []state.Snapshot {
	out := in[:0]
	for _, s := range in {
		if s.SandboxID == sandboxID {
			out = append(out, s)
		}
	}
	return out
}
