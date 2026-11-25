package config_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"headunit/internal/config"
)

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"PORT", "MUSIC_DIR", "DATA_DIR", "SCAN_INTERVAL"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("os.Unsetenv(%q): %v", key, err)
		}
	}
}

func TestLoad_Valid(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want config.Config
	}{
		{
			name: "defaults when unset",
			want: config.Config{
				Port:         "4533",
				MusicDir:     "/music",
				DataDir:      "/data",
				ScanInterval: 5 * time.Minute,
			},
		},
		{
			name: "empty and whitespace fall back to defaults",
			env: map[string]string{
				"PORT":          "   ",
				"MUSIC_DIR":     "",
				"DATA_DIR":      "\t\n ",
				"SCAN_INTERVAL": "  ",
			},
			want: config.Config{
				Port:         "4533",
				MusicDir:     "/music",
				DataDir:      "/data",
				ScanInterval: 5 * time.Minute,
			},
		},
		{
			name: "custom overrides with trailing slashes cleaned",
			env: map[string]string{
				"PORT":          "8080",
				"MUSIC_DIR":     " /mnt/audio/// ",
				"DATA_DIR":      "/var/lib/headunit/",
				"SCAN_INTERVAL": "15m",
			},
			want: config.Config{
				Port:         "8080",
				MusicDir:     "/mnt/audio",
				DataDir:      "/var/lib/headunit",
				ScanInterval: 15 * time.Minute,
			},
		},
		{
			name: "port lower bound",
			env:  map[string]string{"PORT": "1"},
			want: config.Config{
				Port:         "1",
				MusicDir:     "/music",
				DataDir:      "/data",
				ScanInterval: 5 * time.Minute,
			},
		},
		{
			name: "port upper bound",
			env:  map[string]string{"PORT": "65535"},
			want: config.Config{
				Port:         "65535",
				MusicDir:     "/music",
				DataDir:      "/data",
				ScanInterval: 5 * time.Minute,
			},
		},
		{
			name: "port trimmed and normalized",
			env:  map[string]string{"PORT": " 4533 "},
			want: config.Config{
				Port:         "4533",
				MusicDir:     "/music",
				DataDir:      "/data",
				ScanInterval: 5 * time.Minute,
			},
		},
		{
			name: "port leading zeros normalized",
			env:  map[string]string{"PORT": "04533"},
			want: config.Config{
				Port:         "4533",
				MusicDir:     "/music",
				DataDir:      "/data",
				ScanInterval: 5 * time.Minute,
			},
		},
		{
			name: "scan interval zero disables polling",
			env:  map[string]string{"SCAN_INTERVAL": "0"},
			want: config.Config{
				Port:         "4533",
				MusicDir:     "/music",
				DataDir:      "/data",
				ScanInterval: 0,
			},
		},
		{
			name: "scan interval 0s disables polling",
			env:  map[string]string{"SCAN_INTERVAL": "0s"},
			want: config.Config{
				Port:         "4533",
				MusicDir:     "/music",
				DataDir:      "/data",
				ScanInterval: 0,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			got, err := config.Load()
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("Load() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLoad_Invalid(t *testing.T) {
	tests := []struct {
		name        string
		envKey      string
		envVal      string
		wantContain string
	}{
		{name: "port zero", envKey: "PORT", envVal: "0", wantContain: "PORT"},
		{name: "port negative", envKey: "PORT", envVal: "-1", wantContain: "PORT"},
		{name: "port plus prefix", envKey: "PORT", envVal: "+4533", wantContain: "PORT"},
		{name: "port overflow", envKey: "PORT", envVal: "65536", wantContain: "PORT"},
		{name: "port non-numeric", envKey: "PORT", envVal: "abc", wantContain: "PORT"},
		{name: "port colon prefix", envKey: "PORT", envVal: ":4533", wantContain: "PORT"},
		{name: "scan interval negative seconds", envKey: "SCAN_INTERVAL", envVal: "-1s", wantContain: "SCAN_INTERVAL"},
		{name: "scan interval negative minutes", envKey: "SCAN_INTERVAL", envVal: "-5m", wantContain: "SCAN_INTERVAL"},
		{name: "scan interval missing unit", envKey: "SCAN_INTERVAL", envVal: "5", wantContain: "SCAN_INTERVAL"},
		{name: "scan interval invalid string", envKey: "SCAN_INTERVAL", envVal: "not-a-duration", wantContain: "SCAN_INTERVAL"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv(tc.envKey, tc.envVal)

			got, err := config.Load()
			if err == nil {
				t.Fatalf("Load() with %s=%q succeeded with %+v, want error", tc.envKey, tc.envVal, got)
			}
			if !strings.Contains(err.Error(), tc.wantContain) {
				t.Errorf("Load() error = %q, want substring %q", err.Error(), tc.wantContain)
			}
		})
	}
}
