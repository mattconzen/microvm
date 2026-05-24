package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/mattconzen/microvm/backend"
	awsbackend "github.com/mattconzen/microvm/backend/aws"
	"github.com/mattconzen/microvm/cli"
	"github.com/mattconzen/microvm/config"
	"github.com/mattconzen/microvm/obs"
	"github.com/mattconzen/microvm/state"
)

var version = "0.1.0-dev"

func main() {
	if err := run(); err != nil {
		var ee interface{ ExitCode() int }
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	logFormat := os.Getenv("MICROVM_LOG_FORMAT")
	if logFormat == "" {
		logFormat = "text"
	}
	logLevel := os.Getenv("MICROVM_LOG_LEVEL")
	if logLevel == "" {
		logLevel = "info"
	}
	obs.InitLogger(logFormat, logLevel)
	m := obs.InitMetrics(version)
	defer func() { _ = m.Close() }()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	store, err := state.Open()
	if err != nil {
		return fmt.Errorf("open state: %w", err)
	}
	defer store.Close()

	reg := backend.NewRegistry()
	aws, err := awsbackend.FromConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("init aws backend: %w", err)
	}
	aws.WithStore(store)
	reg.Register(aws)

	app := &cli.App{
		Version:  version,
		Config:   cfg,
		Registry: reg,
		Store:    store,
	}
	return cli.NewRoot(ctx, app).Execute()
}
