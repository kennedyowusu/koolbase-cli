package templates

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Extraction writes attacker-shaped input to disk. Each test below is a way
// an archive could reach outside its directory or exhaust the machine.

type tarEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
	mode     int64
	size     int64 // when non-zero, overrides len(body) in the header
}

func makeBundle(t *testing.T, entries ...tarEntry) string {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, e := range entries {
		flag := e.typeflag
		if flag == 0 {
			flag = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		size := int64(len(e.body))
		if e.size != 0 {
			size = e.size
		}
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     mode,
			Size:     size,
			Typeflag: flag,
			Linkname: e.linkname,
		}
		if flag == tar.TypeDir {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", e.name, err)
		}
		if flag == tar.TypeReg && e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractBundle_NormalTree(t *testing.T) {
	bundle := makeBundle(t,
		tarEntry{name: "lib/", typeflag: tar.TypeDir},
		tarEntry{name: "lib/main.dart", body: "void main() {}\n"},
		tarEntry{name: "pubspec.yaml", body: "name: demo\n"},
	)
	dst := t.TempDir()

	if err := ExtractBundle(bundle, dst); err != nil {
		t.Fatalf("a normal bundle failed to extract: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "lib/main.dart"))
	if err != nil || !strings.Contains(string(got), "void main") {
		t.Fatalf("file not extracted correctly: %v %q", err, got)
	}
}

// The classic: an entry that walks out of the destination.
func TestExtractBundle_RejectsPathTraversal(t *testing.T) {
	for _, name := range []string{
		"../escaped.txt",
		"lib/../../escaped.txt",
		"a/b/../../../escaped.txt",
	} {
		bundle := makeBundle(t, tarEntry{name: name, body: "x"})
		err := ExtractBundle(bundle, t.TempDir())
		if !errors.Is(err, ErrUnsafeArchivePath) {
			t.Fatalf("%q should be rejected as traversal, got: %v", name, err)
		}
	}
}

func TestExtractBundle_RejectsAbsolutePaths(t *testing.T) {
	for _, name := range []string{
		"/etc/passwd",
		`C:\Windows\system32\evil.dll`,
		`\\server\share\evil.dll`,
	} {
		bundle := makeBundle(t, tarEntry{name: name, body: "x"})
		err := ExtractBundle(bundle, t.TempDir())
		if !errors.Is(err, ErrUnsafeArchivePath) {
			t.Fatalf("%q should be rejected as absolute, got: %v", name, err)
		}
	}
}

// A backslash separator must not become an alternate traversal mechanism.
func TestExtractBundle_RejectsBackslashTraversal(t *testing.T) {
	bundle := makeBundle(t, tarEntry{name: `..\escaped.txt`, body: "x"})
	if err := ExtractBundle(bundle, t.TempDir()); !errors.Is(err, ErrUnsafeArchivePath) {
		t.Fatalf("backslash traversal should be rejected, got: %v", err)
	}
}

// Symlinks are rejected outright rather than target-validated: a template is
// a portable source tree, and a link pointing outside turns a later write
// into an arbitrary-file write.
func TestExtractBundle_RejectsSymlinks(t *testing.T) {
	bundle := makeBundle(t, tarEntry{
		name: "lib/evil", typeflag: tar.TypeSymlink, linkname: "/etc/passwd",
	})
	if err := ExtractBundle(bundle, t.TempDir()); !errors.Is(err, ErrUnsafeArchiveEntry) {
		t.Fatalf("symlinks must be rejected, got: %v", err)
	}
}

func TestExtractBundle_RejectsHardlinksAndSpecialFiles(t *testing.T) {
	cases := []struct {
		name string
		flag byte
	}{
		{"hardlink", tar.TypeLink},
		{"chardev", tar.TypeChar},
		{"blockdev", tar.TypeBlock},
		{"fifo", tar.TypeFifo},
	}
	for _, c := range cases {
		bundle := makeBundle(t, tarEntry{
			name: "lib/" + c.name, typeflag: c.flag, linkname: "target",
		})
		if err := ExtractBundle(bundle, t.TempDir()); !errors.Is(err, ErrUnsafeArchiveEntry) {
			t.Fatalf("%s should be rejected, got: %v", c.name, err)
		}
	}
}

// An entry-count cap matters independently of byte caps: many tiny files pass
// every size limit and still exhaust time and inodes.
func TestExtractBundle_RejectsTooManyEntries(t *testing.T) {
	entries := make([]tarEntry, 0, maxEntries+10)
	for i := 0; i < maxEntries+10; i++ {
		entries = append(entries, tarEntry{name: filepath.Join("f", itoa(i)), body: ""})
	}
	bundle := makeBundle(t, entries...)

	err := ExtractBundle(bundle, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "entries") {
		t.Fatalf("an over-large entry count should be rejected, got: %v", err)
	}
}

// The size limit must be enforced on bytes actually read, not on the tar
// header's declared size — the header is attacker-controlled, so a limit
// checked against it is decorative.
func TestExtractBundle_EnforcesSizeOnBytesRead(t *testing.T) {
	// Declare a tiny size, ship a body far larger than the per-file cap.
	big := strings.Repeat("A", maxFileBytes+1024)
	bundle := makeBundle(t, tarEntry{name: "big.bin", body: big, size: int64(len(big))})

	if err := ExtractBundle(bundle, t.TempDir()); err == nil {
		t.Fatal("a file over the per-file limit was extracted")
	}
}

// Archive permission bits are ignored entirely: no executable bit, no setuid.
// If a template ever needs an executable script, its manifest will say so.
func TestExtractBundle_IgnoresArchivePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows")
	}
	bundle := makeBundle(t, tarEntry{
		name: "script.sh", body: "#!/bin/sh\n", mode: 0o4777, // setuid + world-writable
	})
	dst := t.TempDir()

	if err := ExtractBundle(bundle, dst); err != nil {
		t.Fatalf("extract failed: %v", err)
	}

	info, err := os.Stat(filepath.Join(dst, "script.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Fatalf("archive permissions were honoured: got %o, want 644", perm)
	}
	if info.Mode()&os.ModeSetuid != 0 {
		t.Fatal("setuid bit survived extraction")
	}
}

func TestExtractBundle_RejectsCorruptArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not.tar.gz")
	if err := os.WriteFile(path, []byte("this is not a gzip stream"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ExtractBundle(path, t.TempDir()); err == nil {
		t.Fatal("a non-gzip file was accepted")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
