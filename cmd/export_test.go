package cmd

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A minimal export zip: main.dart, pubspec, a generated tree, a manifest
// with correct hashes.
func makeExport(t *testing.T, docID string, screens map[string]string, pubspecDeps string) string {
	t.Helper()
	gen := map[string]string{
		"lib/generated/koolbase_generated.dart": "// GENERATED\nexport 'app.dart';\n",
		"lib/generated/app.dart":                "// GENERATED\nclass App {}\n",
	}
	for name, body := range screens {
		gen["lib/generated/screens/"+name] = "// GENERATED\n" + body
	}
	var mf []manifestFile
	for p, b := range gen {
		sum := sha256.Sum256([]byte(b))
		mf = append(mf, manifestFile{Path: p, SHA256: hex.EncodeToString(sum[:])})
	}
	m := exportManifest{
		DocumentID: docID, DocumentName: "T", SchemaVersion: 3, ExporterVersion: 2,
		GeneratedRoots: []string{"lib/generated/"}, OwnedByDeveloper: []string{"lib/main.dart", "pubspec.yaml"},
		GeneratedFiles: mf,
	}
	mj, _ := json.Marshal(m)
	files := map[string]string{
		"lib/main.dart": "import 'generated/koolbase_generated.dart';\nvoid main() => koolbaseMain();\n",
		"pubspec.yaml":  "name: t\ndependencies:\n  flutter:\n    sdk: flutter\n" + pubspecDeps,
		manifestPath:    string(mj),
	}
	for p, b := range gen {
		files[p] = b
	}
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for p, b := range files {
		f, _ := w.Create(p)
		f.Write([]byte(b))
	}
	w.Close()
	path := filepath.Join(t.TempDir(), "export.zip")
	os.WriteFile(path, buf.Bytes(), 0o644)
	return path
}

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		return ""
	}
	return string(b)
}

func TestFreshApplyWritesEverything(t *testing.T) {
	dir := t.TempDir()
	z := makeExport(t, "doc_a", map[string]string{"home.dart": "class Home {}\n"}, "")
	var out bytes.Buffer
	if err := applyExport(z, dir, false, &out); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"lib/main.dart", "pubspec.yaml", "lib/generated/app.dart", "lib/generated/screens/home.dart", manifestPath} {
		if read(t, dir, p) == "" {
			t.Errorf("%s not written", p)
		}
	}
}

func TestReapplyNeverTouchesDeveloperFiles(t *testing.T) {
	dir := t.TempDir()
	z := makeExport(t, "doc_a", map[string]string{"home.dart": "class Home {}\n"}, "")
	applyExport(z, dir, false, &bytes.Buffer{})
	// The developer edits main.dart and adds a file.
	os.WriteFile(filepath.Join(dir, "lib/main.dart"), []byte("MINE\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "lib/app"), 0o755)
	os.WriteFile(filepath.Join(dir, "lib/app/mine.dart"), []byte("MINE TOO\n"), 0o644)
	z2 := makeExport(t, "doc_a", map[string]string{"home.dart": "class Home { int v = 2; }\n"}, "")
	if err := applyExport(z2, dir, false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if read(t, dir, "lib/main.dart") != "MINE\n" {
		t.Error("main.dart was overwritten")
	}
	if read(t, dir, "lib/app/mine.dart") != "MINE TOO\n" {
		t.Error("developer file was touched")
	}
	if !strings.Contains(read(t, dir, "lib/generated/screens/home.dart"), "v = 2") {
		t.Error("generated screen was not replaced")
	}
}

func TestHandEditsInGeneratedAreDetected(t *testing.T) {
	dir := t.TempDir()
	z := makeExport(t, "doc_a", map[string]string{"home.dart": "class Home {}\n"}, "")
	applyExport(z, dir, false, &bytes.Buffer{})
	os.WriteFile(filepath.Join(dir, "lib/generated/screens/home.dart"), []byte("// I edited this\n"), 0o644)
	var out bytes.Buffer
	err := applyExport(z, dir, false, &out)
	if err == nil {
		t.Fatal("expected apply to stop on hand edits")
	}
	if !strings.Contains(out.String(), "lib/generated/screens/home.dart") {
		t.Errorf("edited file not named:\n%s", out.String())
	}
	if read(t, dir, "lib/generated/screens/home.dart") != "// I edited this\n" {
		t.Error("edit was overwritten despite the stop")
	}
	// --force discards.
	if err := applyExport(z, dir, true, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, dir, "lib/generated/screens/home.dart"), "class Home") {
		t.Error("--force did not replace")
	}
}

func TestRemovedScreensAreRemoved(t *testing.T) {
	dir := t.TempDir()
	z := makeExport(t, "doc_a", map[string]string{"home.dart": "a\n", "checkout.dart": "b\n"}, "")
	applyExport(z, dir, false, &bytes.Buffer{})
	z2 := makeExport(t, "doc_a", map[string]string{"home.dart": "a\n"}, "")
	var out bytes.Buffer
	applyExport(z2, dir, false, &out)
	if _, err := os.Stat(filepath.Join(dir, "lib/generated/screens/checkout.dart")); !os.IsNotExist(err) {
		t.Error("checkout.dart should be gone")
	}
	if !strings.Contains(out.String(), "checkout.dart") {
		t.Errorf("removal not reported:\n%s", out.String())
	}
}

func TestPubspecDeltaIsReportedNotWritten(t *testing.T) {
	dir := t.TempDir()
	z := makeExport(t, "doc_a", map[string]string{"home.dart": "a\n"}, "")
	applyExport(z, dir, false, &bytes.Buffer{})
	before := read(t, dir, "pubspec.yaml")
	z2 := makeExport(t, "doc_a", map[string]string{"home.dart": "a\n"}, "  file_picker: ^8.0.0\n")
	var out bytes.Buffer
	applyExport(z2, dir, false, &out)
	if read(t, dir, "pubspec.yaml") != before {
		t.Error("pubspec.yaml was rewritten")
	}
	if !strings.Contains(out.String(), "file_picker: ^8.0.0") {
		t.Errorf("missing dependency not reported:\n%s", out.String())
	}
}

func TestDifferentDocumentRefuses(t *testing.T) {
	dir := t.TempDir()
	applyExport(makeExport(t, "doc_a", map[string]string{"h.dart": "a\n"}, ""), dir, false, &bytes.Buffer{})
	err := applyExport(makeExport(t, "doc_b", map[string]string{"h.dart": "a\n"}, ""), dir, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "different directory") {
		t.Fatalf("expected refusal for a different document, got %v", err)
	}
}

func TestTestDirectoryIsNotStripped(t *testing.T) {
	dir := t.TempDir()
	// A zip with a test/ tree, as the Designer emits.
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	gen := map[string]string{
		"lib/generated/app.dart":        "// GENERATED\n",
		"test/generated/home_test.dart": "// GENERATED\n",
	}
	var mf []manifestFile
	for p, b := range gen {
		sum := sha256.Sum256([]byte(b))
		mf = append(mf, manifestFile{Path: p, SHA256: hex.EncodeToString(sum[:])})
		f, _ := w.Create(p)
		f.Write([]byte(b))
	}
	m := exportManifest{DocumentID: "d", GeneratedRoots: []string{"lib/generated/", "test/generated/"}, GeneratedFiles: mf}
	mj, _ := json.Marshal(m)
	f, _ := w.Create(manifestPath)
	f.Write(mj)
	f, _ = w.Create("lib/main.dart")
	f.Write([]byte("void main() {}\n"))
	w.Close()
	z := filepath.Join(t.TempDir(), "e.zip")
	os.WriteFile(z, buf.Bytes(), 0o644)

	if err := applyExport(z, dir, false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if read(t, dir, "test/generated/home_test.dart") == "" {
		t.Error("test/generated/ was stripped or misplaced")
	}
	if _, err := os.Stat(filepath.Join(dir, "generated")); !os.IsNotExist(err) {
		t.Error("a stray top-level generated/ was created")
	}
}
