package common

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"dario.cat/mergo"
	"github.com/jstaf/onedriver/fs/graph"
	"github.com/jstaf/onedriver/ui"
	"github.com/rs/zerolog/log"
	yaml "gopkg.in/yaml.v3"
)

type Config struct {
	CacheDir string `yaml:"cacheDir"`
	LogLevel string `yaml:"log"`
	// CacheLimit is the maximum size of cached file content, like "10G" or
	// "500M" (binary units) or a plain number of bytes. When exceeded, the
	// least recently used content that is not pinned is evicted. Empty or "0"
	// means no limit (the default).
	CacheLimit string `yaml:"cacheLimit,omitempty"`
	// EvictAfterDays evicts the cached content of files that are not pinned
	// and were not opened for this many days. 0 disables it (the default).
	EvictAfterDays   int `yaml:"evictAfterDays,omitempty"`
	graph.AuthConfig `yaml:"auth"`
}

// ParseSize parses a size like "10G", "512MiB", "1.5T" or "1048576" into
// bytes. Units are binary (K = 1024). An empty string is 0.
func ParseSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	upper := strings.ToUpper(s)
	upper = strings.TrimSuffix(upper, "B")
	upper = strings.TrimSuffix(upper, "I")
	multiplier := uint64(1)
	if n := len(upper); n > 0 {
		switch upper[n-1] {
		case 'K':
			multiplier = 1 << 10
		case 'M':
			multiplier = 1 << 20
		case 'G':
			multiplier = 1 << 30
		case 'T':
			multiplier = 1 << 40
		}
		if multiplier > 1 {
			upper = upper[:n-1]
		}
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(upper), 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return uint64(value * float64(multiplier)), nil
}

// DefaultConfigPath returns the default config location for onedriver
func DefaultConfigPath() string {
	confDir, err := os.UserConfigDir()
	if err != nil {
		log.Error().Err(err).Msg("Could not determine configuration directory.")
	}
	return filepath.Join(confDir, "onedriver/config.yml")
}

// LoadConfig is the primary way of loading onedriver's config
func LoadConfig(path string) *Config {
	xdgCacheDir, _ := os.UserCacheDir()
	defaults := Config{
		CacheDir: filepath.Join(xdgCacheDir, "onedriver"),
		LogLevel: "debug",
	}

	conf, err := os.ReadFile(path)
	if err != nil {
		log.Warn().
			Err(err).
			Str("path", path).
			Msg("Configuration file not found, using defaults.")
		return &defaults
	}
	config := &Config{}
	if err = yaml.Unmarshal(conf, config); err != nil {
		log.Error().
			Err(err).
			Str("path", path).
			Msg("Could not parse configuration file, using defaults.")
	}
	if err = mergo.Merge(config, defaults); err != nil {
		log.Error().
			Err(err).
			Str("path", path).
			Msg("Could not merge configuration file with defaults, using defaults only.")
	}

	config.CacheDir = ui.UnescapeHome(config.CacheDir)
	return config
}

// Write config to a file
func (c Config) WriteConfig(path string) error {
	out, err := yaml.Marshal(c)
	if err != nil {
		log.Error().Err(err).Msg("Could not marshal config!")
		return err
	}
	os.MkdirAll(filepath.Dir(path), 0700)
	err = os.WriteFile(path, out, 0600)
	if err != nil {
		log.Error().Err(err).Msg("Could not write config to disk.")
	}
	return err
}
