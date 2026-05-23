package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/mattconzen/microvm/backend"
)

func newExecCmd(ctx context.Context, app *App, g *GlobalFlags) *cobra.Command {
	var tty bool
	cmd := &cobra.Command{
		Use:   "exec <id> -- <cmd...>",
		Short: "Run a command inside a sandbox",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			cmdArgs := args[1:]
			// Tolerate the conventional "--" separator before the command.
			if len(cmdArgs) > 0 && cmdArgs[0] == "--" {
				cmdArgs = cmdArgs[1:]
			}
			if len(cmdArgs) == 0 {
				return fmt.Errorf("exec: no command given")
			}
			b, sb, err := resolveBackendForID(app, id)
			if err != nil {
				return err
			}
			sbApi := backend.Sandbox{
				ID:        sb.ID,
				Provider:  sb.Provider,
				SessionID: sb.SessionID,
				Image:     sb.Image,
				Name:      sb.Name,
			}
			code, err := b.Exec(ctx, sbApi, cmdArgs, backend.ExecIO{
				Stdin:  os.Stdin,
				Stdout: cmd.OutOrStdout(),
				Stderr: cmd.ErrOrStderr(),
				TTY:    tty,
			})
			// Update last-used.
			sb.LastUsed = time.Now()
			_ = app.Store.PutSandbox(sb)
			if err != nil {
				return err
			}
			if code != 0 {
				return &exitErr{code: code}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&tty, "tty", false, "request a pseudo-TTY")
	return cmd
}

type exitErr struct{ code int }

func (e *exitErr) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e *exitErr) ExitCode() int { return e.code }
