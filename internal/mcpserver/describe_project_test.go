package mcpserver

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kennedyowusu/koolbase-cli/internal/api"
)

// snapshotWithEverything is a snapshot payload carrying every section the
// server can emit, INCLUDING the ones an agent must never receive: function
// source, a pubspec, and secret names. Tests project this and assert on what
// survives.
//
// It also carries an unknown top-level section ("crons") and an unknown field
// inside a known section, standing in for whatever the server adds next.
const snapshotWithEverything = `{
  "version": 2,
  "kind": "koolbase.snapshot",
  "source_project_id": "364cafa3-019b-4af5-86da-da182cf1de3a",
  "collections": [
    {
      "name": "expenses",
      "read_rule": "authenticated",
      "write_rule": "scoped",
      "delete_rule": "server_only",
      "owner_field": "created_by",
      "rule_mode": "",
      "rule_conditions": null,
      "append_only": false
    },
    {
      "name": "audit_log",
      "read_rule": "conditional",
      "write_rule": "server_only",
      "delete_rule": "server_only",
      "owner_field": null,
      "rule_mode": "all",
      "rule_conditions": [{"field":"org_id","op":"eq","value":"$user.org_id"}],
      "append_only": true
    },
    {
      "name": "announcements",
      "read_rule": "public",
      "write_rule": "server_only",
      "delete_rule": "server_only",
      "owner_field": null,
      "rule_mode": "",
      "rule_conditions": null,
      "append_only": false
    },
    {
      "name": "drafts",
      "read_rule": "owner",
      "write_rule": "owner",
      "delete_rule": "owner",
      "owner_field": null,
      "rule_mode": "",
      "rule_conditions": null,
      "append_only": false
    }
  ],
  "environments": [{"name":"Development","slug":"dev","internal_note":"leak-canary"}],
  "buckets": [{"name":"receipts","public":false,"access_mode":"private","max_size_bytes":null,"max_file_size_bytes":5242880,"allowed_mime_types":["image/png"],"versioning_enabled":true}],
  "secret_names": ["STRIPE_SECRET_KEY","TWILIO_AUTH_TOKEN"],
  "functions": [
    {
      "name": "settle",
      "runtime": "dart",
      "timeout_ms": 30000,
      "requires_auth": true,
      "enabled": true,
      "source": "import 'dart:convert'; void main() { /* proprietary */ }",
      "pubspec": "name: settle\ndependencies:\n  http: ^1.0.0"
    }
  ],
  "crons": [{"name":"nightly","schedule":"0 2 * * *"}]
}`

// mustProject projects the payload with no constraint fetching and marshals
// the result, returning both the struct and its JSON form. Most assertions
// here are about what appears in the serialized output, since that is what
// actually reaches an agent.
func mustProject(t *testing.T, raw string) (describeProjectOut, string) {
	t.Helper()
	out, err := projectSnapshot([]byte(raw), nil)
	if err != nil {
		t.Fatalf("projectSnapshot returned error: %v", err)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("failed to marshal projection: %v", err)
	}
	return out, string(encoded)
}

// THE safety test. Function source, pubspec, and secret names all exist in the
// snapshot and must not survive projection. An agent that receives a secret
// name has been handed a target; one that receives function source has been
// handed an implementation it was never meant to see.
//
// Mutation-verified: add `Source` to functionOut and this test fails.
func TestProjectSnapshot_LeaksNoSecretsOrSource(t *testing.T) {
	_, encoded := mustProject(t, snapshotWithEverything)

	forbidden := []string{
		"source", "pubspec", "secret", "STRIPE_SECRET_KEY", "TWILIO_AUTH_TOKEN",
		"proprietary", "dependencies",
	}
	lower := strings.ToLower(encoded)
	for _, f := range forbidden {
		if strings.Contains(lower, strings.ToLower(f)) {
			t.Fatalf("projection leaked %q into agent-facing output:\n%s", f, encoded)
		}
	}
}

// The allow-list property, stated as its own test: sections and fields the
// projection does not name must not appear, even when present upstream. This
// is what makes a new server-side section invisible by default rather than
// agent-visible by default.
func TestProjectSnapshot_IgnoresUnknownUpstreamFields(t *testing.T) {
	_, encoded := mustProject(t, snapshotWithEverything)

	// Assert on JSON KEY form (`"name":`) rather than bare words: `version`
	// is a substring of the legitimately-kept `versioning_enabled`, and
	// `kind` is a substring of every access rule's own `kind` key. Naive
	// substring canaries produce false failures and teach nothing.
	for _, unknownKey := range []string{`"crons"`, `"internal_note"`, `"version"`, `"kind_"`, `"snapshot_version"`} {
		if strings.Contains(encoded, unknownKey) {
			t.Fatalf("unknown upstream field %s passed through the projection:\n%s", unknownKey, encoded)
		}
	}
	// Values from unknown sections must be absent too — a section could leak
	// as a nested value rather than a top-level key.
	for _, unknownValue := range []string{"nightly", "leak-canary"} {
		if strings.Contains(encoded, unknownValue) {
			t.Fatalf("value %q from an unprojected section leaked:\n%s", unknownValue, encoded)
		}
	}
}

// Rules must arrive as data, with each kind carrying exactly its own
// satellites and nothing else. An agent reading
// {"kind":"scoped","owner_field":"user_id","caller_key":"id"} has no room to
// generate an unscoped write.
func TestProjectSnapshot_RulesAsDataPerKind(t *testing.T) {
	out, _ := mustProject(t, snapshotWithEverything)

	if len(out.Collections) != 4 {
		t.Fatalf("expected 4 collections, got %d", len(out.Collections))
	}

	expenses := out.Collections[0]
	if expenses.Read.Kind != "authenticated" {
		t.Fatalf("expenses read kind: want authenticated, got %q", expenses.Read.Kind)
	}
	if expenses.Read.OwnerField != "" || expenses.Read.CallerKey != "" {
		t.Fatalf("ownership fields must not ride on an authenticated rule: %+v", expenses.Read)
	}
	if expenses.Write.Kind != "scoped" {
		t.Fatalf("expenses write kind: want scoped, got %q", expenses.Write.Kind)
	}
	if expenses.Write.OwnerField != "created_by" || expenses.Write.CallerKey != "created_by" {
		t.Fatalf("bare owner_field spec must bind same-name (legacy form), got owner_field=%q caller_key=%q",
			expenses.Write.OwnerField, expenses.Write.CallerKey)
	}
	if expenses.Delete.Kind != "server_only" {
		t.Fatalf("expenses delete kind: want server_only, got %q", expenses.Delete.Kind)
	}

	audit := out.Collections[1]
	if audit.Read.Kind != "conditional" {
		t.Fatalf("audit_log read kind: want conditional, got %q", audit.Read.Kind)
	}
	if audit.Read.Mode != "all" {
		t.Fatalf("conditional rule must carry mode, got %q", audit.Read.Mode)
	}
	if len(audit.Read.Conditions) == 0 {
		t.Fatal("conditional rule must carry its conditions")
	}
	if !audit.AppendOnly {
		t.Fatal("append_only must survive projection")
	}
	if audit.Write.Mode != "" {
		t.Fatalf("mode must not ride on a non-conditional rule, got %q", audit.Write.Mode)
	}

	if out.Collections[2].Read.Kind != "public" {
		t.Fatalf("announcements read kind: want public, got %q", out.Collections[2].Read.Kind)
	}

	// kind=owner: ownership is the server-stamped created_by against the
	// caller's id. The collection declares no owner_field, so a projection
	// that merely passed the column through would leave an agent unable to
	// tell what establishes ownership — the production gap this test guards.
	drafts := out.Collections[3]
	if drafts.Read.Kind != "owner" {
		t.Fatalf("drafts read kind: want owner, got %q", drafts.Read.Kind)
	}
	if drafts.Read.OwnerField != "created_by" || drafts.Read.CallerKey != "id" {
		t.Fatalf("owner rule must resolve to created_by/id, got owner_field=%q caller_key=%q",
			drafts.Read.OwnerField, drafts.Read.CallerKey)
	}
}

// The $caller binding is a BINDING, not a field name. "user_id=$caller.id"
// means record.user_id == caller.id, and must be split accordingly — emitting
// the raw spec would have an agent write a field literally named
// "user_id=$caller.id". Mirrors the server's parseOwnerField exactly.
func TestProjectSnapshot_ScopedOwnerFieldBindingIsSplit(t *testing.T) {
	_, encoded := mustProject(t, `{
      "source_project_id": "p1",
      "collections": [{
        "name": "conversation_members",
        "read_rule": "scoped", "write_rule": "authenticated", "delete_rule": "owner",
        "owner_field": "user_id=$caller.id",
        "rule_mode": "", "rule_conditions": null, "append_only": false
      }]
    }`)

	if strings.Contains(encoded, `$caller`) {
		t.Fatalf("raw owner_field binding leaked into agent-facing output:\n%s", encoded)
	}
	if !strings.Contains(encoded, `"owner_field":"user_id"`) {
		t.Fatalf("expected owner_field split to the record field user_id:\n%s", encoded)
	}
	if !strings.Contains(encoded, `"caller_key":"id"`) {
		t.Fatalf("expected caller_key split to id:\n%s", encoded)
	}
}

// Every rule kind the platform defines must map. A kind this projection does
// not understand must fail loudly rather than emit a bare {"kind":"..."} an
// agent cannot act on — which is exactly how `owner` reached production output
// unmapped. Guards the next kind the platform adds.
func TestProjectSnapshot_EveryPlatformRuleKindMaps(t *testing.T) {
	// The vocabulary as the server declares it (internal/database/service.go).
	for _, kind := range []string{"public", "authenticated", "owner", "scoped", "conditional", "server_only"} {
		ownerField := "null"
		if kind == "scoped" {
			ownerField = `"user_id=$caller.id"`
		}
		raw := `{"source_project_id":"p1","collections":[{
          "name":"c","read_rule":"` + kind + `","write_rule":"` + kind + `","delete_rule":"` + kind + `",
          "owner_field":` + ownerField + `,"rule_mode":"all","rule_conditions":[],"append_only":false}]}`

		out, err := projectSnapshot([]byte(raw), nil)
		if err != nil {
			t.Fatalf("rule kind %q failed to project: %v", kind, err)
		}
		if got := out.Collections[0].Read.Kind; got != kind {
			t.Fatalf("rule kind %q projected as %q", kind, got)
		}
	}

	// Negative control: an unrecognized kind must be an error, not a silent
	// passthrough. Without this, the test above would pass for a projection
	// that emitted any string it was handed.
	_, err := projectSnapshot([]byte(`{"source_project_id":"p1","collections":[{
      "name":"c","read_rule":"telepathy","write_rule":"owner","delete_rule":"owner",
      "owner_field":null,"rule_mode":"","rule_conditions":null,"append_only":false}]}`), nil)
	if err == nil {
		t.Fatal("unknown rule kind projected silently; it must fail loudly")
	}
	if !strings.Contains(err.Error(), "telepathy") {
		t.Fatalf("error should name the unknown kind, got: %v", err)
	}
}

// Unique constraints are the ONLY declared field names a schemaless platform
// holds, so they must reach the agent — projected down to fields and
// case-sensitivity, with constraint/collection/project IDs (agent-useless)
// dropped by the same allow-list discipline as everything else.
func TestProjectSnapshot_AttachesUniqueConstraints(t *testing.T) {
	fetched := []string{}
	fetch := func(collection string) ([]api.UniqueConstraint, error) {
		fetched = append(fetched, collection)
		if collection == "expenses" {
			return []api.UniqueConstraint{{
				ID: "uc-1", ProjectID: "p1", CollectionID: "col-1",
				Fields: []string{"title", "period"}, CaseInsensitive: true,
				CreatedAt: "2026-08-04T00:00:00Z",
			}}, nil
		}
		return nil, nil
	}

	out, err := projectSnapshot([]byte(snapshotWithEverything), fetch)
	if err != nil {
		t.Fatalf("projectSnapshot returned error: %v", err)
	}
	if len(fetched) != 4 {
		t.Fatalf("expected constraints fetched for all 4 collections, got %v", fetched)
	}

	expenses := out.Collections[0]
	if len(expenses.UniqueConstraints) != 1 {
		t.Fatalf("expected 1 constraint on expenses, got %d", len(expenses.UniqueConstraints))
	}
	uc := expenses.UniqueConstraints[0]
	if len(uc.Fields) != 2 || uc.Fields[0] != "title" || uc.Fields[1] != "period" || !uc.CaseInsensitive {
		t.Fatalf("constraint not preserved: %+v", uc)
	}

	// IDs are noise to an agent and must not survive the projection.
	encoded, _ := json.Marshal(out)
	for _, forbidden := range []string{"uc-1", "col-1", `"collection_id"`, `"project_id":"p1"`} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("constraint internals leaked into agent-facing output: %q in\n%s", forbidden, encoded)
		}
	}

	// A collection with no constraints serializes [] not null, so an agent
	// distinguishes "none declared" from "unknown".
	if !strings.Contains(string(encoded), `"unique_constraints":[]`) {
		t.Fatalf("constraint-less collections must carry an empty array:\n%s", encoded)
	}
}

// A failed constraint fetch must fail the whole description. Silently
// emitting a description with constraints absent would have an agent generate
// duplicate writes believing them legal — absent-but-existing is worse than
// an error the agent can see.
func TestProjectSnapshot_ConstraintFetchFailureFailsLoudly(t *testing.T) {
	fetch := func(collection string) ([]api.UniqueConstraint, error) {
		return nil, errors.New("boom")
	}
	_, err := projectSnapshot([]byte(snapshotWithEverything), fetch)
	if err == nil {
		t.Fatal("constraint fetch failure was swallowed; it must fail the projection")
	}
	if !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "expenses") {
		t.Fatalf("error should carry the cause and name the collection, got: %v", err)
	}
}

// Function signatures must survive even as bodies are dropped — an agent needs
// to know `settle` exists and requires auth. Guards against "fix" the leak test
// by dropping functions entirely.
func TestProjectSnapshot_KeepsFunctionSignatures(t *testing.T) {
	out, _ := mustProject(t, snapshotWithEverything)

	if len(out.Functions) != 1 {
		t.Fatalf("expected 1 function, got %d", len(out.Functions))
	}
	f := out.Functions[0]
	if f.Name != "settle" || f.Runtime != "dart" || !f.RequiresAuth || !f.Enabled || f.TimeoutMs != 30000 {
		t.Fatalf("function signature not preserved: %+v", f)
	}
}

// Empty sections must serialize as [] rather than null, so an agent reading the
// output distinguishes "this project has no buckets" from "buckets unknown".
func TestProjectSnapshot_EmptySectionsAreArraysNotNull(t *testing.T) {
	_, encoded := mustProject(t, `{"source_project_id":"p1","collections":[]}`)

	for _, want := range []string{`"collections":[]`, `"environments":[]`, `"buckets":[]`, `"functions":[]`} {
		if !strings.Contains(encoded, want) {
			t.Fatalf("expected %s in output, got:\n%s", want, encoded)
		}
	}
}

// --- sdk_conventions tests ---------------------------------------------------

// Both platforms must return content carrying the shared semantics — the
// claims that must never drift between dialects (SWR, the signed-out throw,
// conflicts not expiring).
func TestSdkConventions_SharedSemanticsInBothPlatforms(t *testing.T) {
	for _, platform := range []string{"flutter", "react_native"} {
		content, err := sdkConventionsFor(platform)
		if err != nil {
			t.Fatalf("platform %q errored: %v", platform, err)
		}
		for _, claim := range []string{"stale-while-revalidate", "THROW", "Conflicts do not expire", "server-enforced"} {
			if !strings.Contains(strings.ToLower(content), strings.ToLower(claim)) {
				t.Fatalf("platform %q conventions missing shared claim %q", platform, claim)
			}
		}
	}
}

// Dialects must not bleed: the Flutter builder chain must not appear in the
// RN content, and RN's query-with-options shape must not appear in Flutter's.
// This is the exact confusion the tool exists to prevent.
func TestSdkConventions_DialectsDoNotBleed(t *testing.T) {
	flutter, err := sdkConventionsFor("flutter")
	if err != nil {
		t.Fatal(err)
	}
	rn, err := sdkConventionsFor("react_native")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(flutter, ".collection('expenses')") {
		t.Fatal("flutter conventions must teach the builder chain")
	}
	if !strings.Contains(rn, "Koolbase.db.query('expenses'") {
		t.Fatal("react_native conventions must teach query-with-options")
	}
	if strings.Contains(rn, ".where(") {
		t.Fatal("the Flutter .where chain leaked into react_native conventions")
	}
	if strings.Contains(flutter, "Koolbase.db.query(") {
		t.Fatal("the RN query-with-options shape leaked into flutter conventions")
	}
}

// An unknown platform must fail naming the valid ones, so an agent that
// guesses (e.g. "reactnative", "ios") self-corrects in one turn.
func TestSdkConventions_UnknownPlatformFailsNamingValid(t *testing.T) {
	_, err := sdkConventionsFor("swift")
	if err == nil {
		t.Fatal("unknown platform must error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "swift") || !strings.Contains(msg, "flutter") || !strings.Contains(msg, "react_native") {
		t.Fatalf("error must name the bad platform and the valid ones, got: %v", msg)
	}
}
