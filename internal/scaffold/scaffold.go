// Package scaffold renders the embedded project templates into a target
// directory.
//
// Deliberately knows nothing about Flutter, cobra, or the Koolbase API: it
// takes a template tree and a set of variables, and writes files. That makes
// it testable without a network, a Flutter install, or a real project — and
// it keeps `koolbase create` a thin orchestration over something provable.
//
// Templates use {% %} delimiters rather than Go's default {{ }}. Generated
// files are Dart, and Dart contains braces everywhere — map literals, set
// literals, string interpolation. {% %} cannot collide with any of it, and a
// variable in a .dart file is visually obvious.
package scaffold

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// Vars are the values a template tree renders against. Every field is
// something the caller knows before rendering starts; the engine never
// derives or fetches anything.
type Vars struct {
	// AppName is the Dart package name: lowercase, underscores.
	AppName string
	// AppTitle is the human-facing name shown in the UI.
	AppTitle string
	// OrgIdentifier is the reverse-domain prefix, e.g. "com.example".
	OrgIdentifier string

	// ProjectID is the Koolbase project the app talks to.
	ProjectID string
	// PublicKey is the environment's pk_live_ key.
	PublicKey string
	// EnvironmentSlug names the environment the default config points at.
	EnvironmentSlug string
	// BaseURL is the Koolbase API the app initializes against.
	BaseURL string

	// Flavors is true when the app is generated with multi-environment
	// configs rather than a single one.
	Flavors bool

	// SDKVersion is the koolbase_flutter constraint written into pubspec.
	SDKVersion string
}

// suffix marking a file as a template to render. Stripped from the output
// name: `app.dart.tmpl` renders to `app.dart`.
//
// Files WITHOUT this suffix are copied byte-for-byte. That matters for
// anything containing {% legitimately, and for binary assets.
const templateSuffix = ".tmpl"

// Render walks src (an embedded or on-disk filesystem), renders every file
// against vars, and writes the result under dst.
//
// Directory names are rendered too, so a tree can carry {% .AppName %} in a
// path. Empty rendered files are still written — a template that renders to
// nothing is a template bug, and a silently missing file is harder to notice
// than an empty one.
func Render(src fs.FS, root string, dst string, vars Vars) error {
	return fs.WalkDir(src, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		outRel, err := renderPath(rel, vars)
		if err != nil {
			return fmt.Errorf("path %s: %w", rel, err)
		}
		outPath := filepath.Join(dst, outRel)

		if d.IsDir() {
			return os.MkdirAll(outPath, 0o755)
		}

		content, err := fs.ReadFile(src, path)
		if err != nil {
			return err
		}

		// Non-template files are copied verbatim.
		if !strings.HasSuffix(outPath, templateSuffix) {
			if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
				return err
			}
			return os.WriteFile(outPath, content, 0o644)
		}

		outPath = strings.TrimSuffix(outPath, templateSuffix)
		rendered, err := renderBytes(rel, content, vars)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return err
		}
		return os.WriteFile(outPath, rendered, 0o644)
	})
}

// renderBytes renders one template body.
//
// A template referencing a field Vars does not have is an ERROR, not an empty
// string — Go's template engine enforces that for struct data, which is why
// Vars is a struct and not a map. (missingkey=error is set for the same
// reason but governs map keys only; it is belt-and-braces if Vars ever
// changes shape.) The failure it prevents: a config silently missing its
// public key produces an app that fails at runtime with no clue why.
func renderBytes(name string, body []byte, vars Vars) ([]byte, error) {
	t, err := template.New(name).
		Delims("{%", "%}").
		Parse(string(body))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, vars); err != nil {
		return nil, fmt.Errorf("render %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

// renderPath renders template variables appearing in a file or directory
// name, so a tree can be organised by app or feature name.
func renderPath(rel string, vars Vars) (string, error) {
	if !strings.Contains(rel, "{%") {
		return rel, nil
	}
	out, err := renderBytes("path:"+rel, []byte(rel), vars)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
