package auth

import (
	"crypto/sha256"
	"strings"
	"testing"

	"golang.org/x/crypto/pbkdf2"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	t.Setenv("AGENT_SERVER_PASSWORD_ITERATIONS", "1000")

	stored, err := HashPassword("correct horse", "")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if parts := strings.Split(stored, "$"); len(parts) != 4 || parts[1] != "1000" {
		t.Fatalf("hash does not encode its cost: %q", stored)
	}
	if !VerifyPassword("correct horse", stored) {
		t.Fatal("the stored hash does not verify")
	}
	if VerifyPassword("wrong horse", stored) {
		t.Fatal("a wrong password verified")
	}
	if NeedsRehash(stored) {
		t.Fatal("a hash at the current cost asked to be rehashed")
	}
	if salt := PasswordSalt(stored); salt == "" || salt != strings.Split(stored, "$")[2] {
		t.Fatalf("PasswordSalt returned %q", salt)
	}
}

func TestLegacyHashStillVerifies(t *testing.T) {
	t.Setenv("AGENT_SERVER_PASSWORD_ITERATIONS", "1000")

	// The pre-cost shape: pbkdf2_sha256$salt$digest.
	digest := pbkdf2.Key([]byte("hunter2"), []byte("legacy-salt"), 1000, sha256.Size, sha256.New)
	legacy := "pbkdf2_sha256$legacy-salt$" + b64(digest)
	if !VerifyPassword("hunter2", legacy) {
		t.Fatal("a three-part hash no longer verifies")
	}
	if !NeedsRehash(legacy) {
		t.Fatal("a three-part hash should be rewritten at login")
	}
}

func TestVerifierMatchesStoredDigest(t *testing.T) {
	t.Setenv("AGENT_SERVER_PASSWORD_ITERATIONS", "1000")

	hash, err := HashPassword("s3cret", "fixed-salt")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	verifier := b64(pbkdf2.Key([]byte("s3cret"), []byte("fixed-salt"), 1000, sha256.Size, sha256.New))
	if !VerifyPasswordVerifier(verifier, hash) {
		t.Fatal("a verifier for the stored credential did not verify")
	}
	wrapped, err := PasswordVerifierHash(verifier, "fixed-salt")
	if err != nil {
		t.Fatalf("wrap verifier: %v", err)
	}
	if !VerifyPasswordVerifier(verifier, wrapped) {
		t.Fatal("a wrapped verifier did not verify")
	}
}

func TestTokenCarriesCredentialVersion(t *testing.T) {
	t.Setenv("AA_ALLOW_DEV_SECRET", "1")

	version := CredentialVersion("pbkdf2_sha256$1000$salt$digest")
	other := CredentialVersion("pbkdf2_sha256$1000$salt$other")
	if version == "" || version == other {
		t.Fatalf("credential versions do not change with the hash: %q / %q", version, other)
	}

	token, err := CreateUserAccessTokenVersion("user-1", version, 60)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	info, err := VerifyUserAccessTokenInfo(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if info.Subject != "user-1" || info.CredentialVersion != version {
		t.Fatalf("unexpected claims: %+v", info)
	}

	plain, err := CreateUserAccessToken("user-1", 60)
	if err != nil {
		t.Fatalf("mint plain: %v", err)
	}
	if info, err := VerifyUserAccessTokenInfo(plain); err != nil || info.CredentialVersion != "" {
		t.Fatalf("a token without a credential version should verify as legacy: %+v %v", info, err)
	}
}
