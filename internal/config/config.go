package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server       ServerConfig       `yaml:"server"`
	Database     DatabaseConfig     `yaml:"database"`
	Upload       UploadConfig       `yaml:"upload"`
	RetryPolicy  RetryPolicyConfig  `yaml:"retry_policy"`
	RateLimits   RateLimitsConfig   `yaml:"rate_limits"`
	Concurrency  ConcurrencyConfig  `yaml:"concurrency"`
	Quota        QuotaConfig        `yaml:"quota"`
	Verification VerificationConfig `yaml:"verification"`
}

type ServerConfig struct {
	Port int    `yaml:"port"`
	Host string `yaml:"host"`
}

type DatabaseConfig struct {
	Path string `yaml:"path"`
}

type UploadConfig struct {
	ChunkSizeMB int    `yaml:"chunk_size_mb"`
	TempDir     string `yaml:"temp_dir"`
}

type RetryPolicyConfig struct {
	MaxAttempts           int `yaml:"max_attempts"`
	InitialBackoffSeconds int `yaml:"initial_backoff_seconds"`
	MaxBackoffSeconds     int `yaml:"max_backoff_seconds"`
	BackoffMultiplier     int `yaml:"backoff_multiplier"`
}

type ProviderRateLimit struct {
	RequestsPerSecond    int `yaml:"requests_per_second"`
	BandwidthMBPerSecond int `yaml:"bandwidth_mb_per_second"`
}

type RateLimitsConfig struct {
	Mega       ProviderRateLimit `yaml:"mega"`
	FourShared ProviderRateLimit `yaml:"fourshared"`
}

type ConcurrencyConfig struct {
	MaxConcurrentUploads    int `yaml:"max_concurrent_uploads"`
	MaxConcurrentPerAccount int `yaml:"max_concurrent_per_account"`
	MaxWorkers              int `yaml:"max_workers"`
}

type QuotaConfig struct {
	SyncIntervalMinutes int  `yaml:"sync_interval_minutes"`
	CacheInDB           bool `yaml:"cache_in_db"`
}

type VerificationConfig struct {
	Enabled           bool `yaml:"enabled"`
	VerifyOnUpload    bool `yaml:"verify_on_upload"`
	PeriodicCheckDays int  `yaml:"periodic_check_days"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	setDefaults(cfg)
	return cfg, nil
}

func setDefaults(cfg *Config) {
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	if cfg.Server.Host == "" {
		cfg.Server.Host = "localhost"
	}
	if cfg.Database.Path == "" {
		cfg.Database.Path = "backmeup.db"
	}
	if cfg.Upload.ChunkSizeMB == 0 {
		cfg.Upload.ChunkSizeMB = 100
	}
	if cfg.RetryPolicy.MaxAttempts == 0 {
		cfg.RetryPolicy.MaxAttempts = 3
	}
	if cfg.RetryPolicy.InitialBackoffSeconds == 0 {
		cfg.RetryPolicy.InitialBackoffSeconds = 2
	}
	if cfg.RetryPolicy.MaxBackoffSeconds == 0 {
		cfg.RetryPolicy.MaxBackoffSeconds = 60
	}
	if cfg.RetryPolicy.BackoffMultiplier == 0 {
		cfg.RetryPolicy.BackoffMultiplier = 2
	}
	if cfg.RateLimits.Mega.RequestsPerSecond == 0 {
		cfg.RateLimits.Mega.RequestsPerSecond = 10
	}
	if cfg.RateLimits.Mega.BandwidthMBPerSecond == 0 {
		cfg.RateLimits.Mega.BandwidthMBPerSecond = 5
	}
	if cfg.RateLimits.FourShared.RequestsPerSecond == 0 {
		cfg.RateLimits.FourShared.RequestsPerSecond = 5
	}
	if cfg.RateLimits.FourShared.BandwidthMBPerSecond == 0 {
		cfg.RateLimits.FourShared.BandwidthMBPerSecond = 3
	}
	if cfg.Concurrency.MaxConcurrentUploads == 0 {
		cfg.Concurrency.MaxConcurrentUploads = 2
	}
	if cfg.Concurrency.MaxConcurrentPerAccount == 0 {
		cfg.Concurrency.MaxConcurrentPerAccount = 1
	}
	if cfg.Concurrency.MaxWorkers == 0 {
		cfg.Concurrency.MaxWorkers = 5
	}
	if cfg.Quota.SyncIntervalMinutes == 0 {
		cfg.Quota.SyncIntervalMinutes = 60
	}
	if cfg.Verification.PeriodicCheckDays == 0 {
		cfg.Verification.PeriodicCheckDays = 30
	}
}
