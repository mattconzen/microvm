package cli

import (
	"context"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/mattconzen/monorepo/apps/microvm/backend"
)

func newShellCmd(ctx context.Context, app *App, g *GlobalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "shell <id>",
		Aliases: []string{"ssh"},
		Short:   "Open an interactive shell to a sandbox",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, sb, err := resolveBackendForID(app, args[0])
			if err != nil {
				return err
			}
			sbApi := backend.Sandbox{ID: sb.ID, Provider: sb.Provider, SessionID: sb.SessionID}

			cols, rows := uint16(80), uint16(24)
			stdinFd := int(os.Stdin.Fd())
			if term.IsTerminal(stdinFd) {
				if w, h, terr := term.GetSize(stdinFd); terr == nil {
					cols, rows = uint16(w), uint16(h)
				}
				old, terr := term.MakeRaw(stdinFd)
				if terr == nil {
					defer term.Restore(stdinFd, old)
				}
			}

			return b.Shell(ctx, sbApi, backend.TTY{
				In:   os.Stdin,
				Out:  cmd.OutOrStdout(),
				Cols: cols,
				Rows: rows,
			})
		},
	}
	return cmd
}
