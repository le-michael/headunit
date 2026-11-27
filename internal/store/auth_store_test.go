package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUserCRUD(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	// Empty ListUsers returns a non-nil empty slice.
	emptyUsers, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers() on empty store error: %v", err)
	}
	if emptyUsers == nil || len(emptyUsers) != 0 {
		t.Fatalf("ListUsers() on empty store = %#v, want non-nil empty slice", emptyUsers)
	}

	// Missing user lookup returns ErrNotFound.
	if _, err := s.GetUserByUsername(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetUserByUsername(missing) err = %v, want ErrNotFound", err)
	}

	// Validation rejections on CreateUser (including ensuring PasswordCiphertext is never echoed).
	secretBytes := []byte("super-secret-ciphertext-payload")
	invalidUsers := []struct {
		name string
		user User
	}{
		{"empty username", User{Username: "", PasswordCiphertext: secretBytes}},
		{"whitespace only username", User{Username: "   ", PasswordCiphertext: secretBytes}},
		{"leading space", User{Username: " alice", PasswordCiphertext: secretBytes}},
		{"trailing space", User{Username: "alice ", PasswordCiphertext: secretBytes}},
		{"nul control char", User{Username: "al\x00ice", PasswordCiphertext: secretBytes}},
		{"newline control char", User{Username: "al\nice", PasswordCiphertext: secretBytes}},
		{"tab control char", User{Username: "al\tice", PasswordCiphertext: secretBytes}},
		{"del control char", User{Username: "al\x7fice", PasswordCiphertext: secretBytes}},
		{"nil ciphertext", User{Username: "alice", PasswordCiphertext: nil}},
		{"empty ciphertext", User{Username: "alice", PasswordCiphertext: []byte{}}},
	}
	for _, tc := range invalidUsers {
		t.Run("create_invalid_"+tc.name, func(t *testing.T) {
			_, err := s.CreateUser(ctx, tc.user)
			if err == nil {
				t.Fatalf("CreateUser(%+v) = nil error, want error", tc.user)
			}
			if strings.Contains(err.Error(), string(secretBytes)) {
				t.Errorf("CreateUser error leaked PasswordCiphertext: %v", err)
			}
		})
	}

	// Empty username and ciphertext rejections on mutation methods.
	if err := s.UpdateUserPassword(ctx, "", secretBytes); err == nil || strings.Contains(err.Error(), string(secretBytes)) {
		t.Errorf("UpdateUserPassword(empty username) err = %v, want non-nil error without ciphertext", err)
	}
	if err := s.UpdateUserPassword(ctx, "alice", nil); err == nil {
		t.Errorf("UpdateUserPassword(nil ciphertext) = nil, want error")
	}
	if err := s.UpdateUserPassword(ctx, "alice", []byte{}); err == nil {
		t.Errorf("UpdateUserPassword(empty ciphertext) = nil, want error")
	}
	if err := s.SetUserAdmin(ctx, "", true); err == nil {
		t.Errorf("SetUserAdmin(empty username) = nil, want error")
	}
	if err := s.SetUserDisabled(ctx, "", true); err == nil {
		t.Errorf("SetUserDisabled(empty username) = nil, want error")
	}
	if err := s.DeleteUser(ctx, ""); err == nil {
		t.Errorf("DeleteUser(empty username) = nil, want error")
	}

	// CreateUser with default ID/timestamps and defensive ciphertext cloning.
	callerCipher := []byte("alice-cipher-v1")
	beforeCreate := time.Now().UTC().Truncate(time.Second)
	aliceCreated, err := s.CreateUser(ctx, User{
		Username:           "Alice",
		PasswordCiphertext: callerCipher,
		IsAdmin:            true,
	})
	afterCreate := time.Now().UTC().Add(time.Second)
	if err != nil {
		t.Fatalf("CreateUser(Alice) unexpected error: %v", err)
	}
	if !strings.HasPrefix(aliceCreated.ID, "usr_") || len(aliceCreated.ID) != 36 {
		t.Fatalf("CreateUser(Alice).ID = %q, want usr_ + 32 hex chars", aliceCreated.ID)
	}
	if aliceCreated.CreatedAt.Before(beforeCreate) || aliceCreated.CreatedAt.After(afterCreate) {
		t.Errorf("CreateUser(Alice).CreatedAt = %v, want within [%v, %v]", aliceCreated.CreatedAt, beforeCreate, afterCreate)
	}
	if !aliceCreated.UpdatedAt.Equal(aliceCreated.CreatedAt) {
		t.Errorf("CreateUser(Alice).UpdatedAt = %v, want equal to CreatedAt %v", aliceCreated.UpdatedAt, aliceCreated.CreatedAt)
	}

	// Mutating caller slice and returned slice must not affect persisted DB row.
	callerCipher[0] = 'X'
	aliceCreated.PasswordCiphertext[0] = 'Y'

	gotAlice, err := s.GetUserByUsername(ctx, "aLiCe")
	if err != nil {
		t.Fatalf("GetUserByUsername(aLiCe) error: %v", err)
	}
	if gotAlice.Username != "Alice" {
		t.Errorf("GetUserByUsername(aLiCe).Username = %q, want %q", gotAlice.Username, "Alice")
	}
	if !bytes.Equal(gotAlice.PasswordCiphertext, []byte("alice-cipher-v1")) {
		t.Errorf("GetUserByUsername(aLiCe).PasswordCiphertext = %q, want %q", gotAlice.PasswordCiphertext, "alice-cipher-v1")
	}
	gotAlice.PasswordCiphertext[0] = 'Z'
	gotAliceAgain, err := s.GetUserByUsername(ctx, "ALICE")
	if err != nil {
		t.Fatalf("GetUserByUsername(ALICE) error: %v", err)
	}
	if !bytes.Equal(gotAliceAgain.PasswordCiphertext, []byte("alice-cipher-v1")) {
		t.Errorf("GetUserByUsername(ALICE).PasswordCiphertext mutated to %q", gotAliceAgain.PasswordCiphertext)
	}

	// Duplicate username (exact & case-insensitive) and duplicate ID return ErrAlreadyExists without leaking ciphertext.
	for _, dup := range []struct {
		name string
		user User
	}{
		{"exact username", User{Username: "Alice", PasswordCiphertext: secretBytes}},
		{"case-insensitive username", User{Username: "ALICE", PasswordCiphertext: secretBytes}},
		{"duplicate id", User{ID: aliceCreated.ID, Username: "other", PasswordCiphertext: secretBytes}},
	} {
		t.Run("duplicate_"+dup.name, func(t *testing.T) {
			_, err := s.CreateUser(ctx, dup.user)
			if !errors.Is(err, ErrAlreadyExists) {
				t.Fatalf("CreateUser(%s) err = %v, want ErrAlreadyExists", dup.name, err)
			}
			if strings.Contains(err.Error(), string(secretBytes)) {
				t.Errorf("CreateUser(%s) error leaked PasswordCiphertext: %v", dup.name, err)
			}
		})
	}

	// Create additional users with explicit historical timestamps and non-UTC timezone.
	est := time.FixedZone("EST", -5*3600)
	histCreated := time.Date(2020, 1, 1, 5, 0, 0, 987654321, est) // 2020-01-01T10:00:00Z
	histUpdated := time.Date(2020, 1, 2, 5, 0, 0, 123456789, est) // 2020-01-02T10:00:00Z

	bob, err := s.CreateUser(ctx, User{
		ID:                 "usr_bob_01",
		Username:           "bob",
		PasswordCiphertext: []byte("bob-cipher"),
		IsAdmin:            false,
		Disabled:           true,
		CreatedAt:          histCreated,
		UpdatedAt:          histUpdated,
	})
	if err != nil {
		t.Fatalf("CreateUser(bob) error: %v", err)
	}
	if !bob.CreatedAt.Equal(histCreated.UTC().Truncate(time.Second)) {
		t.Errorf("bob.CreatedAt = %v, want %v", bob.CreatedAt, histCreated.UTC().Truncate(time.Second))
	}
	if !bob.UpdatedAt.Equal(histUpdated.UTC().Truncate(time.Second)) {
		t.Errorf("bob.UpdatedAt = %v, want %v", bob.UpdatedAt, histUpdated.UTC().Truncate(time.Second))
	}

	aaron, err := s.CreateUser(ctx, User{
		ID:                 "usr_aaron_01",
		Username:           "aaron",
		PasswordCiphertext: []byte("aaron-cipher"),
		CreatedAt:          histCreated,
	})
	if err != nil {
		t.Fatalf("CreateUser(aaron) error: %v", err)
	}

	// ListUsers returns deterministic case-insensitive order: aaron, Alice, bob.
	users, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers() error: %v", err)
	}
	wantOrder := []string{aaron.ID, aliceCreated.ID, bob.ID}
	gotOrder := make([]string, len(users))
	for i, u := range users {
		gotOrder[i] = u.ID
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Errorf("ListUsers() order = %v, want %v", gotOrder, wantOrder)
	}

	// UpdateUserPassword advances updated_at past historical timestamp and updates ciphertext case-insensitively.
	newBobCipher := []byte("bob-cipher-v2-secret")
	if err := s.UpdateUserPassword(ctx, "BOB", newBobCipher); err != nil {
		t.Fatalf("UpdateUserPassword(BOB) error: %v", err)
	}
	gotBob, err := s.GetUserByUsername(ctx, "bob")
	if err != nil {
		t.Fatalf("GetUserByUsername(bob) error: %v", err)
	}
	if !bytes.Equal(gotBob.PasswordCiphertext, newBobCipher) {
		t.Errorf("bob.PasswordCiphertext = %q, want %q", gotBob.PasswordCiphertext, newBobCipher)
	}
	if !gotBob.CreatedAt.Equal(bob.CreatedAt) {
		t.Errorf("bob.CreatedAt changed from %v to %v", bob.CreatedAt, gotBob.CreatedAt)
	}
	if !gotBob.UpdatedAt.After(bob.UpdatedAt) {
		t.Errorf("bob.UpdatedAt = %v, want after historical %v", gotBob.UpdatedAt, bob.UpdatedAt)
	}

	// Missing user errors for mutation methods (and verify ciphertext not leaked).
	if err := s.UpdateUserPassword(ctx, "missing", secretBytes); !errors.Is(err, ErrNotFound) || strings.Contains(err.Error(), string(secretBytes)) {
		t.Errorf("UpdateUserPassword(missing) err = %v, want ErrNotFound without ciphertext", err)
	}
	if err := s.SetUserAdmin(ctx, "missing", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetUserAdmin(missing) err = %v, want ErrNotFound", err)
	}
	if err := s.SetUserDisabled(ctx, "missing", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetUserDisabled(missing) err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteUser(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteUser(missing) err = %v, want ErrNotFound", err)
	}

	// SetUserAdmin: backdate updated_at deterministically, toggle true/false and idempotent repeat.
	pastTS := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.writeDB.ExecContext(ctx, `UPDATE users SET updated_at = ? WHERE id = ?`, formatCreatedAt(pastTS), bob.ID); err != nil {
		t.Fatalf("backdate bob updated_at: %v", err)
	}
	if err := s.SetUserAdmin(ctx, "BOB", true); err != nil {
		t.Fatalf("SetUserAdmin(BOB, true) error: %v", err)
	}
	if err := s.SetUserAdmin(ctx, "bob", true); err != nil {
		t.Fatalf("SetUserAdmin(bob, true) idempotent repeat error: %v", err)
	}
	gotBob, err = s.GetUserByUsername(ctx, "bob")
	if err != nil {
		t.Fatalf("GetUserByUsername(bob) error: %v", err)
	}
	if !gotBob.IsAdmin {
		t.Errorf("bob.IsAdmin = false, want true")
	}
	if !gotBob.UpdatedAt.After(pastTS) {
		t.Errorf("bob.UpdatedAt = %v after SetUserAdmin, want after %v", gotBob.UpdatedAt, pastTS)
	}

	if err := s.SetUserAdmin(ctx, "bob", false); err != nil {
		t.Fatalf("SetUserAdmin(bob, false) error: %v", err)
	}
	gotBob, err = s.GetUserByUsername(ctx, "bob")
	if err != nil {
		t.Fatalf("GetUserByUsername(bob) error: %v", err)
	}
	if gotBob.IsAdmin {
		t.Errorf("bob.IsAdmin = true, want false")
	}

	// SetUserDisabled: backdate updated_at deterministically, toggle false/true and idempotent repeat.
	if _, err := s.writeDB.ExecContext(ctx, `UPDATE users SET updated_at = ? WHERE id = ?`, formatCreatedAt(pastTS), bob.ID); err != nil {
		t.Fatalf("backdate bob updated_at: %v", err)
	}
	if err := s.SetUserDisabled(ctx, "BOB", false); err != nil {
		t.Fatalf("SetUserDisabled(BOB, false) error: %v", err)
	}
	if err := s.SetUserDisabled(ctx, "bob", false); err != nil {
		t.Fatalf("SetUserDisabled(bob, false) idempotent repeat error: %v", err)
	}
	gotBob, err = s.GetUserByUsername(ctx, "bob")
	if err != nil {
		t.Fatalf("GetUserByUsername(bob) error: %v", err)
	}
	if gotBob.Disabled {
		t.Errorf("bob.Disabled = true, want false")
	}
	if !gotBob.UpdatedAt.After(pastTS) {
		t.Errorf("bob.UpdatedAt = %v after SetUserDisabled, want after %v", gotBob.UpdatedAt, pastTS)
	}

	// DeleteUser removes matching user case-insensitively.
	if err := s.DeleteUser(ctx, "BOB"); err != nil {
		t.Fatalf("DeleteUser(BOB) error: %v", err)
	}
	if _, err := s.GetUserByUsername(ctx, "bob"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetUserByUsername(bob) after DeleteUser err = %v, want ErrNotFound", err)
	}
}

func TestAPIKeyCRUDAndCascade(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	// Empty ListAPIKeys returns non-nil empty slices.
	allEmpty, err := s.ListAPIKeys(ctx, "")
	if err != nil {
		t.Fatalf("ListAPIKeys(\"\") on empty store error: %v", err)
	}
	if allEmpty == nil || len(allEmpty) != 0 {
		t.Fatalf("ListAPIKeys(\"\") on empty store = %#v, want non-nil empty slice", allEmpty)
	}
	userEmpty, err := s.ListAPIKeys(ctx, "alice")
	if err != nil {
		t.Fatalf("ListAPIKeys(\"alice\") on empty store error: %v", err)
	}
	if userEmpty == nil || len(userEmpty) != 0 {
		t.Fatalf("ListAPIKeys(\"alice\") on empty store = %#v, want non-nil empty slice", userEmpty)
	}

	alice, err := s.CreateUser(ctx, User{
		Username:           "Alice",
		PasswordCiphertext: []byte("alice-cipher"),
		IsAdmin:            true,
	})
	if err != nil {
		t.Fatalf("CreateUser(Alice) error: %v", err)
	}
	bob, err := s.CreateUser(ctx, User{
		Username:           "bob",
		PasswordCiphertext: []byte("bob-cipher"),
		Disabled:           true,
	})
	if err != nil {
		t.Fatalf("CreateUser(bob) error: %v", err)
	}

	// Validation rejections on CreateAPIKey and RevokeAPIKey.
	const secretHash = "deadbeef0123456789abcdef0123456789abcdef0123456789abcdef01234567"
	invalidKeys := []struct {
		name string
		key  APIKey
	}{
		{"empty user_id", APIKey{UserID: "", KeyPrefix: "hu_abc", KeyHash: secretHash}},
		{"empty key_prefix", APIKey{UserID: alice.ID, KeyPrefix: "", KeyHash: secretHash}},
		{"empty key_hash", APIKey{UserID: alice.ID, KeyPrefix: "hu_abc", KeyHash: ""}},
		{"non-zero revoked_at", APIKey{UserID: alice.ID, KeyPrefix: "hu_abc", KeyHash: secretHash, RevokedAt: time.Now().UTC()}},
	}
	for _, tc := range invalidKeys {
		t.Run("create_invalid_"+tc.name, func(t *testing.T) {
			_, err := s.CreateAPIKey(ctx, tc.key)
			if err == nil {
				t.Fatalf("CreateAPIKey(%s) = nil error, want error", tc.name)
			}
			if strings.Contains(err.Error(), secretHash) {
				t.Errorf("CreateAPIKey(%s) error leaked KeyHash: %v", tc.name, err)
			}
		})
	}
	if err := s.RevokeAPIKey(ctx, ""); err == nil {
		t.Errorf("RevokeAPIKey(\"\") = nil, want error")
	}

	// Nonexistent UserID -> ErrNotFound without leaking KeyHash.
	if _, err := s.CreateAPIKey(ctx, APIKey{
		UserID:    "usr_nonexistent",
		KeyPrefix: "hu_missing",
		KeyHash:   secretHash,
	}); !errors.Is(err, ErrNotFound) || strings.Contains(err.Error(), secretHash) {
		t.Errorf("CreateAPIKey(missing user) err = %v, want ErrNotFound without KeyHash", err)
	}

	// Create active keys for Alice and Bob.
	t1 := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2025, 1, 11, 12, 0, 0, 0, time.UTC)

	aliceKey1, err := s.CreateAPIKey(ctx, APIKey{
		UserID:    alice.ID,
		Username:  "ignored-by-create",
		Name:      "Phone",
		KeyPrefix: "hu_a1",
		KeyHash:   "hash_alice_1",
		CreatedAt: t1,
	})
	if err != nil {
		t.Fatalf("CreateAPIKey(alice 1) error: %v", err)
	}
	if !strings.HasPrefix(aliceKey1.ID, "key_") || len(aliceKey1.ID) != 36 {
		t.Fatalf("CreateAPIKey(alice 1).ID = %q, want key_ + 32 hex chars", aliceKey1.ID)
	}
	if aliceKey1.Username != "" {
		t.Errorf("CreateAPIKey().Username = %q, want empty", aliceKey1.Username)
	}
	if !aliceKey1.RevokedAt.IsZero() {
		t.Errorf("CreateAPIKey().RevokedAt = %v, want zero", aliceKey1.RevokedAt)
	}

	aliceKey2, err := s.CreateAPIKey(ctx, APIKey{
		ID:        "key_alice_2",
		UserID:    alice.ID,
		Name:      "",
		KeyPrefix: "hu_a2",
		KeyHash:   "hash_alice_2",
		CreatedAt: t2,
	})
	if err != nil {
		t.Fatalf("CreateAPIKey(alice 2) error: %v", err)
	}

	bobKey, err := s.CreateAPIKey(ctx, APIKey{
		ID:        "key_bob_1",
		UserID:    bob.ID,
		Name:      "Laptop",
		KeyPrefix: "hu_b1",
		KeyHash:   "hash_bob_1",
	})
	if err != nil {
		t.Fatalf("CreateAPIKey(bob) error: %v", err)
	}

	// Duplicate KeyHash and duplicate ID return ErrAlreadyExists without leaking KeyHash.
	for _, dup := range []struct {
		name string
		key  APIKey
	}{
		{"duplicate key_hash", APIKey{UserID: bob.ID, KeyPrefix: "hu_dup", KeyHash: "hash_alice_1"}},
		{"duplicate id", APIKey{ID: aliceKey1.ID, UserID: bob.ID, KeyPrefix: "hu_dup", KeyHash: "hash_unique_dup"}},
	} {
		t.Run(dup.name, func(t *testing.T) {
			_, err := s.CreateAPIKey(ctx, dup.key)
			if !errors.Is(err, ErrAlreadyExists) {
				t.Fatalf("CreateAPIKey(%s) err = %v, want ErrAlreadyExists", dup.name, err)
			}
			if strings.Contains(err.Error(), dup.key.KeyHash) {
				t.Errorf("CreateAPIKey(%s) error leaked KeyHash: %v", dup.name, err)
			}
		})
	}

	// LookupAPIKeyByHash for unknown hash returns ErrNotFound without leaking hash.
	const unknownHash = "unknown_secret_hash_value"
	if _, err := s.LookupAPIKeyByHash(ctx, unknownHash); !errors.Is(err, ErrNotFound) || strings.Contains(err.Error(), unknownHash) {
		t.Errorf("LookupAPIKeyByHash(unknown) err = %v, want ErrNotFound without hash", err)
	}

	// LookupAPIKeyByHash for active admin user key and disabled user key.
	gotAliceLookup, err := s.LookupAPIKeyByHash(ctx, aliceKey1.KeyHash)
	if err != nil {
		t.Fatalf("LookupAPIKeyByHash(aliceKey1) error: %v", err)
	}
	wantAliceLookup := APIKeyLookup{
		Key: APIKey{
			ID:        aliceKey1.ID,
			UserID:    alice.ID,
			Username:  "Alice",
			Name:      "Phone",
			KeyPrefix: "hu_a1",
			KeyHash:   "hash_alice_1",
			CreatedAt: t1,
		},
		UserIsAdmin:  true,
		UserDisabled: false,
	}
	if !reflect.DeepEqual(*gotAliceLookup, wantAliceLookup) {
		t.Errorf("LookupAPIKeyByHash(aliceKey1) =\n%+v\nwant:\n%+v", *gotAliceLookup, wantAliceLookup)
	}

	gotBobLookup, err := s.LookupAPIKeyByHash(ctx, bobKey.KeyHash)
	if err != nil {
		t.Fatalf("LookupAPIKeyByHash(bobKey) error: %v", err)
	}
	if !gotBobLookup.UserDisabled || gotBobLookup.UserIsAdmin || gotBobLookup.Key.Username != "bob" {
		t.Errorf("LookupAPIKeyByHash(bobKey) = %+v, want UserDisabled=true, UserIsAdmin=false, Username=bob", *gotBobLookup)
	}

	// ListAPIKeys("") lists all keys ordered by lower(username), created_at, id.
	allKeys, err := s.ListAPIKeys(ctx, "")
	if err != nil {
		t.Fatalf("ListAPIKeys(\"\") error: %v", err)
	}
	wantAllIDs := []string{aliceKey1.ID, aliceKey2.ID, bobKey.ID}
	gotAllIDs := make([]string, len(allKeys))
	for i, k := range allKeys {
		gotAllIDs[i] = k.ID
	}
	if !reflect.DeepEqual(gotAllIDs, wantAllIDs) {
		t.Errorf("ListAPIKeys(\"\") IDs = %v, want %v", gotAllIDs, wantAllIDs)
	}

	// ListAPIKeys("ALICE") filters case-insensitively.
	aliceKeys, err := s.ListAPIKeys(ctx, "ALICE")
	if err != nil {
		t.Fatalf("ListAPIKeys(ALICE) error: %v", err)
	}
	if len(aliceKeys) != 2 || aliceKeys[0].ID != aliceKey1.ID || aliceKeys[1].ID != aliceKey2.ID {
		t.Errorf("ListAPIKeys(ALICE) = %+v, want [%s, %s]", aliceKeys, aliceKey1.ID, aliceKey2.ID)
	}

	// RevokeAPIKey sets RevokedAt; backdated repeat call is idempotent and preserves historical timestamp.
	if err := s.RevokeAPIKey(ctx, "key_nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RevokeAPIKey(missing) err = %v, want ErrNotFound", err)
	}
	beforeRevoke := time.Now().UTC().Truncate(time.Second)
	if err := s.RevokeAPIKey(ctx, aliceKey1.ID); err != nil {
		t.Fatalf("RevokeAPIKey(aliceKey1) error: %v", err)
	}
	afterRevoke := time.Now().UTC().Add(time.Second)

	revokedLookup, err := s.LookupAPIKeyByHash(ctx, aliceKey1.KeyHash)
	if err != nil {
		t.Fatalf("LookupAPIKeyByHash(revoked) error: %v", err)
	}
	if revokedLookup.Key.RevokedAt.Before(beforeRevoke) || revokedLookup.Key.RevokedAt.After(afterRevoke) {
		t.Errorf("RevokedAt = %v, want within [%v, %v]", revokedLookup.Key.RevokedAt, beforeRevoke, afterRevoke)
	}

	historicalRevokedAt := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := s.writeDB.ExecContext(ctx, `UPDATE api_keys SET revoked_at = ? WHERE id = ?`, formatCreatedAt(historicalRevokedAt), aliceKey1.ID); err != nil {
		t.Fatalf("backdate revoked_at: %v", err)
	}
	if err := s.RevokeAPIKey(ctx, aliceKey1.ID); err != nil {
		t.Fatalf("RevokeAPIKey(aliceKey1) second call error: %v", err)
	}
	revokedAgain, err := s.LookupAPIKeyByHash(ctx, aliceKey1.KeyHash)
	if err != nil {
		t.Fatalf("LookupAPIKeyByHash(revoked again) error: %v", err)
	}
	if !revokedAgain.Key.RevokedAt.Equal(historicalRevokedAt) {
		t.Errorf("second RevokeAPIKey overwrote RevokedAt to %v, want preserved %v", revokedAgain.Key.RevokedAt, historicalRevokedAt)
	}

	// DeleteUser cascades deletion to all of Alice's API keys while leaving Bob's untouched.
	if err := s.DeleteUser(ctx, "alice"); err != nil {
		t.Fatalf("DeleteUser(alice) error: %v", err)
	}
	if _, err := s.LookupAPIKeyByHash(ctx, aliceKey1.KeyHash); !errors.Is(err, ErrNotFound) {
		t.Errorf("LookupAPIKeyByHash(aliceKey1) after DeleteUser err = %v, want ErrNotFound", err)
	}
	if _, err := s.LookupAPIKeyByHash(ctx, aliceKey2.KeyHash); !errors.Is(err, ErrNotFound) {
		t.Errorf("LookupAPIKeyByHash(aliceKey2) after DeleteUser err = %v, want ErrNotFound", err)
	}
	remainingKeys, err := s.ListAPIKeys(ctx, "")
	if err != nil {
		t.Fatalf("ListAPIKeys(\"\") after DeleteUser error: %v", err)
	}
	if len(remainingKeys) != 1 || remainingKeys[0].ID != bobKey.ID {
		t.Errorf("ListAPIKeys(\"\") after DeleteUser = %+v, want only bobKey %q", remainingKeys, bobKey.ID)
	}
}

func TestAuthStore_CorruptTimestamps(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name      string
		corruptFn func(s *Store) error
		checkFn   func(s *Store) error
	}{
		{
			name: "users.created_at via GetUserByUsername",
			corruptFn: func(s *Store) error {
				_, err := s.writeDB.ExecContext(ctx, `UPDATE users SET created_at = 'not-a-timestamp' WHERE username = 'alice'`)
				return err
			},
			checkFn: func(s *Store) error {
				_, err := s.GetUserByUsername(ctx, "alice")
				return err
			},
		},
		{
			name: "users.updated_at via ListUsers",
			corruptFn: func(s *Store) error {
				_, err := s.writeDB.ExecContext(ctx, `UPDATE users SET updated_at = 'bad-updated-at' WHERE username = 'alice'`)
				return err
			},
			checkFn: func(s *Store) error {
				_, err := s.ListUsers(ctx)
				return err
			},
		},
		{
			name: "api_keys.created_at via LookupAPIKeyByHash",
			corruptFn: func(s *Store) error {
				_, err := s.writeDB.ExecContext(ctx, `UPDATE api_keys SET created_at = 'bad-key-created-at' WHERE key_hash = 'hash_corrupt'`)
				return err
			},
			checkFn: func(s *Store) error {
				_, err := s.LookupAPIKeyByHash(ctx, "hash_corrupt")
				return err
			},
		},
		{
			name: "api_keys.revoked_at via ListAPIKeys",
			corruptFn: func(s *Store) error {
				_, err := s.writeDB.ExecContext(ctx, `UPDATE api_keys SET revoked_at = 'bad-key-revoked-at' WHERE key_hash = 'hash_corrupt'`)
				return err
			},
			checkFn: func(s *Store) error {
				_, err := s.ListAPIKeys(ctx, "alice")
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			u, err := s.CreateUser(ctx, User{
				Username:           "alice",
				PasswordCiphertext: []byte("cipher"),
			})
			if err != nil {
				t.Fatalf("CreateUser() error: %v", err)
			}
			if _, err := s.CreateAPIKey(ctx, APIKey{
				UserID:    u.ID,
				KeyPrefix: "hu_c",
				KeyHash:   "hash_corrupt",
			}); err != nil {
				t.Fatalf("CreateAPIKey() error: %v", err)
			}
			if err := tc.corruptFn(s); err != nil {
				t.Fatalf("corrupt timestamp query error: %v", err)
			}
			err = tc.checkFn(s)
			if err == nil {
				t.Fatalf("expected parse error, got nil")
			}
			if errors.Is(err, ErrNotFound) {
				t.Errorf("got ErrNotFound (%v), want non-ErrNotFound timestamp parse error", err)
			}
		})
	}
}

func TestAuthStore_WALIsolationAndRace(t *testing.T) {
	ctx := context.Background()

	t.Run("held_write_tx_wal_snapshot", func(t *testing.T) {
		s := openTestStore(t)

		seededUser, err := s.CreateUser(ctx, User{
			ID:                 "usr_seeded",
			Username:           "seeded",
			PasswordCiphertext: []byte("seeded-cipher"),
			IsAdmin:            true,
		})
		if err != nil {
			t.Fatalf("CreateUser(seeded) error: %v", err)
		}
		seededKey, err := s.CreateAPIKey(ctx, APIKey{
			ID:        "key_seeded",
			UserID:    seededUser.ID,
			Name:      "Seeded",
			KeyPrefix: "hu_seed",
			KeyHash:   "hash_seeded",
		})
		if err != nil {
			t.Fatalf("CreateAPIKey(seeded) error: %v", err)
		}

		// Hold the sole writeDB connection open in an uncommitted immediate write transaction.
		tx, err := s.writeDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("writeDB.BeginTx() error: %v", err)
		}
		defer func() { _ = tx.Rollback() }()

		nowRaw := formatCreatedAt(time.Now().UTC())
		if _, err := tx.ExecContext(ctx, insertUserSQL,
			"usr_uncommitted", "uncommitted", []byte("uncommitted-cipher"), 0, 0, nowRaw, nowRaw,
		); err != nil {
			t.Fatalf("tx insert uncommitted user error: %v", err)
		}
		if _, err := tx.ExecContext(ctx, insertAPIKeySQL,
			"key_uncommitted", "usr_uncommitted", "Uncommitted", "hu_unc", "hash_uncommitted", nowRaw,
		); err != nil {
			t.Fatalf("tx insert uncommitted api key error: %v", err)
		}

		readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()

		gotUser, err := s.GetUserByUsername(readCtx, "seeded")
		if err != nil {
			t.Fatalf("GetUserByUsername(seeded) while write tx held error: %v", err)
		}
		if gotUser.ID != seededUser.ID {
			t.Errorf("GetUserByUsername(seeded).ID = %q, want %q", gotUser.ID, seededUser.ID)
		}
		if _, err := s.GetUserByUsername(readCtx, "uncommitted"); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetUserByUsername(uncommitted) = %v, want ErrNotFound", err)
		}

		users, err := s.ListUsers(readCtx)
		if err != nil {
			t.Fatalf("ListUsers() while write tx held error: %v", err)
		}
		if len(users) != 1 || users[0].ID != seededUser.ID {
			t.Errorf("ListUsers() while write tx held = %+v, want only seeded user", users)
		}

		lookup, err := s.LookupAPIKeyByHash(readCtx, seededKey.KeyHash)
		if err != nil {
			t.Fatalf("LookupAPIKeyByHash(seeded) while write tx held error: %v", err)
		}
		if lookup.Key.ID != seededKey.ID {
			t.Errorf("LookupAPIKeyByHash(seeded).Key.ID = %q, want %q", lookup.Key.ID, seededKey.ID)
		}
		if _, err := s.LookupAPIKeyByHash(readCtx, "hash_uncommitted"); !errors.Is(err, ErrNotFound) {
			t.Errorf("LookupAPIKeyByHash(uncommitted) = %v, want ErrNotFound", err)
		}

		keys, err := s.ListAPIKeys(readCtx, "")
		if err != nil {
			t.Fatalf("ListAPIKeys() while write tx held error: %v", err)
		}
		if len(keys) != 1 || keys[0].ID != seededKey.ID {
			t.Errorf("ListAPIKeys() while write tx held = %+v, want only seeded key", keys)
		}

		if err := tx.Commit(); err != nil {
			t.Fatalf("tx.Commit() error: %v", err)
		}

		if _, err := s.GetUserByUsername(ctx, "uncommitted"); err != nil {
			t.Errorf("GetUserByUsername(uncommitted) after commit error: %v", err)
		}
		if _, err := s.LookupAPIKeyByHash(ctx, "hash_uncommitted"); err != nil {
			t.Errorf("LookupAPIKeyByHash(uncommitted) after commit error: %v", err)
		}
		usersAfter, err := s.ListUsers(ctx)
		if err != nil {
			t.Fatalf("ListUsers() after commit error: %v", err)
		}
		if len(usersAfter) != 2 {
			t.Errorf("ListUsers() after commit len = %d, want 2", len(usersAfter))
		}
		keysAfter, err := s.ListAPIKeys(ctx, "")
		if err != nil {
			t.Fatalf("ListAPIKeys() after commit error: %v", err)
		}
		if len(keysAfter) != 2 {
			t.Errorf("ListAPIKeys() after commit len = %d, want 2", len(keysAfter))
		}
	})

	t.Run("barrier_synchronized_race_stress", func(t *testing.T) {
		s := openTestStore(t)

		stableUser, err := s.CreateUser(ctx, User{
			ID:                 "usr_stable",
			Username:           "stable",
			PasswordCiphertext: []byte("stable-cipher"),
			IsAdmin:            true,
		})
		if err != nil {
			t.Fatalf("CreateUser(stable) error: %v", err)
		}
		stableKey, err := s.CreateAPIKey(ctx, APIKey{
			ID:        "key_stable",
			UserID:    stableUser.ID,
			Name:      "Stable",
			KeyPrefix: "hu_stab",
			KeyHash:   "hash_stable",
		})
		if err != nil {
			t.Fatalf("CreateAPIKey(stable) error: %v", err)
		}

		const (
			numReaders = 8
			numWriters = 4
			iterations = 25
		)
		start := make(chan struct{})
		errCh := make(chan error, (numReaders+numWriters)*iterations)

		var wg sync.WaitGroup
		for r := range numReaders {
			wg.Add(1)
			go func(readerID int) {
				defer wg.Done()
				<-start
				for i := range iterations {
					if _, err := s.GetUserByUsername(ctx, stableUser.Username); err != nil {
						errCh <- fmt.Errorf("reader %d iter %d GetUserByUsername: %w", readerID, i, err)
						return
					}
					if _, err := s.ListUsers(ctx); err != nil {
						errCh <- fmt.Errorf("reader %d iter %d ListUsers: %w", readerID, i, err)
						return
					}
					if _, err := s.LookupAPIKeyByHash(ctx, stableKey.KeyHash); err != nil {
						errCh <- fmt.Errorf("reader %d iter %d LookupAPIKeyByHash: %w", readerID, i, err)
						return
					}
					if _, err := s.ListAPIKeys(ctx, ""); err != nil {
						errCh <- fmt.Errorf("reader %d iter %d ListAPIKeys(\"\"): %w", readerID, i, err)
						return
					}
					if _, err := s.ListAPIKeys(ctx, stableUser.Username); err != nil {
						errCh <- fmt.Errorf("reader %d iter %d ListAPIKeys(stable): %w", readerID, i, err)
						return
					}
				}
			}(r)
		}

		for w := range numWriters {
			wg.Add(1)
			go func(writerID int) {
				defer wg.Done()
				<-start
				for i := range iterations {
					username := fmt.Sprintf("worker_%d_%d", writerID, i)
					u, err := s.CreateUser(ctx, User{
						Username:           username,
						PasswordCiphertext: []byte(fmt.Sprintf("cipher-%d-%d", writerID, i)),
					})
					if err != nil {
						errCh <- fmt.Errorf("writer %d iter %d CreateUser: %w", writerID, i, err)
						return
					}
					if err := s.UpdateUserPassword(ctx, username, []byte("updated-cipher")); err != nil {
						errCh <- fmt.Errorf("writer %d iter %d UpdateUserPassword: %w", writerID, i, err)
						return
					}
					if err := s.SetUserAdmin(ctx, username, i%2 == 0); err != nil {
						errCh <- fmt.Errorf("writer %d iter %d SetUserAdmin: %w", writerID, i, err)
						return
					}
					if err := s.SetUserDisabled(ctx, username, i%3 == 0); err != nil {
						errCh <- fmt.Errorf("writer %d iter %d SetUserDisabled: %w", writerID, i, err)
						return
					}
					k, err := s.CreateAPIKey(ctx, APIKey{
						UserID:    u.ID,
						Name:      "Worker Key",
						KeyPrefix: "hu_wrk",
						KeyHash:   fmt.Sprintf("hash_%d_%d", writerID, i),
					})
					if err != nil {
						errCh <- fmt.Errorf("writer %d iter %d CreateAPIKey: %w", writerID, i, err)
						return
					}
					if err := s.RevokeAPIKey(ctx, k.ID); err != nil {
						errCh <- fmt.Errorf("writer %d iter %d RevokeAPIKey: %w", writerID, i, err)
						return
					}
					if err := s.DeleteUser(ctx, username); err != nil {
						errCh <- fmt.Errorf("writer %d iter %d DeleteUser: %w", writerID, i, err)
						return
					}
				}
			}(w)
		}

		close(start)
		wg.Wait()
		close(errCh)

		for err := range errCh {
			t.Error(err)
		}
	})
}
