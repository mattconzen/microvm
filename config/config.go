package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	DefaultProvider string    `yaml:"default_provider"`
	AWS             AWSConfig `yaml:"aws"`
}

type AWSConfig struct {
	Region           string `yaml:"region"`
	AgentRuntimeArn  string `yaml:"agent_runtime_arn"`
	ECRImage         string `yaml:"ecr_image"`
	ECRImageDigest   string `yaml:"ecr_image_digest"`
}

func Dir() (string, error) {
	if d := os.Getenv("MICROVM_HOME"); d != "" {
		return d, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".microvm"), nil
}

func Path() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.yaml"), nil
}

func Load() (*Config, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{DefaultProvider: "aws"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	if c.DefaultProvider == "" {
		c.DefaultProvider = "aws"
	}
	return &c, nil
}

func Save(c *Config) error {
	d, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	p, err := Path()
	if err != nil {
		return err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}
