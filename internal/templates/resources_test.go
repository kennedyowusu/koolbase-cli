package templates

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// This merge produces the document that reconciles someone's real backend.
// Every property below is one that, if wrong, either loses their
// configuration or creates resources they did not ask for.

func projectSnapshot(t *testing.T) []byte {
	t.Helper()
	// Shaped like a real snapshot, including the keys provisioning must not
	// touch.
	return []byte(`{
		"version": 2,
		"kind": "koolbase.snapshot",
		"source_project_id": "proj-1",
		"collections": [
			{"name":"profiles","read_rule":"owner","write_rule":"owner","delete_rule":"owner",
			 "owner_field":null,"rule_mode":"all","rule_conditions":[],"append_only":false}
		],
		"buckets": [{"name":"avatars","public":true}],
		"functions": [{"name":"send_email","runtime":"dart"}],
		"crons": [{"name":"nightly","schedule":"0 0 * * *"}],
		"secret_names": ["STRIPE_KEY"],
		"environments": [{"slug":"production","flags":[{"key":"beta","enabled":true}]}]
	}`)
}

func chatResources() *Resources {
	return &Resources{
		Collections: []CollectionSpec{
			{Name: "conversations", Read: "authenticated", Write: "authenticated", Delete: "owner"},
			{Name: "messages", Read: "authenticated", Write: "authenticated", Delete: "owner", AppendOnly: true},
		},
		Buckets: []BucketSpec{
			{Name: "chat_attachments", AllowedMimeTypes: []string{"image/*"}},
		},
	}
}

// THE property. A snapshot carries functions, crons, secrets, and
// per-environment config that provisioning has no business rewriting. Decoding
// into a partial struct would silently drop all of it — the developer's
// backend would be reconciled to a document missing half their configuration.
func TestMergeIntoSnapshot_PreservesEverythingItDoesNotUnderstand(t *testing.T) {
	merged, err := MergeIntoSnapshot(projectSnapshot(t), chatResources(), "chat")
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"version", "kind", "source_project_id",
		"functions", "crons", "secret_names", "environments"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("%q was dropped from the merged snapshot", key)
		}
	}

	// And their contents, not just their presence.
	if !strings.Contains(string(doc["functions"]), "send_email") {
		t.Fatalf("functions were emptied: %s", doc["functions"])
	}
	if !strings.Contains(string(doc["environments"]), "beta") {
		t.Fatalf("environment flags were lost: %s", doc["environments"])
	}
}

func TestMergeIntoSnapshot_AddsCollectionsAndBuckets(t *testing.T) {
	merged, err := MergeIntoSnapshot(projectSnapshot(t), chatResources(), "chat")
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	var doc struct {
		Collections []map[string]any `json:"collections"`
		Buckets     []map[string]any `json:"buckets"`
	}
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatal(err)
	}

	// The project's own collection survives alongside the new ones.
	names := map[string]map[string]any{}
	for _, c := range doc.Collections {
		names[c["name"].(string)] = c
	}
	for _, want := range []string{"profiles", "conversations", "messages"} {
		if _, ok := names[want]; !ok {
			t.Fatalf("collection %q missing; got %v", want, names)
		}
	}

	// Rules land in the fields the server actually reads.
	msgs := names["messages"]
	if msgs["read_rule"] != "authenticated" || msgs["delete_rule"] != "owner" {
		t.Fatalf("messages rules wrong: %v", msgs)
	}
	if msgs["append_only"] != true {
		t.Fatalf("append_only was not carried: %v", msgs)
	}
	// Fields the server expects on every collection must be present, or apply
	// would see a malformed entry.
	for _, key := range []string{"owner_field", "rule_mode", "rule_conditions"} {
		if _, ok := msgs[key]; !ok {
			t.Fatalf("collection entry is missing %q: %v", key, msgs)
		}
	}

	if len(doc.Buckets) != 2 {
		t.Fatalf("expected the project's bucket plus the template's, got %d", len(doc.Buckets))
	}
}

// A collision refuses the WHOLE install and reports every conflict at once.
func TestMergeIntoSnapshot_RefusesOnCollision(t *testing.T) {
	res := chatResources()
	// Collide on both a collection and a bucket.
	res.Collections[0].Name = "profiles"
	res.Buckets[0].Name = "avatars"

	_, err := MergeIntoSnapshot(projectSnapshot(t), res, "chat")
	if err == nil {
		t.Fatal("a name collision was silently merged")
	}

	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected a ConflictError, got %T: %v", err, err)
	}
	if len(conflict.Conflicts) != 2 {
		t.Fatalf("expected both conflicts reported at once, got %d: %v",
			len(conflict.Conflicts), conflict.Conflicts)
	}
	// The message must say nothing was created, or a developer cannot tell
	// whether they are half-installed.
	if !strings.Contains(err.Error(), "Nothing was created") {
		t.Fatalf("error should state that nothing was created:\n%v", err)
	}
}

// A collision is name existence, full stop. An existing collection whose rules
// happen to match is STILL a conflict — judging it "close enough" is reuse
// semantics arrived at informally.
func TestMergeIntoSnapshot_IdenticalRulesAreStillACollision(t *testing.T) {
	res := &Resources{
		Collections: []CollectionSpec{
			{Name: "profiles", Read: "owner", Write: "owner", Delete: "owner"},
		},
	}

	if _, err := MergeIntoSnapshot(projectSnapshot(t), res, "profile"); err == nil {
		t.Fatal("an existing collection with matching rules was treated as reusable")
	}
}

// Auth declares nothing, and must pass through untouched rather than being
// re-serialised — a no-op should be exactly that.
func TestMergeIntoSnapshot_EmptyResourcesIsAPassThrough(t *testing.T) {
	original := projectSnapshot(t)

	merged, err := MergeIntoSnapshot(original, nil, "auth")
	if err != nil {
		t.Fatalf("a template without resources should merge cleanly: %v", err)
	}
	if string(merged) != string(original) {
		t.Fatal("an empty merge rewrote the snapshot")
	}

	merged, err = MergeIntoSnapshot(original, &Resources{}, "auth")
	if err != nil {
		t.Fatal(err)
	}
	if string(merged) != string(original) {
		t.Fatal("an empty Resources rewrote the snapshot")
	}
}

// A project with no collections at all — a brand new one — must still work.
func TestMergeIntoSnapshot_EmptyProject(t *testing.T) {
	empty := []byte(`{"version":2,"kind":"koolbase.snapshot","collections":[],"buckets":[]}`)

	merged, err := MergeIntoSnapshot(empty, chatResources(), "chat")
	if err != nil {
		t.Fatalf("merge into an empty project: %v", err)
	}

	var doc struct {
		Collections []map[string]any `json:"collections"`
		Buckets     []map[string]any `json:"buckets"`
	}
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Collections) != 2 || len(doc.Buckets) != 1 {
		t.Fatalf("expected 2 collections and 1 bucket, got %d and %d",
			len(doc.Collections), len(doc.Buckets))
	}
}

func TestSummarise_DescribesWhatWillBeCreated(t *testing.T) {
	got := chatResources().Summarise()

	for _, want := range []string{"conversations", "messages", "chat_attachments",
		"append-only", "private"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary should mention %q:\n%s", want, got)
		}
	}
	if (&Resources{}).Summarise() != "" {
		t.Fatal("an empty Resources should summarise to nothing")
	}
}
