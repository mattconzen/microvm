package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mattconzen/monorepo/apps/microvm/backend"
)

func newCpCmd(ctx context.Context, app *App, g *GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "cp <src> <dst>",
		Short: "Copy a file to/from a sandbox (either side may be <id>:/path)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			src, dst := args[0], args[1]
			srcID, srcPath, srcRemote := parseCpArg(src)
			dstID, dstPath, dstRemote := parseCpArg(dst)
			if srcRemote == dstRemote {
				return fmt.Errorf("cp: exactly one side must be a remote <id>:/path (got src=%q dst=%q)", src, dst)
			}
			var id, remote, local string
			direction := "to"
			if srcRemote {
				id, remote, local = srcID, srcPath, dstPath
				direction = "from"
			} else {
				id, remote, local = dstID, dstPath, srcPath
			}
			b, sb, err := resolveBackendForID(app, id)
			if err != nil {
				return err
			}
			sbApi := backend.Sandbox{ID: sb.ID, Provider: sb.Provider, SessionID: sb.SessionID}

			var n int64
			if direction == "to" {
				n, err = b.CopyTo(ctx, sbApi, local, remote)
			} else {
				n, err = b.CopyFrom(ctx, sbApi, remote, local)
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "copied %d bytes\n", n)
			return nil
		},
	}
}

// parseCpArg splits "id:/path" into (id, path, true); otherwise returns ("", arg, false).
func parseCpArg(s string) (id, path string, remote bool) {
	// Don't confuse with "C:" Windows paths — IDs are mvm_xxx so a colon-prefixed match is enough.
	if !strings.HasPrefix(s, "mvm_") {
		return "", s, false
	}
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return "", s, false
	}
	return s[:i], s[i+1:], true
}
