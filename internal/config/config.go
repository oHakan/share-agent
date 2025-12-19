// Package config provides configuration management using Viper.
// It supports loading from config files, environment variables, and defaults.
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config holds all configuration values for the agent.
type Config struct {
	// DevMode enables development-friendly logging and behaviors
	DevMode bool `mapstructure:"dev_mode"`

	// DockerRequired determines if the agent should fail when Docker is unavailable
	// If true, agent exits on Docker connection failure
	// If false, agent warns but continues in degraded mode
	DockerRequired bool `mapstructure:"docker_required"`

	// LogLevel sets the minimum log level (debug, info, warn, error)
	LogLevel string `mapstructure:"log_level"`

	// DockerTimeout is the maximum time to wait for Docker daemon response
	DockerTimeout time.Duration `mapstructure:"docker_timeout"`

	// GPUTimeout is the maximum time to wait for GPU discovery
	GPUTimeout time.Duration `mapstructure:"gpu_timeout"`

	// AgentVersion is the semantic version of this agent
	AgentVersion string `mapstructure:"agent_version"`

	// OrchestratorAddress is the gRPC address of the orchestrator
	OrchestratorAddress string `mapstructure:"orchestrator_address"`

	// MaxRetries is the max number of retry attempts for connection/registration
	MaxRetries int `mapstructure:"max_retries"`

	// RetryBackoff is the initial backoff duration between retries
	RetryBackoff time.Duration `mapstructure:"retry_backoff"`
}

// DefaultConfig returns a Config with sensible default values.
func DefaultConfig() *Config {
	return &Config{
		DevMode:             false,
		DockerRequired:      false, // Default to optional for broader compatibility
		LogLevel:            "info",
		DockerTimeout:       5 * time.Second,
		GPUTimeout:          10 * time.Second,
		AgentVersion:        "0.1.0-mvp",
		OrchestratorAddress: "localhost:50051",
		MaxRetries:          5,
		RetryBackoff:        time.Second,
	}
}

// Load reads configuration from environment variables and config files.
// Environment variables take precedence over config file values.
// All environment variables should be prefixed with "AGENT_" (e.g., AGENT_DEV_MODE).
func Load() (*Config, error) {
	v := viper.New()

	// Set default values
	defaults := DefaultConfig()
	v.SetDefault("dev_mode", defaults.DevMode)
	v.SetDefault("docker_required", defaults.DockerRequired)
	v.SetDefault("log_level", defaults.LogLevel)
	v.SetDefault("docker_timeout", defaults.DockerTimeout)
	v.SetDefault("gpu_timeout", defaults.GPUTimeout)
	v.SetDefault("agent_version", defaults.AgentVersion)
	v.SetDefault("orchestrator_address", defaults.OrchestratorAddress)
	v.SetDefault("max_retries", defaults.MaxRetries)
	v.SetDefault("retry_backoff", defaults.RetryBackoff)

	// Configure environment variable handling
	// Environment variables are prefixed with AGENT_ and use underscores
	// Example: AGENT_DEV_MODE=true, AGENT_DOCKER_REQUIRED=false
	v.SetEnvPrefix("AGENT")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Optional: look for config file
	v.SetConfigName("agent")
	v.SetConfigType("yaml")
	v.AddConfigPath(".")           // Current directory
	v.AddConfigPath("./config")    // Config subdirectory
	v.AddConfigPath("/etc/agent/") // System-wide config (Linux)

	// Read config file if it exists (not an error if missing)
	if err := v.ReadInConfig(); err != nil {
		// Only ignore "config file not found" errors
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("reading config file: %w", err)
		}
		// Config file not found is acceptable - we'll use defaults + env vars
	}

	// Unmarshal into struct
	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("unmarshaling config: %w", err)
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return cfg, nil
}

// Validate checks that all configuration values are valid.
func (c *Config) Validate() error {
	validLogLevels := map[string]bool{
		"debug": true,
		"info":  true,
		"warn":  true,
		"error": true,
	}

	if !validLogLevels[strings.ToLower(c.LogLevel)] {
		return fmt.Errorf("invalid log_level %q: must be one of debug, info, warn, error", c.LogLevel)
	}

	if c.DockerTimeout <= 0 {
		return fmt.Errorf("docker_timeout must be positive, got %v", c.DockerTimeout)
	}

	if c.GPUTimeout <= 0 {
		return fmt.Errorf("gpu_timeout must be positive, got %v", c.GPUTimeout)
	}

	return nil
}

// String returns a string representation of the config (useful for logging).
func (c *Config) String() string {
	return fmt.Sprintf(
		"Config{DevMode: %v, DockerRequired: %v, LogLevel: %s, DockerTimeout: %v, GPUTimeout: %v, Version: %s}",
		c.DevMode, c.DockerRequired, c.LogLevel, c.DockerTimeout, c.GPUTimeout, c.AgentVersion,
	)
}
