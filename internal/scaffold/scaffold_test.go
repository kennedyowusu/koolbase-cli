package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// The engine's job is narrow and its failures are expensive: a config that
// renders without its public key produces an app that fails at runtime with
// no clue why. These tests pin the properties that prevent that.

func testVars() Vars {
	return Vars{
		AppName:         "demo_app",
		AppTitle:        "Demo App",
		OrgIdentifier:   "com.example",
		ProjectID:       "proj-123",
		PublicKey:       "pk_live_abc",
		EnvironmentSlug: "development",
		BaseURL:         "https://api.koolbase.com",
		Flavors:         false,
		SDKVersion:      "^10.3.0",
	}
}

func TestRender_SubstitutesVariables(t *testing.T) {
	src := fstest.MapFS{
		"tpl/lib/config.dart.tmpl": &fstest.MapFile{Data: []byte(
			"const projectId = '{% .ProjectID %}';\nconst publicKey = '{% .PublicKey %}';\n")},
	}
	dst := t.TempDir()

	if err := Render(src, "tpl", dst, testVars()); err != nil {
		t.Fatalf("render failed: %v", err)
	}

	got := readFile(t, filepath.Join(dst, "lib/config.dart"))
	if !strings.Contains(got, "proj-123") || !strings.Contains(got, "pk_live_abc") {
		t.Fatalf("variables not substituted:\n%s", got)
	}
	if strings.Contains(got, "{%") {
		t.Fatalf("template markers survived rendering:\n%s", got)
	}
}

// The .tmpl suffix is what marks a file for rendering, and it must be
// stripped — a generated project containing main.dart.tmpl does not compile.
func TestRender_StripsTemplateSuffix(t *testing.T) {
	src := fstest.MapFS{
		"tpl/lib/main.dart.tmpl": &fstest.MapFile{Data: []byte("void main() {}\n")},
	}
	dst := t.TempDir()

	if err := Render(src, "tpl", dst, testVars()); err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "lib/main.dart")); err != nil {
		t.Fatalf("expected lib/main.dart: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "lib/main.dart.tmpl")); err == nil {
		t.Fatal("the .tmpl file was written to the output tree")
	}
}

// Files without the suffix are copied byte-for-byte. This is what makes it
// safe to ship a file that legitimately contains {% — or a binary asset.
func TestRender_CopiesNonTemplatesVerbatim(t *testing.T) {
	body := "literal {% .NotAVariable %} stays\n"
	src := fstest.MapFS{
		"tpl/assets/notes.txt": &fstest.MapFile{Data: []byte(body)},
	}
	dst := t.TempDir()

	if err := Render(src, "tpl", dst, testVars()); err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if got := readFile(t, filepath.Join(dst, "assets/notes.txt")); got != body {
		t.Fatalf("verbatim copy altered the file:\ngot:  %q\nwant: %q", got, body)
	}
}

func TestRender_RendersPathVariables(t *testing.T) {
	src := fstest.MapFS{
		"tpl/lib/{% .AppName %}/thing.dart.tmpl": &fstest.MapFile{Data: []byte("// x\n")},
	}
	dst := t.TempDir()

	if err := Render(src, "tpl", dst, testVars()); err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "lib/demo_app/thing.dart")); err != nil {
		t.Fatalf("path variable not rendered: %v", err)
	}
}

// A template referencing something Vars does not have must FAIL, loudly, at
// generate time. The alternative — Go's default empty-string substitution —
// ships a project whose config silently lacks a key, and the developer debugs
// a runtime auth failure instead of reading an error.
func TestRender_UnknownVariableIsAnError(t *testing.T) {
	src := fstest.MapFS{
		"tpl/lib/bad.dart.tmpl": &fstest.MapFile{Data: []byte("{% .NoSuchField %}")},
	}
	dst := t.TempDir()

	err := Render(src, "tpl", dst, testVars())
	if err == nil {
		t.Fatal("an unknown template variable rendered silently; it must fail")
	}
	if !strings.Contains(err.Error(), "bad.dart") {
		t.Fatalf("error should name the offending template, got: %v", err)
	}
}

func TestRender_CreatesNestedDirectories(t *testing.T) {
	src := fstest.MapFS{
		"tpl/lib/features/auth/presentation/screens/login.dart.tmpl": &fstest.MapFile{
			Data: []byte("// {% .AppTitle %}\n")},
	}
	dst := t.TempDir()

	if err := Render(src, "tpl", dst, testVars()); err != nil {
		t.Fatalf("render failed: %v", err)
	}
	got := readFile(t, filepath.Join(dst, "lib/features/auth/presentation/screens/login.dart"))
	if !strings.Contains(got, "Demo App") {
		t.Fatalf("nested file not rendered:\n%s", got)
	}
}

// Booleans drive conditional blocks — flavors on/off is the first real use.
func TestRender_ConditionalOnFlavors(t *testing.T) {
	tpl := "{% if .Flavors %}MULTI{% else %}SINGLE{% end %}\n"
	src := fstest.MapFS{"tpl/x.txt.tmpl": &fstest.MapFile{Data: []byte(tpl)}}

	off := t.TempDir()
	if err := Render(src, "tpl", off, testVars()); err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if got := readFile(t, filepath.Join(off, "x.txt")); !strings.Contains(got, "SINGLE") {
		t.Fatalf("flavors=false should take the else branch, got %q", got)
	}

	v := testVars()
	v.Flavors = true
	on := t.TempDir()
	if err := Render(src, "tpl", on, v); err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if got := readFile(t, filepath.Join(on, "x.txt")); !strings.Contains(got, "MULTI") {
		t.Fatalf("flavors=true should take the if branch, got %q", got)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
