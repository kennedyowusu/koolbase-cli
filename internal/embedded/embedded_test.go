package embedded_test

import (
	"testing"

	"github.com/kennedyowusu/koolbase-cli/cmd"
	"github.com/kennedyowusu/koolbase-cli/internal/embedded"
)

// THE staleness check.
//
// The templates compiled into this binary are exports from the
// koolbase-templates repository, not files anyone edits here. When they drift
// — because someone edited the embedded copy, or because the source moved
// ahead and nobody re-exported — this test fails.
//
// It has to be a test rather than a warning: the fallback path is the only
// thing that exercises the embedded copies, so drift is otherwise invisible
// until a user with no network gets a template that is months old.
func TestEmbeddedTemplatesAreCurrent(t *testing.T) {
	fsys := cmd.TemplatesFS()

	manifest, err := embedded.ParseManifest(fsys, "templates/"+embedded.ManifestName)
	if err != nil {
		t.Fatalf("%v", err)
	}

	if err := embedded.Verify(fsys, "templates", manifest); err != nil {
		t.Fatalf("%v", err)
	}
}

// A digest must actually change when content does — otherwise the test above
// would pass for a hasher that ignores its input.
func TestTreeDigestRespondsToContent(t *testing.T) {
	fsys := cmd.TemplatesFS()

	skeleton, err := embedded.TreeDigest(fsys, "templates/skeleton")
	if err != nil {
		t.Fatal(err)
	}
	auth, err := embedded.TreeDigest(fsys, "templates/features/auth")
	if err != nil {
		t.Fatal(err)
	}

	if skeleton == auth {
		t.Fatal("two different trees produced the same digest")
	}
	if len(skeleton) != 64 {
		t.Fatalf("digest is %d characters, expected a hex sha256", len(skeleton))
	}
}
