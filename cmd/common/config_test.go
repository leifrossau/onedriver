package common

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

const configTestDir = "pkg/resources/test"

// We should load config correctly.
func TestLoadConfig(t *testing.T) {
	t.Parallel()

	conf := LoadConfig(filepath.Join(configTestDir, "config-test.yml"))

	home, _ := os.UserHomeDir()
	assert.Equal(t, filepath.Join(home, "somewhere/else"), conf.CacheDir)
	assert.Equal(t, "warn", conf.LogLevel)
}

func TestConfigMerge(t *testing.T) {
	t.Parallel()

	conf := LoadConfig(filepath.Join(configTestDir, "config-test-merge.yml"))

	assert.Equal(t, "debug", conf.LogLevel)
	assert.Equal(t, "/some/directory", conf.CacheDir)
}

// We should come up with the defaults if there is no config file.
func TestLoadNonexistentConfig(t *testing.T) {
	t.Parallel()

	conf := LoadConfig(filepath.Join(configTestDir, "does-not-exist.yml"))

	home, _ := os.UserHomeDir()
	assert.Equal(t, filepath.Join(home, ".cache/onedriver"), conf.CacheDir)
	assert.Equal(t, "debug", conf.LogLevel)
}

func TestWriteConfig(t *testing.T) {
	t.Parallel()
	conf := LoadConfig(filepath.Join(configTestDir, "config-test.yml"))
	assert.NoError(t, conf.WriteConfig("tmp/nested/config.yml"))
}

func TestParseSize(t *testing.T) {
	t.Parallel()
	tests := map[string]uint64{
		"":      0,
		"0":     0,
		"1234":  1234,
		"1K":    1024,
		"10G":   10 << 30,
		"10GB":  10 << 30,
		"10GiB": 10 << 30,
		"512m":  512 << 20,
		"1.5T":  3 << 39,
		" 2 G ": 2 << 30,
		"100B":  100,
	}
	for in, want := range tests {
		got, err := ParseSize(in)
		assert.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{"abc", "-1G", "G", "10X"} {
		_, err := ParseSize(in)
		assert.Error(t, err, in)
	}
}

func TestLoadConfigEviction(t *testing.T) {
	t.Parallel()
	conf := LoadConfig(filepath.Join(configTestDir, "config-test-eviction.yml"))
	assert.Equal(t, "10G", conf.CacheLimit)
	assert.Equal(t, 30, conf.EvictAfterDays)
}
