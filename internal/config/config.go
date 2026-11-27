// Package config loads and validates Headunit runtime configuration from
// environment variables.
package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultPort               = "4533"
	defaultMusicDir           = "/music"
	defaultDataDir            = "/data"
	defaultUploadsDirname     = "uploads"
	defaultScanInterval       = 5 * time.Minute
	defaultUploadsEnabled     = true
	defaultUploadMaxFileBytes = int64(256 << 20) // 268435456 (256 MiB)
)

// Config holds runtime settings loaded from environment variables.
type Config struct {
	// Port is the TCP listen port (1-65535; default "4533").
	Port string

	// MusicDir is the root directory scanned and watched for audio files (default "/music").
	MusicDir string

	// DataDir is the directory storing persistent server state (default "/data").
	DataDir string

	// UploadsDir is the server-managed directory for uploaded audio files, derived from DataDir.
	UploadsDir string

	// ScanInterval is the periodic library reconciliation interval; 0 disables polling (default 5m).
	ScanInterval time.Duration

	// AuthSecretKey is the optional 64-character lowercase hex secret key from AUTH_SECRET_KEY (32 bytes); "" when unset or blank.
	AuthSecretKey string

	// UploadsEnabled controls whether new uploads are accepted (UPLOADS_ENABLED; default true).
	UploadsEnabled bool

	// UploadMaxFileBytes is the maximum allowed size in bytes for an uploaded file (UPLOAD_MAX_FILE_BYTES; default 268435456, must be > 0).
	UploadMaxFileBytes int64
}

// Load reads configuration from PORT, MUSIC_DIR, DATA_DIR, SCAN_INTERVAL,
// AUTH_SECRET_KEY, UPLOADS_ENABLED, and UPLOAD_MAX_FILE_BYTES. Unset or
// whitespace-only variables fall back to defaults. Directory paths are
// normalized with filepath.Clean without inspecting or creating them on disk.
func Load() (Config, error) {
	cfg := Config{
		Port:               defaultPort,
		MusicDir:           defaultMusicDir,
		DataDir:            defaultDataDir,
		ScanInterval:       defaultScanInterval,
		UploadsEnabled:     defaultUploadsEnabled,
		UploadMaxFileBytes: defaultUploadMaxFileBytes,
	}

	if v := strings.TrimSpace(os.Getenv("PORT")); v != "" {
		portNum, err := strconv.ParseUint(v, 10, 16)
		if err != nil {
			return Config{}, fmt.Errorf("invalid PORT %q: %w", v, err)
		}
		if portNum < 1 {
			return Config{}, fmt.Errorf("invalid PORT %q: must be between 1 and 65535", v)
		}
		cfg.Port = strconv.FormatUint(portNum, 10)
	}

	if v := strings.TrimSpace(os.Getenv("MUSIC_DIR")); v != "" {
		cfg.MusicDir = filepath.Clean(v)
	}

	if v := strings.TrimSpace(os.Getenv("DATA_DIR")); v != "" {
		cfg.DataDir = filepath.Clean(v)
	}
	cfg.UploadsDir = filepath.Join(cfg.DataDir, defaultUploadsDirname)

	if v := strings.TrimSpace(os.Getenv("SCAN_INTERVAL")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("invalid SCAN_INTERVAL %q: %w", v, err)
		}
		if d < 0 {
			return Config{}, fmt.Errorf("invalid SCAN_INTERVAL %q: must be non-negative", v)
		}
		cfg.ScanInterval = d
	}

	if v := strings.TrimSpace(os.Getenv("AUTH_SECRET_KEY")); v != "" {
		if len(v) != 64 {
			return Config{}, errors.New("invalid AUTH_SECRET_KEY: must be 64 hexadecimal characters (32 bytes)")
		}
		decoded, err := hex.DecodeString(v)
		if err != nil || len(decoded) != 32 {
			return Config{}, errors.New("invalid AUTH_SECRET_KEY: must be 64 hexadecimal characters (32 bytes)")
		}
		cfg.AuthSecretKey = strings.ToLower(v)
	}

	if v := strings.TrimSpace(os.Getenv("UPLOADS_ENABLED")); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("invalid UPLOADS_ENABLED %q: %w", v, err)
		}
		cfg.UploadsEnabled = enabled
	}

	if v := strings.TrimSpace(os.Getenv("UPLOAD_MAX_FILE_BYTES")); v != "" {
		n, err := strconv.ParseUint(v, 10, 63)
		if err != nil {
			return Config{}, fmt.Errorf("invalid UPLOAD_MAX_FILE_BYTES %q: %w", v, err)
		}
		if n == 0 {
			return Config{}, fmt.Errorf("invalid UPLOAD_MAX_FILE_BYTES %q: must be greater than 0", v)
		}
		cfg.UploadMaxFileBytes = int64(n)
	}

	return cfg, nil
}
