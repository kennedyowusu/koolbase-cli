package templates

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The store decides whether a scaffold uses fresh source, cached source, or
// fails. Its promotion path runs concurrently across processes, which is
// where a bug would be invisible until it was expensive.

// bundleFixture builds a signed bundle containing one file, trusts its key
// for the test, and returns the tar.gz bytes plus a matching entry.
func bundleFixture(t *testing.T, id, version, body string) ([]byte, Entry) {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name: "pubspec.yaml.tmpl", Mode: 0o644,
		Size: int64(len(body)), Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()

	raw := buf.Bytes()
	digest := sha256.Sum256(raw)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyID := "kbtpl_store_test"
	trustKey(t, keyID, hex.EncodeToString(pub))

	return raw, Entry{
		ID:           id,
		Framework:    Flutter,
		Version:      version,
		SHA256:       hex.EncodeToString(digest[:]),
		Signature:    hex.EncodeToString(ed25519.Sign(priv, digest[:])),
		SigningKeyID: keyID,
	}
}

// serveBundle stands up a server for one bundle and points the entry at it.
func serveBundle(t *testing.T, raw []byte, e *Entry) *httptest.Server {
	t.Helper()
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	e.BundleURL = srv.URL + "/bundle.tar.gz"
	t.Cleanup(func() { t.Logf("bundle served %d time(s)", hits) })
	return srv
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return &Store{Root: filepath.Join(t.TempDir(), "templates"), HTTP: &http.Client{}}
}

func TestStore_PrepareDownloadsVerifiesAndExtracts(t *testing.T) {
	raw, e := bundleFixture(t, "chat", "1.0.0", "name: demo\n")
	serveBundle(t, raw, &e)
	s := newTestStore(t)

	fsys, err := s.Prepare(e)
	if err != nil {
		t.Fatalf("prepare failed: %v", err)
	}

	got, err := fs.ReadFile(fsys, "pubspec.yaml.tmpl")
	if err != nil || !strings.Contains(string(got), "name: demo") {
		t.Fatalf("template contents not available: %v %q", err, got)
	}
}

// A second Prepare must not touch the network — that is what makes a repeat
// scaffold instant and what lets it work offline.
func TestStore_SecondPrepareUsesCache(t *testing.T) {
	raw, e := bundleFixture(t, "chat", "1.0.0", "name: demo\n")

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write(raw)
	}))
	defer srv.Close()
	e.BundleURL = srv.URL + "/bundle.tar.gz"

	s := newTestStore(t)
	if _, err := s.Prepare(e); err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	if hits != 1 {
		t.Fatalf("expected one download, got %d", hits)
	}

	if _, err := s.Prepare(e); err != nil {
		t.Fatalf("second prepare: %v", err)
	}
	if hits != 1 {
		t.Fatalf("cache was ignored: %d downloads", hits)
	}
}

// With a populated cache and the server gone, Prepare must still succeed.
func TestStore_CachedEntryWorksOffline(t *testing.T) {
	raw, e := bundleFixture(t, "chat", "1.0.0", "name: demo\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(raw)
	}))
	e.BundleURL = srv.URL + "/bundle.tar.gz"

	s := newTestStore(t)
	if _, err := s.Prepare(e); err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	srv.Close() // network is now gone

	if _, err := s.Prepare(e); err != nil {
		t.Fatalf("a cached template must not need the network: %v", err)
	}
}

// A bundle whose contents changed after signing must not be cached, however
// the download succeeded.
func TestStore_RefusesTamperedBundle(t *testing.T) {
	raw, e := bundleFixture(t, "chat", "1.0.0", "name: demo\n")
	tampered := append([]byte{}, raw...)
	tampered[len(tampered)-1] ^= 0xff

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tampered)
	}))
	defer srv.Close()
	e.BundleURL = srv.URL + "/bundle.tar.gz"

	s := newTestStore(t)
	if _, err := s.Prepare(e); err == nil {
		t.Fatal("a tampered bundle was cached")
	}
	if dirExists(s.entryDir(e)) {
		t.Fatal("a failed verification left a cache entry behind")
	}
}

// Losing the promotion race is normal: the winner's entry is validated and
// reused rather than overwritten.
func TestStore_PromoteLoserReusesValidWinner(t *testing.T) {
	raw, e := bundleFixture(t, "chat", "1.0.0", "name: demo\n")
	serveBundle(t, raw, &e)
	s := newTestStore(t)

	// Populate the cache as the "winner".
	if _, err := s.Prepare(e); err != nil {
		t.Fatalf("winner prepare: %v", err)
	}
	winnerMeta := filepath.Join(s.entryDir(e), cacheMetaName)
	before, err := os.ReadFile(winnerMeta)
	if err != nil {
		t.Fatal(err)
	}

	// A second process arrives with its own staged copy and loses the race.
	staged := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(staged, e); err != nil {
		t.Fatal(err)
	}
	if err := s.promote(staged, s.entryDir(e), e); err != nil {
		t.Fatalf("losing the race should not be an error: %v", err)
	}

	after, err := os.ReadFile(winnerMeta)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the loser overwrote the winner's cache entry")
	}
}

// "Destination exists" is NOT proof another process succeeded. A partial or
// corrupted entry must be reported, never trusted and never silently
// replaced — replacing it could destroy a concurrent writer's work.
func TestStore_PromoteRefusesInvalidExistingEntry(t *testing.T) {
	raw, e := bundleFixture(t, "chat", "1.0.0", "name: demo\n")
	_ = raw
	s := newTestStore(t)

	// A directory exists at the cache path but holds no valid metadata —
	// what a process killed mid-promotion would leave.
	dir := s.entryDir(e)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	staged := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(staged, e); err != nil {
		t.Fatal(err)
	}

	err := s.promote(staged, dir, e)
	if err == nil {
		t.Fatal("an incomplete cache entry was treated as a successful race loss")
	}
	if !strings.Contains(err.Error(), "does not validate") {
		t.Fatalf("error should say the entry is invalid, got: %v", err)
	}
}

// A cache entry recorded against a different digest is a miss, not a hit: the
// catalog changed under a URL that was supposed to be immutable, and serving
// the old contents would silently deliver something other than what is
// published.
func TestStore_CacheEntryWithWrongDigestIsAMiss(t *testing.T) {
	_, e := bundleFixture(t, "chat", "1.0.0", "name: demo\n")
	s := newTestStore(t)

	dir := s.entryDir(e)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := e
	stale.SHA256 = strings.Repeat("a", 64)
	if err := writeMeta(dir, stale); err != nil {
		t.Fatal(err)
	}

	if err := validateCacheEntry(dir, e); err == nil {
		t.Fatal("a digest mismatch was treated as a cache hit")
	}
}

// Template development must work without uploading anything.
func TestStore_FileURLCatalogAndBundle(t *testing.T) {
	raw, e := bundleFixture(t, "chat", "1.0.0", "name: local\n")

	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "chat-1.0.0.tar.gz")
	if err := os.WriteFile(bundlePath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	e.BundleURL = "file://" + bundlePath

	catalogPath := filepath.Join(dir, "catalog.json")
	data, err := json.Marshal(Catalog{SchemaVersion: CatalogSchemaVersion, Templates: []Entry{e}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalogPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KOOLBASE_TEMPLATE_CATALOG", "file://"+catalogPath)

	s := newTestStore(t)
	c, err := s.FetchCatalog()
	if err != nil {
		t.Fatalf("local catalog fetch failed: %v", err)
	}
	if len(c.Templates) != 1 {
		t.Fatalf("expected one template, got %d", len(c.Templates))
	}

	fsys, err := s.Prepare(c.Templates[0])
	if err != nil {
		t.Fatalf("local bundle prepare failed: %v", err)
	}
	got, err := fs.ReadFile(fsys, "pubspec.yaml.tmpl")
	if err != nil || !strings.Contains(string(got), "name: local") {
		t.Fatalf("local template not usable: %v %q", err, got)
	}
}

// Relative bundle URLs resolve against the catalog's own base, so a staging
// catalog carries its bundles rather than reaching back to production.
func TestStore_RelativeBundleURLResolvesAgainstCatalog(t *testing.T) {
	t.Setenv("KOOLBASE_TEMPLATE_CATALOG", "https://staging-templates.example.com/catalog.json")
	s := newTestStore(t)

	got, err := s.bundleURL(Entry{BundleURL: "templates/flutter/chat/1.0.0.tar.gz"})
	if err != nil {
		t.Fatal(err)
	}
	want := "https://staging-templates.example.com/templates/flutter/chat/1.0.0.tar.gz"
	if got != want {
		t.Fatalf("relative bundle URL resolved to %s, want %s", got, want)
	}
}

func TestStore_SweepStagingLeavesFreshDirectories(t *testing.T) {
	s := newTestStore(t)
	dir, err := s.tempDir()
	if err != nil {
		t.Fatal(err)
	}
	s.SweepStaging()
	if !dirExists(dir) {
		t.Fatal("a fresh staging directory was swept")
	}
}
