package templates

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// Template bundles are verified twice: SHA-256 for integrity, Ed25519 for
// authenticity. Both are required, always. A bundle that fails either is not
// installed — it is source code about to be written into a developer's
// project, and "it downloaded fine" is not a trust statement.
//
// The signature is over the raw 32-byte digest, hex-encoded, matching how
// engine artifacts are signed (cmd/engine_publish.go). Publish and verify
// must agree on this or nothing validates.

// templateSigningKeys is the trusted keyring: id → hex-encoded Ed25519 public
// key (32 bytes).
//
// A KEYRING rather than one literal key, because rotation with a single key
// is a flag day: every CLI would have to update before the publisher could
// switch. With ids, a new key ships in a release, sits unused, and becomes
// active whenever the publisher starts signing with it — old bundles keep
// verifying throughout.
//
// The template keypair is deliberately SEPARATE from the patch and engine
// keys. Those authorise runtime and engine artifacts; this authorises source
// written into projects. A compromise of template publishing must not force
// rotation of keys protecting installed runtimes.
//
// Only public key material appears here. The private key exists solely in the
// publishing environment.
var templateSigningKeys = map[string]string{
	// TODO(keygen): fill in when the template keypair is generated.
	// "kbtpl_2026_01": "<64 hex chars — 32-byte Ed25519 public key>",
}

// devUnsignedEnv opts out of signature verification for local template
// development. Deliberately its own switch, named unsafely, and NOT implied
// by pointing KOOLBASE_TEMPLATE_CATALOG at a file:// URL — "let me test my
// local catalog" must never quietly become "stop checking signatures".
const devUnsignedEnv = "KOOLBASE_TEMPLATES_ALLOW_UNSIGNED"

// unsignedAllowed reports whether the unsafe development escape hatch is on.
func unsignedAllowed() bool {
	return strings.TrimSpace(os.Getenv(devUnsignedEnv)) == "1"
}

// VerifyBundle checks a downloaded bundle against its catalog entry.
//
// Cached bundles are verified on every use, not trusted because they were
// downloaded once: a cache directory is ordinary user-writable disk, and
// "previously downloaded" says nothing about what is there now.
func VerifyBundle(path string, e Entry) error {
	digest, err := fileDigest(path)
	if err != nil {
		return err
	}

	want := strings.ToLower(strings.TrimSpace(e.SHA256))
	got := hex.EncodeToString(digest)
	if want == "" {
		return fmt.Errorf("catalog entry %s@%s declares no sha256 — refusing to install unverifiable source",
			e.ID, e.Version)
	}
	if got != want {
		return fmt.Errorf("bundle %s@%s failed integrity check: expected %s, got %s",
			e.ID, e.Version, want, got)
	}

	return verifySignature(digest, e)
}

func verifySignature(digest []byte, e Entry) error {
	if unsignedAllowed() {
		fmt.Fprintf(os.Stderr,
			"⚠ %s=1: installing %s@%s WITHOUT signature verification. Development only.\n",
			devUnsignedEnv, e.ID, e.Version)
		return nil
	}

	if e.Signature == "" || e.SigningKeyID == "" {
		return fmt.Errorf("template %s@%s is unsigned — refusing to install", e.ID, e.Version)
	}

	pubHex, known := templateSigningKeys[e.SigningKeyID]
	if !known {
		return fmt.Errorf(
			"template %s@%s is signed with key %q, which this CLI does not trust — update with `koolbase upgrade`",
			e.ID, e.Version, e.SigningKeyID)
	}

	pub, err := hex.DecodeString(pubHex)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		// A malformed compiled-in key is a build defect, not a user problem,
		// but it must fail closed rather than skip verification.
		return fmt.Errorf("trusted key %q is malformed in this build", e.SigningKeyID)
	}

	sig, err := hex.DecodeString(e.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("template %s@%s has a malformed signature", e.ID, e.Version)
	}

	// Signed over the raw digest, matching engine artifact signing.
	if !ed25519.Verify(ed25519.PublicKey(pub), digest, sig) {
		return fmt.Errorf(
			"template %s@%s failed signature verification — the bundle does not match what Koolbase published",
			e.ID, e.Version)
	}
	return nil
}

func fileDigest(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("could not read bundle: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, fmt.Errorf("could not hash bundle: %w", err)
	}
	return h.Sum(nil), nil
}
