package templates

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Fetching, caching, and preparing a template for rendering.
//
// Cache population is LOCK-FREE. The cache entry is immutable and
// version-addressed, so no coordination is needed before work — only
// correctness at the promotion boundary. Competing processes each prepare a
// verified temporary entry and race to rename it into place; the winner
// populates the path, losers validate what landed and discard their copy.
//
// This is simpler than locking and crash-safe: a temporary directory left by
// a killed process is garbage to be swept, not a stuck lock to recover from.

const (
	// catalogTimeout bounds the catalog fetch. A slow network must degrade to
	// the cache rather than hang a scaffold.
	catalogTimeout = 10 * time.Second
	// bundleTimeout bounds a bundle download.
	bundleTimeout = 60 * time.Second
	// tempMaxAge is when an abandoned temporary directory becomes sweepable.
	tempMaxAge = 24 * time.Hour
)

// Store is the local template cache.
type Store struct {
	// Root is the cache directory, typically ~/.koolbase/templates.
	Root string
	// HTTP is the client used for catalog and bundle fetches.
	HTTP *http.Client
}

// NewStore returns a Store rooted at the user's Koolbase directory.
func NewStore() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("could not locate your home directory: %w", err)
	}
	return &Store{
		Root: filepath.Join(home, ".koolbase", "templates"),
		HTTP: &http.Client{Timeout: bundleTimeout},
	}, nil
}

// entryDir is the immutable cache path for one template version. Immutable
// because a published bundle is never replaced: once this directory exists
// and validates, its contents are correct forever.
func (s *Store) entryDir(e Entry) string {
	return filepath.Join(s.Root, string(e.Framework), e.ID, e.Version)
}

// metaFile records what was verified when an entry was promoted, so a later
// process can tell a complete cache entry from a half-written one.
type cacheMeta struct {
	ID           string    `json:"id"`
	Framework    Framework `json:"framework"`
	Version      string    `json:"version"`
	SHA256       string    `json:"sha256"`
	SigningKeyID string    `json:"signing_key_id"`
	PromotedAt   time.Time `json:"promoted_at"`
}

// Prepare returns a filesystem rooted at the requested template's contents,
// fetching and caching it if necessary.
//
// Order matters: a valid cache entry short-circuits before any network call,
// so a second scaffold of the same template is instant and works offline.
func (s *Store) Prepare(e Entry) (fs.FS, error) {
	dir := s.entryDir(e)

	if err := validateCacheEntry(dir, e); err == nil {
		return os.DirFS(dir), nil
	}

	bundlePath, cleanup, err := s.download(e)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	if err := VerifyBundle(bundlePath, e); err != nil {
		return nil, err
	}

	// Extract into a unique temporary directory on the SAME filesystem as the
	// cache, which is what keeps the promoting rename atomic.
	tmp, err := s.tempDir()
	if err != nil {
		return nil, err
	}
	extractDir := filepath.Join(tmp, "root")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	if err := ExtractBundle(bundlePath, extractDir); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	if err := writeMeta(extractDir, e); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}

	if err := s.promote(extractDir, dir, e); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	os.RemoveAll(tmp)

	return os.DirFS(dir), nil
}

// promote atomically moves a prepared entry into its immutable cache path.
//
// Losing the race is normal, not an error — but "destination exists" is NOT
// proof another process succeeded. A process killed mid-rename, a partial
// copy, or a hand-edited cache all present as existing. The winner's entry is
// validated before it is trusted, and an invalid one is reported rather than
// silently overwritten: replacing it could destroy a concurrent writer's
// in-progress work, and trusting it could render unverified source.
func (s *Store) promote(from, to string, e Entry) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}

	err := os.Rename(from, to)
	if err == nil {
		return nil
	}

	// Re-check the destination: on some platforms a competing process may
	// still have been finishing when the rename was attempted.
	if validErr := validateCacheEntry(to, e); validErr == nil {
		return nil // another process won; its entry is good.
	} else if os.IsExist(err) || dirExists(to) {
		return fmt.Errorf(
			"cached template at %s exists but does not validate (%v) — remove it and retry",
			to, validErr)
	}
	return fmt.Errorf("could not place template in cache: %w", err)
}

// validateCacheEntry reports whether a cache directory holds a complete,
// correct entry for e.
func validateCacheEntry(dir string, e Entry) error {
	data, err := os.ReadFile(filepath.Join(dir, cacheMetaName))
	if err != nil {
		return fmt.Errorf("no cache metadata: %w", err)
	}
	var m cacheMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("unreadable cache metadata: %w", err)
	}
	if m.ID != e.ID || m.Framework != e.Framework || m.Version != e.Version {
		return fmt.Errorf("cache entry is for %s/%s@%s, wanted %s/%s@%s",
			m.Framework, m.ID, m.Version, e.Framework, e.ID, e.Version)
	}
	// The digest recorded at promotion must match what the catalog now
	// declares. A mismatch means the catalog changed under an immutable URL,
	// which should be impossible — treat it as a cache miss rather than
	// serving something that no longer matches what was published.
	if !strings.EqualFold(m.SHA256, e.SHA256) {
		return fmt.Errorf("cache entry digest does not match the catalog")
	}
	return nil
}

const cacheMetaName = ".koolbase-template.json"

func writeMeta(dir string, e Entry) error {
	m := cacheMeta{
		ID: e.ID, Framework: e.Framework, Version: e.Version,
		SHA256: e.SHA256, SigningKeyID: e.SigningKeyID,
		PromotedAt: time.Now().UTC(),
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, cacheMetaName), data, 0o644)
}

// tempDir creates a uniquely named staging directory inside the cache root,
// never a fixed .tmp path — two processes must not share staging.
func (s *Store) tempDir() (string, error) {
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return "", err
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	dir := filepath.Join(s.Root, ".staging-"+hex.EncodeToString(b[:]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// SweepStaging removes abandoned staging directories. Best-effort: a failure
// here is never worth failing a scaffold over.
func (s *Store) SweepStaging() {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-tempMaxAge)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), ".staging-") {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		os.RemoveAll(filepath.Join(s.Root, entry.Name()))
	}
}

// download fetches a bundle to a temporary file and returns its path plus a
// cleanup function.
func (s *Store) download(e Entry) (string, func(), error) {
	src, err := s.bundleURL(e)
	if err != nil {
		return "", func() {}, err
	}

	f, err := os.CreateTemp("", "koolbase-bundle-*.tar.gz")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() {
		f.Close()
		os.Remove(f.Name())
	}

	body, err := s.open(src, bundleTimeout)
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("could not download template %s@%s: %w", e.ID, e.Version, err)
	}
	defer body.Close()

	// Cap the download itself: the verifier would catch oversized content
	// later, but there is no reason to write gigabytes to disk first.
	if _, err := io.Copy(f, io.LimitReader(body, maxTotalBytes+1)); err != nil {
		cleanup()
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return f.Name(), cleanup, nil
}

// bundleURL resolves an entry's bundle location against the catalog's own
// base, so a catalog served from staging or a file:// path carries its
// bundles with it rather than reaching back to production.
func (s *Store) bundleURL(e Entry) (string, error) {
	if strings.Contains(e.BundleURL, "://") {
		return e.BundleURL, nil
	}
	base, err := url.Parse(CatalogURL())
	if err != nil {
		return "", fmt.Errorf("catalog URL is not valid: %w", err)
	}
	ref, err := url.Parse(e.BundleURL)
	if err != nil {
		return "", fmt.Errorf("bundle URL is not valid: %w", err)
	}
	return base.ResolveReference(ref).String(), nil
}

// FetchCatalog retrieves and parses the catalog.
func (s *Store) FetchCatalog() (*Catalog, error) {
	body, err := s.open(CatalogURL(), catalogTimeout)
	if err != nil {
		return nil, fmt.Errorf("could not fetch the template catalog: %w", err)
	}
	defer body.Close()

	data, err := io.ReadAll(io.LimitReader(body, 4<<20))
	if err != nil {
		return nil, err
	}
	return ParseCatalog(data)
}

// open reads from an http(s) or file URL. file:// support exists so template
// development needs no upload; it changes only WHERE content is read, never
// whether bundles are verified.
func (s *Store) open(raw string, timeout time.Duration) (io.ReadCloser, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%q is not a valid URL: %w", raw, err)
	}

	switch u.Scheme {
	case "file":
		return os.Open(u.Path)
	case "http", "https":
		client := s.HTTP
		if client == nil {
			client = &http.Client{}
		}
		c := *client
		c.Timeout = timeout
		resp, err := c.Get(raw)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
		}
		return resp.Body, nil
	default:
		return nil, fmt.Errorf("unsupported URL scheme %q", u.Scheme)
	}
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
