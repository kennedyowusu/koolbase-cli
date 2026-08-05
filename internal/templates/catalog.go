// Package templates resolves Koolbase project templates: fetching the
// catalog, choosing a compatible version, and locating the bundle to install.
//
// Distribution is deliberately independent of the Koolbase API. The catalog
// is a public document at a Koolbase-owned URL, so template availability does
// not depend on control-plane uptime, deployment state, gateway behaviour, or
// regional routing. The URL is Koolbase-owned rather than a raw R2 object URL
// so storage can move — CDN, bucket rename, regional distribution — without a
// CLI release.
package templates

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// DefaultCatalogURL is compiled into the CLI. KOOLBASE_TEMPLATE_CATALOG
// overrides it for development and staging, and accepts a file:// URL so
// template work needs no upload.
//
// The override changes WHERE the catalog is read from. It does NOT relax
// bundle verification — that requires a separate, explicit, development-only
// switch, because "point at my local catalog" must never quietly become
// "stop checking signatures".
const DefaultCatalogURL = "https://templates.koolbase.com/catalog.json"

// CatalogURL returns the catalog location for this run.
func CatalogURL() string {
	if v := strings.TrimSpace(os.Getenv("KOOLBASE_TEMPLATE_CATALOG")); v != "" {
		return v
	}
	return DefaultCatalogURL
}

// Framework identifies a client SDK a template targets. Framework is a field
// and a resolution dimension, never a distribution boundary: one catalog
// carries every framework's templates so the CLI fetches once, filters
// locally, and cannot observe one framework published while another is stale.
type Framework string

const (
	Flutter     Framework = "flutter"
	ReactNative Framework = "react_native"
)

// Catalog is the published index of every available template version.
//
// SchemaVersion gates the CLI's willingness to read it: a catalog written to
// a newer schema than the binary understands is refused with a message to
// update, rather than parsed optimistically into wrong behaviour.
type Catalog struct {
	SchemaVersion int     `json:"schema_version"`
	Templates     []Entry `json:"templates"`
}

// CatalogSchemaVersion is the schema this build understands.
const CatalogSchemaVersion = 1

// Entry is one template at one version for one framework.
//
// Three version axes, deliberately independent:
//
//   - Version           — the template's own release
//   - FrameworkVersions — the Flutter/RN range it is known to work on
//   - SDKVersions       — the koolbase_flutter / @koolbase/react-native range
//     it generates code against
//
// The SDK range is what stops a template built for 10.x installing against
// 11.x and generating code that does not compile.
type Entry struct {
	ID          string    `json:"id"`
	Framework   Framework `json:"framework"`
	Version     string    `json:"version"`
	Title       string    `json:"title"`
	Description string    `json:"description"`

	FrameworkVersions string `json:"framework_constraint"`
	SDKVersions       string `json:"koolbase_sdk_constraint"`

	// BundleURL is IMMUTABLE. A published bundle is never replaced: the
	// catalog's "current version" is a pointer, and 1.2.0 is the same bytes
	// forever. Without that, a pinned version means whatever was last
	// uploaded under the name, and reproducibility is a fiction.
	BundleURL string `json:"bundle_url"`
	SHA256    string `json:"sha256"`
	Signature string `json:"signature"`

	// SigningKeyID names which template-signing key produced Signature, so a
	// new key can ship in a CLI release and sit unused until the publisher
	// switches. Without it, rotation is a flag day: every CLI must update
	// before the publisher can change keys.
	SigningKeyID string `json:"signing_key_id"`

	// Deprecated marks a version that should not be chosen automatically but
	// remains resolvable when pinned — withdrawing a version must not break
	// a build that named it.
	Deprecated bool   `json:"deprecated,omitempty"`
	Notice     string `json:"notice,omitempty"`
}

// ParseCatalog decodes a catalog document and refuses one written to a schema
// this build does not understand.
func ParseCatalog(data []byte) (*Catalog, error) {
	var c Catalog
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("template catalog is not valid JSON: %w", err)
	}
	if c.SchemaVersion == 0 {
		return nil, fmt.Errorf("template catalog is missing schema_version")
	}
	if c.SchemaVersion > CatalogSchemaVersion {
		return nil, fmt.Errorf(
			"this template catalog uses schema version %d; this CLI understands %d — update with `koolbase upgrade`",
			c.SchemaVersion, CatalogSchemaVersion)
	}
	return &c, nil
}

// Request is what a caller wants: a template id, a framework, and optionally
// an exact version (`chat@1.2.0`).
type Request struct {
	ID        string
	Framework Framework
	// Version is empty for "newest compatible", or an exact version string.
	Version string
}

// ParseRef splits `chat` or `chat@1.2.0` into a Request.
func ParseRef(ref string, framework Framework) (Request, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Request{}, fmt.Errorf("a template name is required")
	}
	id, version, found := strings.Cut(ref, "@")
	if id == "" || (found && version == "") {
		return Request{}, fmt.Errorf("%q is not a valid template reference — use `chat` or `chat@1.2.0`", ref)
	}
	return Request{ID: id, Framework: framework, Version: version}, nil
}

// Resolve picks the entry to install.
//
// An exact version always resolves to itself, deprecated or not: a build that
// pinned a version must keep producing the same source. Without a version,
// the newest non-deprecated entry compatible with the environment wins.
func Resolve(c *Catalog, req Request, env Environment) (*Entry, error) {
	var forID []Entry
	for _, e := range c.Templates {
		if e.ID == req.ID && e.Framework == req.Framework {
			forID = append(forID, e)
		}
	}
	if len(forID) == 0 {
		return nil, fmt.Errorf("no %s template named %q — run `koolbase templates list` to see what is available",
			req.Framework, req.ID)
	}

	if req.Version != "" {
		for i := range forID {
			if forID[i].Version == req.Version {
				return &forID[i], nil
			}
		}
		return nil, fmt.Errorf("%s@%s not found — available: %s",
			req.ID, req.Version, strings.Join(versionsOf(forID), ", "))
	}

	// Newest first, so the first compatible candidate wins.
	sort.Slice(forID, func(i, j int) bool {
		return compareVersions(forID[i].Version, forID[j].Version) > 0
	})

	var skipped []string
	for i := range forID {
		e := forID[i]
		if e.Deprecated {
			continue
		}
		if err := env.Satisfies(e); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%v)", e.Version, err))
			continue
		}
		return &e, nil
	}

	if len(skipped) > 0 {
		return nil, fmt.Errorf("no version of %q is compatible with your environment:\n  %s",
			req.ID, strings.Join(skipped, "\n  "))
	}
	return nil, fmt.Errorf("every published version of %q is deprecated — pin one explicitly if you still need it", req.ID)
}

func versionsOf(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Version)
	}
	return out
}
