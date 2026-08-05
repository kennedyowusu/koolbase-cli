package templates

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Template bundles may contain only regular files and directories. Symbolic
// links, hardlinks, devices, FIFOs, sockets, and unknown entry types are
// rejected outright.
//
// A template is a portable source tree, not a filesystem trick: anything a
// link might express can be a copied file or a later explicit build step.
// Supporting links buys nearly nothing and materially complicates the safety
// model across macOS, Linux, and Windows.
//
// This runs AFTER signature verification, so a hostile archive implies an
// already-compromised publishing pipeline — which is precisely the scenario
// worth surviving rather than assuming away.

// Extraction limits.
//
// The entry cap matters independently of the byte caps: an archive of
// hundreds of thousands of empty files consumes time and inodes without
// exceeding a single byte limit.
const (
	maxFileBytes  = 10 << 20 // 10 MB per regular file
	maxTotalBytes = 50 << 20 // 50 MB decompressed, total
	maxEntries    = 5000
)

var (
	// ErrUnsafeArchivePath is returned when an entry would write outside the
	// extraction root.
	ErrUnsafeArchivePath = errors.New("archive entry escapes the extraction directory")
	// ErrUnsafeArchiveEntry is returned for any entry that is not a regular
	// file or directory.
	ErrUnsafeArchiveEntry = errors.New("archive contains an entry type that is not allowed")
)

// ExtractBundle unpacks a verified tar.gz into dst, which must already exist
// and should be a fresh temporary directory on the same filesystem as the
// final cache location — that is what makes the later promotion rename
// atomic.
//
// Permissions from the archive are IGNORED. Directories are created 0755 and
// files written 0644: no executable bit, no setuid, no ownership, no
// timestamps. If a template ever needs an executable script, its manifest
// will say so — archive metadata never grants execution.
func ExtractBundle(bundlePath, dst string) error {
	f, err := os.Open(bundlePath)
	if err != nil {
		return fmt.Errorf("could not open bundle: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("bundle is not valid gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var (
		entries int
		total   int64
	)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("corrupt bundle: %w", err)
		}

		entries++
		if entries > maxEntries {
			return fmt.Errorf("bundle contains more than %d entries", maxEntries)
		}

		target, err := safeJoin(dst, hdr.Name)
		if err != nil {
			return fmt.Errorf("%w: %q", err, hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}

		case tar.TypeReg:
			if hdr.Size < 0 {
				return fmt.Errorf("bundle entry %q declares a negative size", hdr.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			written, err := writeLimited(tr, target, maxTotalBytes-total)
			if err != nil {
				return err
			}
			total += written

		default:
			// Symlinks, hardlinks, char/block devices, FIFOs, sockets, and
			// anything unrecognised.
			return fmt.Errorf("%w: %q (type %q)", ErrUnsafeArchiveEntry, hdr.Name, string(hdr.Typeflag))
		}
	}
}

// writeLimited copies at most the smaller of maxFileBytes and remaining, and
// counts what it ACTUALLY reads rather than trusting the tar header's
// declared size — the header is attacker-controlled, so a limit checked
// against it is decorative.
func writeLimited(r io.Reader, target string, remaining int64) (int64, error) {
	if remaining <= 0 {
		return 0, fmt.Errorf("bundle exceeds the %d byte decompressed limit", int64(maxTotalBytes))
	}
	limit := int64(maxFileBytes)
	if remaining < limit {
		limit = remaining
	}

	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer out.Close()

	// limit+1 so a file exactly at the cap is distinguishable from one that
	// exceeds it.
	written, err := io.Copy(out, io.LimitReader(r, limit+1))
	if err != nil {
		return written, err
	}
	if written > limit {
		return written, fmt.Errorf("bundle entry %q exceeds the size limit", filepath.Base(target))
	}
	return written, nil
}

// safeJoin resolves an archive entry name against the extraction root and
// guarantees the result stays inside it.
//
// Containment is checked with filepath.Rel rather than by scanning for "..":
// the relative-path check IS the invariant, while substring scanning misses
// cases and rejects legitimate names.
func safeJoin(root, name string) (string, error) {
	if name == "" {
		return "", ErrUnsafeArchivePath
	}

	// Tar paths are forward-slash by convention. Normalise backslashes too,
	// so a Windows-style separator inside an entry name cannot act as an
	// alternate traversal mechanism.
	clean := path.Clean(strings.ReplaceAll(name, `\`, "/"))

	// Absolute paths and UNC-style names are never acceptable.
	if strings.HasPrefix(clean, "/") || strings.HasPrefix(name, `\\`) {
		return "", ErrUnsafeArchivePath
	}
	// A drive-letter prefix (C:...) on any platform.
	if len(clean) >= 2 && clean[1] == ':' {
		return "", ErrUnsafeArchivePath
	}
	if clean == "." {
		return root, nil
	}

	target := filepath.Join(root, filepath.FromSlash(clean))

	rel, err := filepath.Rel(root, target)
	if err != nil {
		return "", ErrUnsafeArchivePath
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrUnsafeArchivePath
	}
	return target, nil
}
