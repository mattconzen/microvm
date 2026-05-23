package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newListCmd(ctx context.Context, app *App, g *GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List local sandboxes",
		RunE: func(cmd *cobra.Command, args []string) error {
			rows, err := app.Store.ListSandboxes()
			if err != nil {
				return err
			}
			sort.Slice(rows, func(i, j int) bool { return rows[i].CreatedAt.Before(rows[j].CreatedAt) })
			if g.Output == "json" {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tPROVIDER\tNAME\tIMAGE\tCREATED")
			for _, r := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.Provider, r.Name, r.Image, r.CreatedAt.Format("2006-01-02 15:04:05"))
			}
			return tw.Flush()
		},
	}
}
