package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	keySize               = 32
	keyHexLen             = keySize * 2
	keyFileName           = "auth.key"
	maxKeyFileReadBytes   = 1024
	passwordCipherVersion = byte(0x01)

	apiKeyPrefixHeader   = "hu_"
	apiKeyRandomBytes    = 32
	apiKeyPrefixHexChars = 8
)

// LoadOrCreateKey resolves the 32-byte AES-256 master key used for password
// encryption. When envKeyHex is non-empty, it decodes the 64-character hex key
// without inspecting or modifying dataDir. Otherwise it reads or atomically
// creates <dataDir>/auth.key with mode 0600.
func LoadOrCreateKey(dataDir, envKeyHex string) ([]byte, error) {
	if envKeyHex != "" {
		return decodeKeyHex(envKeyHex, "AUTH_SECRET_KEY")
	}
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("auth: data directory must not be empty")
	}

	keyPath := filepath.Join(dataDir, keyFileName)
	key, err := readKeyFile(keyPath)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	return createKeyFile(dataDir, keyPath)
}

func decodeKeyHex(raw, source string) ([]byte, error) {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) != keyHexLen {
		return nil, fmt.Errorf("auth: %s must be %d hexadecimal characters (%d bytes)", source, keyHexLen, keySize)
	}
	key, err := hex.DecodeString(trimmed)
	if err != nil || len(key) != keySize {
		// Never wrap hex.InvalidByteError or echo raw, because it may contain secret key material.
		return nil, fmt.Errorf("auth: %s must be %d hexadecimal characters (%d bytes)", source, keyHexLen, keySize)
	}
	return key, nil
}

func readKeyFile(keyPath string) ([]byte, error) {
	linfo, err := os.Lstat(keyPath)
	if err != nil {
		return nil, err
	}
	if linfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("auth: key file %q must not be a symlink", keyPath)
	}
	if !linfo.Mode().IsRegular() {
		return nil, fmt.Errorf("auth: key file %q must be a regular file", keyPath)
	}

	f, err := os.Open(keyPath)
	if err != nil {
		return nil, fmt.Errorf("auth: open key file %q: %w", keyPath, err)
	}
	defer f.Close()

	finfo, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("auth: stat open key file %q: %w", keyPath, err)
	}
	if !finfo.Mode().IsRegular() || !os.SameFile(linfo, finfo) {
		return nil, fmt.Errorf("auth: key file %q changed during read or is not a regular file", keyPath)
	}

	data, err := io.ReadAll(io.LimitReader(f, maxKeyFileReadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("auth: read key file %q: %w", keyPath, err)
	}
	if len(data) > maxKeyFileReadBytes {
		return nil, fmt.Errorf("auth: key file %q exceeds maximum size (%d bytes)", keyPath, maxKeyFileReadBytes)
	}
	return decodeKeyHex(string(data), "key file")
}

func createKeyFile(dataDir, keyPath string) ([]byte, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("auth: create data directory %q: %w", dataDir, err)
	}

	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("auth: generate key: %w", err)
	}

	tmpFile, err := os.CreateTemp(dataDir, ".auth.key-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("auth: create temp key file: %w", err)
	}
	tmpPath := tmpFile.Name()
	closed := false
	defer func() {
		if !closed {
			_ = tmpFile.Close()
		}
		_ = os.Remove(tmpPath)
	}()

	if err := tmpFile.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("auth: chmod temp key file: %w", err)
	}
	if _, err := io.WriteString(tmpFile, hex.EncodeToString(key)+"\n"); err != nil {
		return nil, fmt.Errorf("auth: write temp key file: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		return nil, fmt.Errorf("auth: sync temp key file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("auth: close temp key file: %w", err)
	}
	closed = true

	if err := os.Link(tmpPath, keyPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			// Another writer won the race and published a synced key file.
			return readKeyFile(keyPath)
		}
		return nil, fmt.Errorf("auth: link key file %q: %w", keyPath, err)
	}
	return key, nil
}

// PasswordCipher encrypts and decrypts stored Subsonic account passwords using
// AES-256-GCM. It is safe for concurrent use after construction.
type PasswordCipher struct {
	aead cipher.AEAD
}

// NewPasswordCipher returns a PasswordCipher initialized with a 32-byte AES-256 key.
func NewPasswordCipher(key []byte) (*PasswordCipher, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("auth: password cipher key must be %d bytes", keySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("auth: create AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("auth: create GCM cipher: %w", err)
	}
	return &PasswordCipher{aead: aead}, nil
}

// Encrypt encrypts plaintext verbatim using a random 12-byte nonce and wire
// layout [0x01][12-byte nonce][ciphertext + 16-byte GCM tag].
func (c *PasswordCipher) Encrypt(plaintext string) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, errors.New("auth: password cipher is not initialized")
	}
	nonceSize := c.aead.NonceSize()
	out := make([]byte, 1+nonceSize, 1+nonceSize+len(plaintext)+c.aead.Overhead())
	out[0] = passwordCipherVersion
	nonce := out[1 : 1+nonceSize]
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("auth: generate encryption nonce: %w", err)
	}
	// Bind the 1-byte version header as AAD so tampering with the version fails authentication.
	return c.aead.Seal(out, nonce, []byte(plaintext), out[:1]), nil
}

// Decrypt validates and decrypts a blob produced by Encrypt, returning the
// original password verbatim.
func (c *PasswordCipher) Decrypt(blob []byte) (string, error) {
	if c == nil || c.aead == nil {
		return "", errors.New("auth: password cipher is not initialized")
	}
	nonceSize := c.aead.NonceSize()
	minLen := 1 + nonceSize + c.aead.Overhead()
	if len(blob) < minLen {
		return "", errors.New("auth: invalid encrypted password")
	}
	if blob[0] != passwordCipherVersion {
		return "", errors.New("auth: unsupported encrypted password version")
	}
	nonce := blob[1 : 1+nonceSize]
	ciphertext := blob[1+nonceSize:]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, blob[:1])
	if err != nil {
		// Never wrap underlying cipher details or echo ciphertext bytes.
		return "", errors.New("auth: failed to decrypt password")
	}
	return string(plaintext), nil
}

// GenerateAPIKey creates a new random Headunit OpenSubsonic API key along with
// its non-secret display prefix and SHA-256 hex digest.
func GenerateAPIKey() (plaintext, prefix, hash string, err error) {
	raw := make([]byte, apiKeyRandomBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", "", fmt.Errorf("auth: generate api key: %w", err)
	}
	plaintext = apiKeyPrefixHeader + hex.EncodeToString(raw)
	prefix = plaintext[:len(apiKeyPrefixHeader)+apiKeyPrefixHexChars]
	hash = HashAPIKey(plaintext)
	return plaintext, prefix, hash, nil
}

// HashAPIKey returns the lowercase hex SHA-256 digest of raw.
func HashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
