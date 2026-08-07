package templates

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// A template declares the backend it needs; installing it creates that
// backend. This is the merge half: the project's current snapshot plus the
// template's resources, refused entirely if any name is already taken.
//
// Deliberately built on snapshot apply rather than per-resource endpoints.
// Apply already reconciles collections, rules, and buckets, already has a
// dry run that produces a plan, and is already the path the platform trusts
// for structural change. A second mechanism would be a second set of bugs.
//
// These types mirror the publisher's (koolbase-templates
// cmd/kbtpl/resources.go). Duplicated rather than shared for the same reason
// the catalog type is: the two repositories are separately released, and the
// schema version is the contract between them.

// Resources is the backend a template needs.
type Resources struct {
	Collections []CollectionSpec `json:"collections,omitempty"`
	Buckets     []BucketSpec     `json:"buckets,omitempty"`
}

type CollectionSpec struct {
	Name       string `json:"name"`
	Read       string `json:"read"`
	Write      string `json:"write"`
	Delete     string `json:"delete"`
	OwnerField string `json:"owner_field,omitempty"`
	AppendOnly bool   `json:"append_only,omitempty"`
}

type BucketSpec struct {
	Name             string   `json:"name"`
	Public           bool     `json:"public,omitempty"`
	AllowedMimeTypes []string `json:"allowed_mime_types,omitempty"`
}

// IsEmpty reports whether a template needs no backend — true for auth, whose
// backend exists on every project already.
func (r *Resources) IsEmpty() bool {
	return r == nil || (len(r.Collections) == 0 && len(r.Buckets) == 0)
}

// Conflict is one resource a template wants whose name is already taken.
type Conflict struct {
	Kind string // "collection" or "bucket"
	Name string
}

// ConflictError reports every collision at once.
//
// All of them, not the first: a developer resolving these one error at a time
// would run the command four times to learn what one run could have told
// them. And nothing is created — a partial install is worse than none,
// because the failure stays invisible until something references what is
// missing.
type ConflictError struct {
	Template  string
	Conflicts []Conflict
}

func (e *ConflictError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "cannot install %q — these already exist in your project:\n", e.Template)
	for _, c := range e.Conflicts {
		fmt.Fprintf(&b, "\n  %-11s %s", c.Kind, c.Name)
	}
	b.WriteString("\n\nNothing was created and nothing was written.\n")
	b.WriteString("Rename or remove them, then run the command again.")
	return b.String()
}

// MergeIntoSnapshot adds a template's resources to a project's snapshot,
// returning the document to apply.
//
// Works on the RAW document rather than a decoded struct. The snapshot
// carries functions, crons, triggers, secrets, and per-environment config
// that provisioning has no business rewriting; decoding into a partial type
// would silently drop all of it on the way back out. Only the two arrays this
// understands are touched, and every other key survives byte-identical.
//
// Refuses entirely on any name collision. A collision is NAME existence, not
// detected incompatibility: judging an existing `messages` collection "close
// enough" would be reuse semantics arrived at informally, impossible to
// explain to the developer whose install behaved differently from someone
// else's, and the beginning of a compatibility system nobody designed.
func MergeIntoSnapshot(current []byte, r *Resources, templateID string) ([]byte, error) {
	if r.IsEmpty() {
		return current, nil
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(current, &doc); err != nil {
		return nil, fmt.Errorf("could not read your project's backend definition: %w", err)
	}

	existingNames := func(key string) (map[string]bool, []json.RawMessage, error) {
		names := map[string]bool{}
		var entries []json.RawMessage
		raw, ok := doc[key]
		if !ok || len(raw) == 0 {
			return names, entries, nil
		}
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, nil, fmt.Errorf("could not read your project's %s: %w", key, err)
		}
		for _, e := range entries {
			var named struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(e, &named); err != nil {
				return nil, nil, fmt.Errorf("could not read your project's %s: %w", key, err)
			}
			names[named.Name] = true
		}
		return names, entries, nil
	}

	haveCollection, collections, err := existingNames("collections")
	if err != nil {
		return nil, err
	}
	haveBucket, buckets, err := existingNames("buckets")
	if err != nil {
		return nil, err
	}

	var conflicts []Conflict
	for _, c := range r.Collections {
		if haveCollection[c.Name] {
			conflicts = append(conflicts, Conflict{Kind: "collection", Name: c.Name})
		}
	}
	for _, b := range r.Buckets {
		if haveBucket[b.Name] {
			conflicts = append(conflicts, Conflict{Kind: "bucket", Name: b.Name})
		}
	}
	if len(conflicts) > 0 {
		sort.Slice(conflicts, func(i, j int) bool {
			if conflicts[i].Kind != conflicts[j].Kind {
				return conflicts[i].Kind < conflicts[j].Kind
			}
			return conflicts[i].Name < conflicts[j].Name
		})
		return nil, &ConflictError{Template: templateID, Conflicts: conflicts}
	}

	// Built as maps rather than typed structs so the entries match the shape
	// the server already emits, field for field.
	for _, c := range r.Collections {
		entry := map[string]any{
			"name":            c.Name,
			"read_rule":       c.Read,
			"write_rule":      c.Write,
			"delete_rule":     c.Delete,
			"owner_field":     nil,
			"rule_mode":       "all",
			"rule_conditions": []any{},
			"append_only":     c.AppendOnly,
		}
		if c.OwnerField != "" {
			entry["owner_field"] = c.OwnerField
		}
		raw, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		collections = append(collections, raw)
	}

	for _, b := range r.Buckets {
		entry := map[string]any{
			"name":   b.Name,
			"public": b.Public,
		}
		if len(b.AllowedMimeTypes) > 0 {
			entry["allowed_mime_types"] = b.AllowedMimeTypes
		}
		raw, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		buckets = append(buckets, raw)
	}

	if doc["collections"], err = json.Marshal(collections); err != nil {
		return nil, err
	}
	if doc["buckets"], err = json.Marshal(buckets); err != nil {
		return nil, err
	}

	return json.Marshal(doc)
}

// Summarise describes what a template will create, for the plan shown before
// anything is applied.
func (r *Resources) Summarise() string {
	if r.IsEmpty() {
		return ""
	}
	var b strings.Builder
	for _, c := range r.Collections {
		fmt.Fprintf(&b, "  collection  %-22s read %s · write %s · delete %s",
			c.Name, c.Read, c.Write, c.Delete)
		if c.AppendOnly {
			b.WriteString(" · append-only")
		}
		b.WriteString("\n")
	}
	for _, bk := range r.Buckets {
		visibility := "private"
		if bk.Public {
			visibility = "public"
		}
		fmt.Fprintf(&b, "  bucket      %-22s %s\n", bk.Name, visibility)
	}
	return b.String()
}
