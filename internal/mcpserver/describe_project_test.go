package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"
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

// mustProject projects the payload and marshals the result, returning both the
// struct and its JSON form. Most assertions here are about what appears in the
// serialized output, since that is what actually reaches an agent.
func mustProject(t *testing.T, raw string) (describeProjectOut, string) {
	t.Helper()
	out, err := projectSnapshot([]byte(raw))
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
// satellites: owner_field only on scoped, mode/conditions only on conditional.
// An agent reading `{"kind":"scoped","owner_field":"created_by"}` has no room
// to generate an unscoped write.
func TestProjectSnapshot_RulesAsDataPerKind(t *testing.T) {
	out, _ := mustProject(t, snapshotWithEverything)

	if len(out.Collections) != 3 {
		t.Fatalf("expected 3 collections, got %d", len(out.Collections))
	}

	expenses := out.Collections[0]
	if expenses.Read.Kind != "authenticated" {
		t.Fatalf("expenses read kind: want authenticated, got %q", expenses.Read.Kind)
	}
	if expenses.Read.OwnerField != nil {
		t.Fatalf("owner_field must not ride on a non-scoped rule, got %q", *expenses.Read.OwnerField)
	}
	if expenses.Write.Kind != "scoped" {
		t.Fatalf("expenses write kind: want scoped, got %q", expenses.Write.Kind)
	}
	if expenses.Write.OwnerField == nil || *expenses.Write.OwnerField != "created_by" {
		t.Fatalf("scoped write must carry owner_field=created_by, got %v", expenses.Write.OwnerField)
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
