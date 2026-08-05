package templates

import (
	"strings"
	"testing"
)

// Resolution decides which code lands in a developer's repo. A subtle bug
// here installs the wrong template silently, so the properties that matter
// are pinned directly.

func catalog(entries ...Entry) *Catalog {
	return &Catalog{SchemaVersion: CatalogSchemaVersion, Templates: entries}
}

func entry(id, version string, opts ...func(*Entry)) Entry {
	e := Entry{
		ID:                id,
		Framework:         Flutter,
		Version:           version,
		BundleURL:         "templates/flutter/" + id + "/" + version + ".tar.gz",
		SHA256:            "deadbeef",
		Signature:         "sig",
		FrameworkVersions: ">=3.35 <4.0",
		SDKVersions:       ">=10.3 <11.0",
	}
	for _, o := range opts {
		o(&e)
	}
	return e
}

func deprecated(e *Entry)            { e.Deprecated = true }
func rn(e *Entry)                    { e.Framework = ReactNative }
func sdkRange(r string) func(*Entry) { return func(e *Entry) { e.SDKVersions = r } }
func fwRange(r string) func(*Entry)  { return func(e *Entry) { e.FrameworkVersions = r } }

func currentEnv() Environment {
	return Environment{FlutterVersion: "3.35.2", SDKVersion: "10.3.0"}
}

// --- schema gating ----------------------------------------------------------

// A catalog written to a newer schema must be REFUSED, not parsed
// optimistically: reading unknown structure as if it were understood is how a
// forward-compatible format silently does the wrong thing.
func TestParseCatalog_RefusesNewerSchema(t *testing.T) {
	_, err := ParseCatalog([]byte(`{"schema_version": 99, "templates": []}`))
	if err == nil {
		t.Fatal("a newer schema version was accepted")
	}
	if !strings.Contains(err.Error(), "99") {
		t.Fatalf("error should name the version it saw, got: %v", err)
	}
}

func TestParseCatalog_RefusesMissingSchemaVersion(t *testing.T) {
	if _, err := ParseCatalog([]byte(`{"templates": []}`)); err == nil {
		t.Fatal("a catalog without schema_version was accepted")
	}
}

// --- reference parsing ------------------------------------------------------

func TestParseRef(t *testing.T) {
	got, err := ParseRef("chat", Flutter)
	if err != nil || got.ID != "chat" || got.Version != "" {
		t.Fatalf("bare ref: got %+v err %v", got, err)
	}

	got, err = ParseRef("chat@1.2.0", Flutter)
	if err != nil || got.ID != "chat" || got.Version != "1.2.0" {
		t.Fatalf("pinned ref: got %+v err %v", got, err)
	}

	for _, bad := range []string{"", "   ", "@1.0.0", "chat@"} {
		if _, err := ParseRef(bad, Flutter); err == nil {
			t.Fatalf("%q should not parse", bad)
		}
	}
}

// --- resolution -------------------------------------------------------------

func TestResolve_PicksNewestCompatible(t *testing.T) {
	c := catalog(
		entry("chat", "1.0.0"),
		entry("chat", "1.10.0"), // must beat 1.9.0: numeric, not lexical
		entry("chat", "1.9.0"),
	)
	got, err := Resolve(c, Request{ID: "chat", Framework: Flutter}, currentEnv())
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if got.Version != "1.10.0" {
		t.Fatalf("want 1.10.0, got %s — versions must compare numerically", got.Version)
	}
}

// Frameworks share one catalog, so resolution MUST filter by framework or a
// Flutter project could be handed React Native source.
func TestResolve_FiltersByFramework(t *testing.T) {
	c := catalog(
		entry("chat", "2.0.0", rn),
		entry("chat", "1.0.0"),
	)
	got, err := Resolve(c, Request{ID: "chat", Framework: Flutter}, currentEnv())
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if got.Framework != Flutter || got.Version != "1.0.0" {
		t.Fatalf("resolved across frameworks: %+v", got)
	}
}

// A pinned version resolves to itself ALWAYS — including when deprecated.
// Withdrawing a version must not break a build that named it.
func TestResolve_PinnedVersionWinsEvenWhenDeprecated(t *testing.T) {
	c := catalog(
		entry("chat", "1.0.0", deprecated),
		entry("chat", "2.0.0"),
	)
	got, err := Resolve(c, Request{ID: "chat", Framework: Flutter, Version: "1.0.0"}, currentEnv())
	if err != nil {
		t.Fatalf("pinned resolve failed: %v", err)
	}
	if got.Version != "1.0.0" {
		t.Fatalf("pin ignored: got %s", got.Version)
	}
}

func TestResolve_SkipsDeprecatedWhenUnpinned(t *testing.T) {
	c := catalog(
		entry("chat", "2.0.0", deprecated),
		entry("chat", "1.0.0"),
	)
	got, err := Resolve(c, Request{ID: "chat", Framework: Flutter}, currentEnv())
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if got.Version != "1.0.0" {
		t.Fatalf("a deprecated version was chosen automatically: %s", got.Version)
	}
}

// The SDK constraint is what stops a template generating code against an SDK
// it was never built for — the failure would surface as a compile error in
// the developer's brand-new project.
func TestResolve_SkipsIncompatibleSDK(t *testing.T) {
	c := catalog(
		entry("chat", "3.0.0", sdkRange(">=11.0 <12.0")), // needs an SDK we lack
		entry("chat", "2.0.0"),
	)
	got, err := Resolve(c, Request{ID: "chat", Framework: Flutter}, currentEnv())
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if got.Version != "2.0.0" {
		t.Fatalf("an SDK-incompatible version was chosen: %s", got.Version)
	}
}

// When nothing fits, the error must say WHY each candidate was rejected —
// "no compatible version" alone leaves the developer with nothing to act on.
func TestResolve_NoCompatibleVersionExplainsWhy(t *testing.T) {
	c := catalog(entry("chat", "3.0.0", sdkRange(">=11.0 <12.0")))
	_, err := Resolve(c, Request{ID: "chat", Framework: Flutter}, currentEnv())
	if err == nil {
		t.Fatal("expected a compatibility error")
	}
	if !strings.Contains(err.Error(), "3.0.0") || !strings.Contains(err.Error(), "10.3.0") {
		t.Fatalf("error should name the rejected version and what was found, got: %v", err)
	}
}

func TestResolve_UnknownTemplate(t *testing.T) {
	_, err := Resolve(catalog(entry("chat", "1.0.0")),
		Request{ID: "nope", Framework: Flutter}, currentEnv())
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("expected a not-found error naming the template, got: %v", err)
	}
}

func TestResolve_UnknownPinnedVersionListsAvailable(t *testing.T) {
	c := catalog(entry("chat", "1.0.0"), entry("chat", "2.0.0"))
	_, err := Resolve(c, Request{ID: "chat", Framework: Flutter, Version: "9.9.9"}, currentEnv())
	if err == nil {
		t.Fatal("expected an error for an unpublished version")
	}
	if !strings.Contains(err.Error(), "1.0.0") || !strings.Contains(err.Error(), "2.0.0") {
		t.Fatalf("error should list what IS available, got: %v", err)
	}
}

// --- constraints ------------------------------------------------------------

func TestCheckConstraint(t *testing.T) {
	cases := []struct {
		have, constraint string
		ok               bool
	}{
		{"3.35.2", ">=3.35 <4.0", true},
		{"3.34.0", ">=3.35 <4.0", false},
		{"4.0.0", ">=3.35 <4.0", false},
		{"3.35.0", ">=3.35", true},
		{"10.3.0", ">=10.3 <11.0", true},
		{"11.0.0", ">=10.3 <11.0", false},
		{"3.35", ">=3.35.0", true}, // missing components count as zero
		{"3.35.0-beta.1", ">=3.35", true},
	}
	for _, c := range cases {
		err := checkConstraint("Flutter", c.have, c.constraint)
		if (err == nil) != c.ok {
			t.Fatalf("have=%s constraint=%q: want ok=%v, got err=%v",
				c.have, c.constraint, c.ok, err)
		}
	}
}

// An unknown version skips the check rather than failing it: refusing to
// scaffold because a version string could not be read is a worse outcome than
// installing a template that might want something newer.
func TestCheckConstraint_UnknownVersionSkips(t *testing.T) {
	if err := checkConstraint("Flutter", "", ">=99.0"); err != nil {
		t.Fatalf("an unknown version should skip the check, got: %v", err)
	}
	if err := checkConstraint("Flutter", "3.35.0", ""); err != nil {
		t.Fatalf("an absent constraint should pass, got: %v", err)
	}
}

// An operator this build does not understand must NOT silently pass — a newer
// catalog could use a vocabulary this binary lacks, and treating it as
// satisfied would install something unverified.
func TestCheckConstraint_UnknownOperatorFails(t *testing.T) {
	err := checkConstraint("Flutter", "3.35.0", "~>3.35")
	if err == nil {
		t.Fatal("an unsupported operator was treated as satisfied")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.10.0", "1.9.0", 1},
		{"1.9.0", "1.10.0", -1},
		{"2.0", "2.0.0", 0},
		{"10.3.0", "9.9.9", 1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Fatalf("compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
