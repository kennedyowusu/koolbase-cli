// Package embedded verifies that the templates compiled into the CLI are a
// current export of their sources.
//
// The CLI embeds a minimal set of templates as an availability floor:
// `koolbase create` must work when template distribution does not. Those
// copies are exports from the koolbase-templates repository, written by
// `kbtpl export-embedded`, never edited by hand — a hand-maintained second
// copy drifts, and invisibly, because only the fallback path reveals it.
//
// The manifest records each tree's digest at export time. The test in this
// package recomputes them, so a stale or hand-edited embedded template fails
// `go test ./...` rather than waiting to be noticed by a user who happened to
// be offline.
package embedded

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// ManifestName is written beside the embedded trees by `kbtpl
// export-embedded`.
const ManifestName = "embedded.json"

// Manifest maps a tree's directory to the digest of its contents.
type Manifest struct {
	GeneratedAt string            `json:"generated_at"`
	Trees       map[string]string `json:"trees"`
}

// ParseManifest reads the manifest from the embedded filesystem.
func ParseManifest(fsys fs.FS, path string) (*Manifest, error) {
	data, err := fs.ReadFile(fsys, path)
	if err != nil {
		return nil, fmt.Errorf("no embedded manifest at %s — run `kbtpl export-embedded`: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	if len(m.Trees) == 0 {
		return nil, fmt.Errorf("%s records no trees", path)
	}
	return &m, nil
}

// TreeDigest hashes a directory within an embedded filesystem, matching what
// kbtpl computes over the source directory: sorted relative paths and file
// contents, independent of ordering.
//
// The manifest itself is excluded — it contains the digests, so including it
// would make every export change the value it records.
func TreeDigest(fsys fs.FS, root string) (string, error) {
	files := map[string][]byte{}

	err := fs.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if name == ".DS_Store" || name == ManifestName {
			return nil
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(path, root), "/")
		files[rel] = data
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("%s contains no files", root)
	}

	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	h := newTreeHasher()
	for _, p := range paths {
		h.add(p, files[p])
	}
	return h.sum(), nil
}

// Verify checks every tree the manifest records against the embedded
// filesystem, returning an error naming what drifted.
//
// base is the directory holding the manifest and the trees.
func Verify(fsys fs.FS, base string, m *Manifest) error {
	var problems []string

	for tree, want := range m.Trees {
		got, err := TreeDigest(fsys, base+"/"+tree)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", tree, err))
			continue
		}
		if got != want {
			problems = append(problems, fmt.Sprintf(
				"%s: embedded %s, manifest records %s", tree, got[:12], want[:12]))
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf(
			"embedded templates have drifted from their sources:\n  %s\n\nRe-export them from the koolbase-templates checkout:\n  kbtpl export-embedded --cli <path-to-koolbase-cli>",
			strings.Join(problems, "\n  "))
	}
	return nil
}
