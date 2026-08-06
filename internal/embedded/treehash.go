package embedded

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
)

// treeHasher digests a set of files by path and content.
//
// THIS FILE IS DUPLICATED, BYTE FOR BYTE, from koolbase-templates'
// cmd/kbtpl/treehash.go (only the package name differs). It must stay
// identical: that repository hashes a directory, this one hashes an embed.FS,
// and the staleness check only works if both produce the same digest for the
// same files.
//
// Duplicated rather than shared through a module because the two repositories
// are separately released, and a shared module would couple their release
// cadences — the coupling that moving templates out of the binary was meant
// to remove. It is thirty lines and it never changes; if it ever must,
// changing it in one place and not the other makes every check fail loudly
// rather than silently pass.
type treeHasher struct{ h hash.Hash }

func newTreeHasher() *treeHasher { return &treeHasher{h: sha256.New()} }

// add folds one file in. Path and length are hashed alongside the content so
// that moving a file, or splitting one file into two with the same total
// bytes, changes the digest.
func (t *treeHasher) add(path string, content []byte) {
	fmt.Fprintf(t.h, "%s\x00%d\x00", path, len(content))
	t.h.Write(content)
}

func (t *treeHasher) sum() string { return hex.EncodeToString(t.h.Sum(nil)) }
