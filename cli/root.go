package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/mattconzen/microvm/backend"
	"github.com/mattconzen/microvm/config"
	"github.com/mattconzen/microvm/obs"
	"github.com/mattconzen/microvm/state"
)

type App struct {
	Version  string
	Config   *config.Config
	Registry *backend.Registry
	Store    *state.Store
}

type GlobalFlags struct {
	Provider  string
	ConfigDir string
	Output    string
	LogFormat string
	LogLevel  string
}

func NewRoot(ctx context.Context, app *App) *cobra.Command {
	g := &GlobalFlags{}
	cmd := &cobra.Command{
		Use:           "microvm",
		Short:         "TensorLake-style CLI for provisioning microVMs",
		Long:          "microvm is a CLI for provisioning isolated microVM sessions on AWS Bedrock AgentCore.",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	cmd.PersistentFlags().StringVar(&g.Provider, "provider", "", "backend (default: from config: aws)")
	cmd.PersistentFlags().StringVarP(&g.Output, "output", "o", "table", "output format: table|json")
	cmd.PersistentFlags().StringVar(&g.LogFormat, "log-format", "text", "log format: text|json")
	cmd.PersistentFlags().StringVar(&g.LogLevel, "log-level", "info", "log level: debug|info|warn|error")

	cmd.AddCommand(newVersionCmd(app))
	cmd.AddCommand(newLoginCmd(ctx, app, g))
	cmd.AddCommand(newSbxCmd(ctx, app, g))
	return cmd
}

// resolveBackend picks the backend named in --provider, falling back to the
// config default.
func resolveBackend(app *App, g *GlobalFlags) (backend.Backend, error) {
	name := g.Provider
	if name == "" {
		name = app.Config.DefaultProvider
	}
	if name == "" {
		name = "aws"
	}
	return app.Registry.Get(name)
}

// resolveBackendForID looks up the sandbox in state to find which backend owns
// it, ignoring --provider so that `microvm sbx exec mvm_xxx` always routes
// correctly.
func resolveBackendForID(app *App, id string) (backend.Backend, state.Sandbox, error) {
	sb, err := app.Store.GetSandbox(id)
	if err != nil {
		return nil, sb, err
	}
	b, err := app.Registry.Get(sb.Provider)
	return b, sb, err
}

// helper: kill log context warnings about unused
var _ = obs.L
