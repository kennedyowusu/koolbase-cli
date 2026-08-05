package templates

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Verification decides whether source code from the network gets written into
// a developer's project. Every property below is a thing that, if wrong,
// installs unverified code silently.

// signedFixture writes a bundle, registers a fresh signing key under keyID for
// the duration of the test, and returns the file path plus a matching entry.
func signedFixture(t *testing.T, content, keyID string) (string, Entry) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}

	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}

	digest := sha256.Sum256([]byte(content))
	sig := ed25519.Sign(priv, digest[:])

	trustKey(t, keyID, hex.EncodeToString(pub))

	return path, Entry{
		ID:           "chat",
		Version:      "1.0.0",
		SHA256:       hex.EncodeToString(digest[:]),
		Signature:    hex.EncodeToString(sig),
		SigningKeyID: keyID,
	}
}

// trustKey adds a key to the compiled-in keyring for one test and removes it
// afterwards, so tests cannot leak trust into each other.
func trustKey(t *testing.T, id, pubHex string) {
	t.Helper()
	templateSigningKeys[id] = pubHex
	t.Cleanup(func() { delete(templateSigningKeys, id) })
}

func TestVerifyBundle_AcceptsSignedBundle(t *testing.T) {
	path, e := signedFixture(t, "template bundle contents", "kbtpl_test")
	if err := VerifyBundle(path, e); err != nil {
		t.Fatalf("a correctly signed bundle was rejected: %v", err)
	}
}

// The whole point: content that changed after signing must not install.
func TestVerifyBundle_RejectsTamperedContent(t *testing.T) {
	path, e := signedFixture(t, "original contents", "kbtpl_test")

	if err := os.WriteFile(path, []byte("malicious contents"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	err := VerifyBundle(path, e)
	if err == nil {
		t.Fatal("a tampered bundle passed verification")
	}
	if !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("expected an integrity failure, got: %v", err)
	}
}

// A catalog whose sha256 was swapped to match tampered content must still
// fail — that is what the signature is for. Integrity alone is only a typo
// check; authenticity is the security property.
func TestVerifyBundle_RejectsRecomputedDigestWithoutValidSignature(t *testing.T) {
	path, e := signedFixture(t, "original contents", "kbtpl_test")

	tampered := "malicious contents"
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	// Attacker updates the digest to match; the signature still covers the
	// ORIGINAL digest.
	newDigest := sha256.Sum256([]byte(tampered))
	e.SHA256 = hex.EncodeToString(newDigest[:])

	err := VerifyBundle(path, e)
	if err == nil {
		t.Fatal("content passed with a recomputed digest and a stale signature")
	}
	if !strings.Contains(err.Error(), "signature") {
		t.Fatalf("expected a signature failure, got: %v", err)
	}
}

func TestVerifyBundle_RejectsUnsigned(t *testing.T) {
	path, e := signedFixture(t, "contents", "kbtpl_test")
	e.Signature = ""
	e.SigningKeyID = ""

	err := VerifyBundle(path, e)
	if err == nil || !strings.Contains(err.Error(), "unsigned") {
		t.Fatalf("an unsigned bundle should be refused, got: %v", err)
	}
}

// An entry with no declared digest is unverifiable. Installing it because
// "there was nothing to check" would be the worst possible reading.
func TestVerifyBundle_RejectsMissingDigest(t *testing.T) {
	path, e := signedFixture(t, "contents", "kbtpl_test")
	e.SHA256 = ""

	err := VerifyBundle(path, e)
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("an entry without a digest should be refused, got: %v", err)
	}
}

// A key this build does not carry must be refused with an actionable message,
// never trusted because the catalog vouched for it — the catalog is exactly
// what an attacker would control.
func TestVerifyBundle_RejectsUnknownSigningKey(t *testing.T) {
	path, e := signedFixture(t, "contents", "kbtpl_test")
	e.SigningKeyID = "kbtpl_not_in_this_build"

	err := VerifyBundle(path, e)
	if err == nil {
		t.Fatal("a bundle signed by an untrusted key was accepted")
	}
	if !strings.Contains(err.Error(), "does not trust") {
		t.Fatalf("expected an untrusted-key error, got: %v", err)
	}
}

// The keyring's purpose: two keys valid at once, so rotation is not a flag
// day. A bundle signed by either trusted key installs.
func TestVerifyBundle_KeyringAllowsRotation(t *testing.T) {
	oldPath, oldEntry := signedFixture(t, "signed with the old key", "kbtpl_old")
	newPath, newEntry := signedFixture(t, "signed with the new key", "kbtpl_new")

	if err := VerifyBundle(oldPath, oldEntry); err != nil {
		t.Fatalf("previous key should still verify during rotation: %v", err)
	}
	if err := VerifyBundle(newPath, newEntry); err != nil {
		t.Fatalf("new key should verify: %v", err)
	}
}

// A signature signed by a DIFFERENT key than the entry claims must fail —
// this is the check that would catch a catalog pointing a valid key id at a
// bundle it did not sign.
func TestVerifyBundle_RejectsSignatureFromAnotherKey(t *testing.T) {
	path, e := signedFixture(t, "contents", "kbtpl_test")

	otherPub, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	trustKey(t, "kbtpl_other", hex.EncodeToString(otherPub))

	digest := sha256.Sum256([]byte("something else entirely"))
	e.Signature = hex.EncodeToString(ed25519.Sign(otherPriv, digest[:]))

	if err := VerifyBundle(path, e); err == nil {
		t.Fatal("a signature over different content was accepted")
	}
}

// The unsigned escape hatch must be its own explicit switch. This test also
// documents that it EXISTS, so nobody re-adds it accidentally believing it
// does not.
func TestVerifyBundle_UnsignedEscapeHatchIsExplicit(t *testing.T) {
	path, e := signedFixture(t, "contents", "kbtpl_test")
	e.Signature = ""
	e.SigningKeyID = ""

	if err := VerifyBundle(path, e); err == nil {
		t.Fatal("unsigned bundles must be refused by default")
	}

	t.Setenv(devUnsignedEnv, "1")
	if err := VerifyBundle(path, e); err != nil {
		t.Fatalf("with the development switch set, unsigned should install: %v", err)
	}

	// The integrity check still applies even in development.
	e.SHA256 = strings.Repeat("0", 64)
	if err := VerifyBundle(path, e); err == nil {
		t.Fatal("the development switch must not disable integrity checking")
	}
}

// Pointing the catalog at a local file must NOT imply skipping signatures.
func TestVerifyBundle_FileCatalogDoesNotImplyUnsigned(t *testing.T) {
	t.Setenv("KOOLBASE_TEMPLATE_CATALOG", "file:///tmp/catalog.json")

	path, e := signedFixture(t, "contents", "kbtpl_test")
	e.Signature = ""
	e.SigningKeyID = ""

	if err := VerifyBundle(path, e); err == nil {
		t.Fatal("a file:// catalog silently disabled signature verification")
	}
}
