package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

func newGetCmd(ctx context.Context, app *App, g *GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "get <id>",
		Short: "Inspect a sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sb, err := app.Store.GetSandbox(args[0])
			if err != nil {
				return err
			}
			if g.Output == "json" {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(sb)
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"id:         %s\nprovider:   %s\nsession_id: %s\nname:       %s\nimage:      %s\ncreated_at: %s\nlast_used:  %s\n",
				sb.ID, sb.Provider, sb.SessionID, sb.Name, sb.Image, sb.CreatedAt, sb.LastUsed)
			return nil
		},
	}
}
