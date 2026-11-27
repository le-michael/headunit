package auth_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"headunit/internal/auth"
)

const testKeyHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func mustDecodeHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex.DecodeString(%q): %v", s, err)
	}
	return b
}

func isLowerHex(s string) bool {
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func TestLoadOrCreateKey_EnvPrecedence(t *testing.T) {
	want := mustDecodeHex(t, testKeyHex)

	t.Run("valid lowercase hex ignores nonexistent dataDir", func(t *testing.T) {
		nonexistentDir := filepath.Join(t.TempDir(), "missing", "nested")
		got, err := auth.LoadOrCreateKey(nonexistentDir, testKeyHex)
		if err != nil {
			t.Fatalf("LoadOrCreateKey() error = %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("LoadOrCreateKey() = %x, want %x", got, want)
		}
		if _, statErr := os.Stat(nonexistentDir); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("dataDir stat = %v, want ErrNotExist", statErr)
		}
	})

	t.Run("valid uppercase hex with whitespace ignores empty dataDir", func(t *testing.T) {
		envVal := "  \t" + strings.ToUpper(testKeyHex) + "\n "
		got, err := auth.LoadOrCreateKey("", envVal)
		if err != nil {
			t.Fatalf("LoadOrCreateKey() error = %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("LoadOrCreateKey() = %x, want %x", got, want)
		}
	})

	invalidEnvCases := []struct {
		name      string
		envKeyHex string
		leakCheck string
	}{
		{name: "whitespace only", envKeyHex: "   \t\n ", leakCheck: "\t"},
		{name: "too short 62 chars", envKeyHex: testKeyHex[:62], leakCheck: testKeyHex[:16]},
		{name: "too long 66 chars", envKeyHex: testKeyHex + "ab", leakCheck: testKeyHex[:16]},
		{name: "odd length 63 chars", envKeyHex: testKeyHex[:63], leakCheck: testKeyHex[:16]},
		{name: "0x prefix 64 chars total", envKeyHex: "0x" + testKeyHex[:62], leakCheck: "0x"},
		{name: "0x prefix 66 chars total", envKeyHex: "0x" + testKeyHex, leakCheck: "0x"},
		{name: "non-hex character", envKeyHex: testKeyHex[:63] + "Z", leakCheck: "Z"},
	}

	for _, tc := range invalidEnvCases {
		t.Run("invalid_"+tc.name, func(t *testing.T) {
			nonexistentDir := filepath.Join(t.TempDir(), "never-created")
			_, err := auth.LoadOrCreateKey(nonexistentDir, tc.envKeyHex)
			if err == nil {
				t.Fatalf("LoadOrCreateKey() = nil error, want error")
			}
			if !strings.Contains(err.Error(), "AUTH_SECRET_KEY") {
				t.Errorf("error = %q, want mention of AUTH_SECRET_KEY", err.Error())
			}
			if strings.Contains(err.Error(), tc.envKeyHex) || strings.Contains(err.Error(), tc.leakCheck) {
				t.Errorf("error %q leaked secret env material %q", err.Error(), tc.leakCheck)
			}
			if _, statErr := os.Stat(nonexistentDir); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("dataDir stat = %v, want ErrNotExist", statErr)
			}
		})
	}
}

func TestLoadOrCreateKey_CreateAndReload(t *testing.T) {
	t.Run("rejects empty and whitespace dataDir", func(t *testing.T) {
		for _, dir := range []string{"", "   ", "\t\n"} {
			if _, err := auth.LoadOrCreateKey(dir, ""); err == nil {
				t.Errorf("LoadOrCreateKey(%q, \"\") = nil error, want error", dir)
			}
		}
	})

	t.Run("atomic creation with 0600 mode and stable reload", func(t *testing.T) {
		dataDir := filepath.Join(t.TempDir(), "nested", "data")

		created, err := auth.LoadOrCreateKey(dataDir, "")
		if err != nil {
			t.Fatalf("LoadOrCreateKey(create) error = %v", err)
		}
		if len(created) != 32 {
			t.Fatalf("created key len = %d, want 32", len(created))
		}

		keyPath := filepath.Join(dataDir, "auth.key")
		info, err := os.Lstat(keyPath)
		if err != nil {
			t.Fatalf("Lstat(%q) error = %v", keyPath, err)
		}
		if !info.Mode().IsRegular() {
			t.Errorf("auth.key mode = %v, want regular file", info.Mode())
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("auth.key permissions = %04o, want 0600", perm)
		}

		raw, err := os.ReadFile(keyPath)
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", keyPath, err)
		}
		wantFile := hex.EncodeToString(created) + "\n"
		if string(raw) != wantFile {
			t.Errorf("auth.key content = %q, want %q", string(raw), wantFile)
		}

		entries, err := os.ReadDir(dataDir)
		if err != nil {
			t.Fatalf("ReadDir(%q) error = %v", dataDir, err)
		}
		if len(entries) != 1 || entries[0].Name() != "auth.key" {
			t.Errorf("dataDir entries = %v, want only auth.key", entries)
		}

		reloaded, err := auth.LoadOrCreateKey(dataDir, "")
		if err != nil {
			t.Fatalf("LoadOrCreateKey(reload) error = %v", err)
		}
		if !bytes.Equal(reloaded, created) {
			t.Errorf("reloaded key = %x, want %x", reloaded, created)
		}
	})
}

func TestLoadOrCreateKey_RejectsUnsafeFiles(t *testing.T) {
	t.Run("symlink to valid key file", func(t *testing.T) {
		dataDir := t.TempDir()
		target := filepath.Join(t.TempDir(), "real.key")
		if err := os.WriteFile(target, []byte(testKeyHex+"\n"), 0o600); err != nil {
			t.Fatalf("WriteFile(target) error = %v", err)
		}
		linkPath := filepath.Join(dataDir, "auth.key")
		if err := os.Symlink(target, linkPath); err != nil {
			t.Fatalf("Symlink() error = %v", err)
		}

		if _, err := auth.LoadOrCreateKey(dataDir, ""); err == nil {
			t.Fatal("LoadOrCreateKey(symlink) = nil error, want error")
		}
	})

	t.Run("broken symlink", func(t *testing.T) {
		dataDir := t.TempDir()
		missingTarget := filepath.Join(dataDir, "missing.key")
		linkPath := filepath.Join(dataDir, "auth.key")
		if err := os.Symlink(missingTarget, linkPath); err != nil {
			t.Fatalf("Symlink() error = %v", err)
		}

		if _, err := auth.LoadOrCreateKey(dataDir, ""); err == nil {
			t.Fatal("LoadOrCreateKey(broken symlink) = nil error, want error")
		}
		if _, err := os.Stat(missingTarget); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("missingTarget stat = %v, want ErrNotExist", err)
		}
	})

	t.Run("directory at key path", func(t *testing.T) {
		dataDir := t.TempDir()
		keyPath := filepath.Join(dataDir, "auth.key")
		if err := os.Mkdir(keyPath, 0o700); err != nil {
			t.Fatalf("Mkdir() error = %v", err)
		}

		if _, err := auth.LoadOrCreateKey(dataDir, ""); err == nil {
			t.Fatal("LoadOrCreateKey(dir) = nil error, want error")
		}
		info, err := os.Lstat(keyPath)
		if err != nil || !info.IsDir() {
			t.Errorf("auth.key directory was altered: info=%v err=%v", info, err)
		}
	})

	t.Run("empty file is not overwritten", func(t *testing.T) {
		dataDir := t.TempDir()
		keyPath := filepath.Join(dataDir, "auth.key")
		if err := os.WriteFile(keyPath, nil, 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		if _, err := auth.LoadOrCreateKey(dataDir, ""); err == nil {
			t.Fatal("LoadOrCreateKey(empty file) = nil error, want error")
		}
		after, err := os.ReadFile(keyPath)
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}
		if len(after) != 0 {
			t.Errorf("empty file was modified, len = %d", len(after))
		}
	})

	t.Run("corrupt file does not leak contents and is not overwritten", func(t *testing.T) {
		dataDir := t.TempDir()
		keyPath := filepath.Join(dataDir, "auth.key")
		corrupt := "corrupt-key-material-Z99\n"
		if err := os.WriteFile(keyPath, []byte(corrupt), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		_, err := auth.LoadOrCreateKey(dataDir, "")
		if err == nil {
			t.Fatal("LoadOrCreateKey(corrupt file) = nil error, want error")
		}
		if strings.Contains(err.Error(), "corrupt-key-material") || strings.Contains(err.Error(), "Z99") {
			t.Errorf("error %q leaked corrupt key file contents", err.Error())
		}
		after, err := os.ReadFile(keyPath)
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}
		if string(after) != corrupt {
			t.Errorf("corrupt file was modified: got %q, want %q", string(after), corrupt)
		}
	})

	t.Run("oversized file exceeding 1024 bytes is rejected even with valid hex prefix", func(t *testing.T) {
		dataDir := t.TempDir()
		keyPath := filepath.Join(dataDir, "auth.key")
		oversized := testKeyHex + strings.Repeat(" ", 1025-len(testKeyHex))
		if err := os.WriteFile(keyPath, []byte(oversized), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		_, err := auth.LoadOrCreateKey(dataDir, "")
		if err == nil {
			t.Fatal("LoadOrCreateKey(oversized file) = nil error, want error")
		}
		if strings.Contains(err.Error(), testKeyHex[:16]) {
			t.Errorf("error %q leaked key file bytes", err.Error())
		}
	})

	t.Run("exact 1024-byte file with valid hex succeeds", func(t *testing.T) {
		dataDir := t.TempDir()
		keyPath := filepath.Join(dataDir, "auth.key")
		exact := testKeyHex + strings.Repeat(" ", 1024-len(testKeyHex))
		if err := os.WriteFile(keyPath, []byte(exact), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		got, err := auth.LoadOrCreateKey(dataDir, "")
		if err != nil {
			t.Fatalf("LoadOrCreateKey(1024 bytes) error = %v", err)
		}
		if !bytes.Equal(got, mustDecodeHex(t, testKeyHex)) {
			t.Errorf("key mismatch")
		}
	})
}

func TestLoadOrCreateKey_ConcurrentCreate(t *testing.T) {
	const workers = 32
	dataDir := t.TempDir()

	keys := make([][]byte, workers)
	errs := make([]error, workers)
	start := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		go func(id int) {
			defer wg.Done()
			<-start
			keys[id], errs[id] = auth.LoadOrCreateKey(dataDir, "")
		}(i)
	}

	close(start)
	wg.Wait()

	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("worker %d error = %v", i, errs[i])
		}
		if len(keys[i]) != 32 {
			t.Fatalf("worker %d key len = %d, want 32", i, len(keys[i]))
		}
		if !bytes.Equal(keys[i], keys[0]) {
			t.Errorf("worker %d key = %x, want %x", i, keys[i], keys[0])
		}
	}

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "auth.key" {
		t.Errorf("dataDir entries = %v, want only auth.key (no leftover temp files)", entries)
	}
}

func TestPasswordCipher_RoundTrip(t *testing.T) {
	c, err := auth.NewPasswordCipher(mustDecodeHex(t, testKeyHex))
	if err != nil {
		t.Fatalf("NewPasswordCipher() error = %v", err)
	}

	passwords := []struct {
		name string
		val  string
	}{
		{name: "standard ASCII", val: "correct-horse-battery-staple"},
		{name: "empty string", val: ""},
		{name: "leading and trailing spaces", val: "  secret password  "},
		{name: "newlines and tabs", val: "line1\r\nline2\t"},
		{name: "multi-byte UTF-8", val: "pässwörd-🎵-日本語"},
		{name: "embedded NUL byte", val: "before\x00after"},
		{name: "long password", val: strings.Repeat("A", 4096)},
	}

	for _, tc := range passwords {
		t.Run(tc.name, func(t *testing.T) {
			blob1, err := c.Encrypt(tc.val)
			if err != nil {
				t.Fatalf("Encrypt() error = %v", err)
			}
			if gotLen, wantLen := len(blob1), 29+len(tc.val); gotLen != wantLen {
				t.Errorf("ciphertext len = %d, want %d", gotLen, wantLen)
			}
			if blob1[0] != 0x01 {
				t.Errorf("version byte = %#x, want 0x01", blob1[0])
			}

			got, err := c.Decrypt(blob1)
			if err != nil {
				t.Fatalf("Decrypt() error = %v", err)
			}
			if got != tc.val {
				t.Errorf("Decrypt() = %q, want %q", got, tc.val)
			}

			blob2, err := c.Encrypt(tc.val)
			if err != nil {
				t.Fatalf("second Encrypt() error = %v", err)
			}
			if bytes.Equal(blob1, blob2) {
				t.Errorf("two encryptions of %q produced identical ciphertext blobs", tc.name)
			}
		})
	}

	t.Run("concurrent Encrypt and Decrypt", func(t *testing.T) {
		const workers = 32
		start := make(chan struct{})
		errs := make([]error, workers)

		var wg sync.WaitGroup
		wg.Add(workers)
		for i := range workers {
			go func(id int) {
				defer wg.Done()
				<-start
				pw := "  pass-" + strings.Repeat("x", id) + "  "
				blob, err := c.Encrypt(pw)
				if err != nil {
					errs[id] = err
					return
				}
				got, err := c.Decrypt(blob)
				if err != nil {
					errs[id] = err
					return
				}
				if got != pw {
					errs[id] = errors.New("round-trip mismatch")
				}
			}(i)
		}

		close(start)
		wg.Wait()

		for i := range workers {
			if errs[i] != nil {
				t.Errorf("worker %d error = %v", i, errs[i])
			}
		}
	})
}

func TestPasswordCipher_Errors(t *testing.T) {
	t.Run("invalid key lengths", func(t *testing.T) {
		for _, n := range []int{0, 16, 31, 33, 64} {
			if _, err := auth.NewPasswordCipher(make([]byte, n)); err == nil {
				t.Errorf("NewPasswordCipher(len=%d) = nil error, want error", n)
			}
		}
	})

	t.Run("nil receiver and uninitialized cipher", func(t *testing.T) {
		var nilCipher *auth.PasswordCipher
		if _, err := nilCipher.Encrypt("secret"); err == nil {
			t.Error("(*PasswordCipher)(nil).Encrypt() = nil error, want error")
		}
		if _, err := nilCipher.Decrypt(make([]byte, 32)); err == nil {
			t.Error("(*PasswordCipher)(nil).Decrypt() = nil error, want error")
		}

		zeroCipher := &auth.PasswordCipher{}
		if _, err := zeroCipher.Encrypt("secret"); err == nil {
			t.Error("(&PasswordCipher{}).Encrypt() = nil error, want error")
		}
		if _, err := zeroCipher.Decrypt(make([]byte, 32)); err == nil {
			t.Error("(&PasswordCipher{}).Decrypt() = nil error, want error")
		}
	})

	key1 := mustDecodeHex(t, testKeyHex)
	key2 := mustDecodeHex(t, "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	c1, err := auth.NewPasswordCipher(key1)
	if err != nil {
		t.Fatalf("NewPasswordCipher(key1) error = %v", err)
	}
	c2, err := auth.NewPasswordCipher(key2)
	if err != nil {
		t.Fatalf("NewPasswordCipher(key2) error = %v", err)
	}

	const secretPassword = "ultra-secret-password-value"
	validBlob, err := c1.Encrypt(secretPassword)
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}

	t.Run("truncated blobs", func(t *testing.T) {
		for _, n := range []int{0, 1, 12, 28} {
			if _, err := c1.Decrypt(validBlob[:n]); err == nil {
				t.Errorf("Decrypt(len=%d) = nil error, want error", n)
			}
		}
	})

	t.Run("wrong version byte", func(t *testing.T) {
		for _, v := range []byte{0x00, 0x02, 0xff} {
			tampered := bytes.Clone(validBlob)
			tampered[0] = v
			if _, err := c1.Decrypt(tampered); err == nil {
				t.Errorf("Decrypt(version=%#x) = nil error, want error", v)
			}
		}
	})

	t.Run("wrong key", func(t *testing.T) {
		_, err := c2.Decrypt(validBlob)
		if err == nil {
			t.Fatal("Decrypt(wrong key) = nil error, want error")
		}
		if strings.Contains(err.Error(), secretPassword) {
			t.Errorf("error %q leaked plaintext password", err.Error())
		}
	})

	t.Run("tampered nonce, ciphertext, and tag", func(t *testing.T) {
		positions := []struct {
			name string
			idx  int
		}{
			{name: "nonce", idx: 1},
			{name: "ciphertext", idx: 13},
			{name: "tag", idx: len(validBlob) - 1},
		}
		for _, pos := range positions {
			tampered := bytes.Clone(validBlob)
			tampered[pos.idx] ^= 0xff
			_, err := c1.Decrypt(tampered)
			if err == nil {
				t.Errorf("Decrypt(tampered %s) = nil error, want error", pos.name)
				continue
			}
			if strings.Contains(err.Error(), secretPassword) {
				t.Errorf("error %q leaked plaintext password", err.Error())
			}
		}
	})
}

func TestAPIKeyHelpers(t *testing.T) {
	t.Run("HashAPIKey deterministic lowercase SHA-256 hex", func(t *testing.T) {
		const raw = "hu_test_api_key"
		sum := sha256.Sum256([]byte(raw))
		want := hex.EncodeToString(sum[:])

		got := auth.HashAPIKey(raw)
		if got != want {
			t.Errorf("HashAPIKey() = %q, want %q", got, want)
		}
		if len(got) != 64 || !isLowerHex(got) {
			t.Errorf("HashAPIKey() = %q, want 64 lowercase hex chars", got)
		}
	})

	t.Run("GenerateAPIKey format and uniqueness", func(t *testing.T) {
		seen := make(map[string]struct{}, 16)
		for range 16 {
			plaintext, prefix, hash, err := auth.GenerateAPIKey()
			if err != nil {
				t.Fatalf("GenerateAPIKey() error = %v", err)
			}
			if !strings.HasPrefix(plaintext, "hu_") {
				t.Errorf("plaintext = %q, want hu_ prefix", plaintext)
			}
			if len(plaintext) != 67 || !isLowerHex(strings.TrimPrefix(plaintext, "hu_")) {
				t.Errorf("plaintext = %q (len %d), want hu_ + 64 lowercase hex chars", plaintext, len(plaintext))
			}
			if prefix != plaintext[:11] || len(prefix) != 11 {
				t.Errorf("prefix = %q (len %d), want %q (len 11)", prefix, len(prefix), plaintext[:11])
			}
			if wantHash := auth.HashAPIKey(plaintext); hash != wantHash {
				t.Errorf("hash = %q, want %q", hash, wantHash)
			}
			if _, dup := seen[plaintext]; dup {
				t.Fatalf("GenerateAPIKey() produced duplicate key %q", plaintext)
			}
			seen[plaintext] = struct{}{}
		}
	})
}
