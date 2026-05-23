package cli

import (
	"context"

	"github.com/spf13/cobra"
)

func newSbxCmd(ctx context.Context, app *App, g *GlobalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "sbx",
		Short:   "Manage microVM sandboxes",
		Aliases: []string{"sandbox"},
	}
	cmd.AddCommand(
		newCreateCmd(ctx, app, g),
		newListCmd(ctx, app, g),
		newGetCmd(ctx, app, g),
		newExecCmd(ctx, app, g),
		newCpCmd(ctx, app, g),
		newShellCmd(ctx, app, g),
		newSnapshotCmd(ctx, app, g),
		newSnapshotsCmd(ctx, app, g),
		newResumeCmd(ctx, app, g),
		newTerminateCmd(ctx, app, g),
	)
	return cmd
}
