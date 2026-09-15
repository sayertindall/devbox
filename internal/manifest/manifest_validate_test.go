// Validate-path tests: the rules a manifest must satisfy on its own, whether it
// was built locally or arrived from elsewhere.
package manifest

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestValidateRejectsManifestBytesMismatch(t *testing.T) {
	root := t.TempDir()
	write(t, root, "app.go", "package app\n", 0o644)
	write(t, root, "sub/keep.txt", "keep\n", 0o644)
	symlink(t, root, "alias.go", "app.go")
	good := mustBuild(t, root)

	var sum int64
	for _, e := range good.Entries {
		if e.Kind == KindFile {
			sum += e.Size
		}
	}
	if good.Bytes != sum {
		t.Fatalf("Build produced Bytes = %d, want the sum of file sizes %d", good.Bytes, sum)
	}

	for _, tc := range []struct {
		name  string
		bytes int64
	}{
		{"inflated quota", sum + 1<<30},
		{"deflated quota", sum - 1},
		{"zero quota with declared files", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := good
			bad.Entries = slices.Clone(good.Entries)
			bad.Bytes = tc.bytes
			bad.SHA256 = ""

			err := bad.Validate()
			if err == nil || !strings.Contains(err.Error(), "byte total") {
				t.Fatalf("Validate error = %v, want a byte-total error", err)
			}
			if _, err := Encode(bad); err == nil {
				t.Fatal("Encode accepted a forged byte total")
			}
			if err := Compare(bad, good); err == nil {
				t.Fatal("Compare accepted a forged byte total")
			}
			if err := Materialize(root, bad, openDest(t, filepath.Join(t.TempDir(), "dst"))); err == nil {
				t.Fatal("Materialize accepted a forged byte total")
			}
		})
	}

	t.Run("digest recomputed over the forged quota", func(t *testing.T) {
		// An external forger controls the whole manifest and can recompute a
		// self-consistent digest over the wire bytes, so the digest must not
		// launder an invalid quota. Encode itself already refuses this manifest,
		// which is why the digest is computed over the raw canonical form here.
		bad := good
		bad.Entries = slices.Clone(good.Entries)
		bad.Bytes = sum + 4096
		if _, err := Encode(bad); err == nil {
			t.Fatal("Encode serialized a forged byte total")
		}
		raw, err := json.Marshal(canonical{Version: bad.Version, Bytes: bad.Bytes, Entries: bad.Entries})
		if err != nil {
			t.Fatalf("marshal canonical: %v", err)
		}
		bad.SHA256 = digest(string(raw))

		validateErr := bad.Validate()
		if validateErr == nil || !strings.Contains(validateErr.Error(), "byte total") {
			t.Fatalf("Validate error = %v, want a byte-total error", validateErr)
		}
		if err := Materialize(root, bad, openDest(t, filepath.Join(t.TempDir(), "dst"))); err == nil {
			t.Fatal("Materialize accepted a self-consistent forged quota")
		}
	})

	t.Run("overflowing entry sizes cannot forge a small quota", func(t *testing.T) {
		// Two maximal sizes wrap the running total negative, and a third entry
		// brings it back to a small positive number. Without an overflow guard
		// the manifest would authenticate a 2-byte quota while declaring roughly
		// eighteen exabytes of content.
		bad := Manifest{Version: Version, Bytes: 2, Entries: []Entry{
			{Path: "a.bin", Kind: KindFile, Size: math.MaxInt64, SHA256: digest("a")},
			{Path: "b.bin", Kind: KindFile, Size: math.MaxInt64, SHA256: digest("b")},
			{Path: "c.bin", Kind: KindFile, Size: 4, SHA256: digest("c")},
		}}
		var wrapped int64
		for _, e := range bad.Entries {
			wrapped += e.Size
		}
		if wrapped != bad.Bytes {
			t.Fatalf("test premise broken: wrapped sum is %d, want %d", wrapped, bad.Bytes)
		}

		err := bad.Validate()
		if err == nil || !strings.Contains(err.Error(), "overflow") {
			t.Fatalf("Validate error = %v, want an overflow error", err)
		}
		if _, err := Encode(bad); err == nil {
			t.Fatal("Encode accepted an overflowing byte total")
		}
		if err := Materialize(root, bad, openDest(t, filepath.Join(t.TempDir(), "dst"))); err == nil {
			t.Fatal("Materialize accepted an overflowing byte total")
		}
	})
}

func TestValidateRejectsNegativeManifestBytes(t *testing.T) {
	root := t.TempDir()
	write(t, root, "app.go", "package app\n", 0o644)
	good := mustBuild(t, root)

	for _, bytes := range []int64{-1, -good.Bytes - 1, -1 << 40} {
		t.Run(fmt.Sprint(bytes), func(t *testing.T) {
			bad := good
			bad.Entries = slices.Clone(good.Entries)
			bad.Bytes = bytes
			bad.SHA256 = ""

			err := bad.Validate()
			if err == nil || !strings.Contains(err.Error(), "negative") {
				t.Fatalf("Validate error = %v, want a negative-byte-total error", err)
			}
			if _, err := Encode(bad); err == nil {
				t.Fatal("Encode accepted a negative byte total")
			}
			if err := Materialize(root, bad, openDest(t, filepath.Join(t.TempDir(), "dst"))); err == nil {
				t.Fatal("Materialize accepted a negative byte total")
			}
		})
	}

	t.Run("empty manifest with zero bytes is valid", func(t *testing.T) {
		empty := Manifest{Version: Version, Entries: []Entry{}, Bytes: 0}
		if err := empty.Validate(); err != nil {
			t.Fatalf("Validate rejected an empty manifest: %v", err)
		}
	})
}

// caseVariantManifestEntries are entries an external manifest could declare to
// smuggle an excluded path past a case-sensitive comparison.
func caseVariantManifestEntries() []Entry {
	return []Entry{
		{Path: ".ENV", Kind: KindFile, Size: 1, SHA256: digest("x")},
		{Path: ".Env.local", Kind: KindFile, Size: 1, SHA256: digest("x")},
		{Path: ".SSH/id_ed25519", Kind: KindFile, Size: 1, SHA256: digest("x")},
		{Path: "sub/.GIT/config", Kind: KindFile, Size: 1, SHA256: digest("x")},
		{Path: "NODE_MODULES/pkg/index.js", Kind: KindFile, Size: 1, SHA256: digest("x")},
		{Path: "Dist/app.js", Kind: KindFile, Size: 1, SHA256: digest("x")},
		{Path: "leak", Kind: KindSymlink, Target: ".ENV"},
		{Path: "deeplink", Kind: KindSymlink, Target: "sub/.Git/config"},
	}
}

func TestValidateRejectsCaseVariantExcludedPath(t *testing.T) {
	for _, entry := range caseVariantManifestEntries() {
		t.Run(entry.Path+"->"+entry.Target, func(t *testing.T) {
			m := Manifest{Version: Version, Entries: []Entry{entry}, Bytes: entry.Size}
			err := m.Validate()
			if err == nil {
				t.Fatalf("Validate accepted case-variant excluded entry %+v", entry)
			}
			if !strings.Contains(err.Error(), "excluded") {
				t.Fatalf("error = %v, want an exclusion error", err)
			}
		})
	}
}

func TestCompareValidatesBothManifests(t *testing.T) {
	// Compare used to check only quota metadata, so two byte-identical
	// projections that broke the same path rule compared equal. Comparing two
	// projections is only meaningful once each one is well-formed on its own.
	malformed := Manifest{Version: Version, Bytes: 1, Entries: []Entry{
		{Path: ".ssh/id_ed25519", Kind: KindFile, Size: 1, SHA256: digest("x")},
	}}
	if err := malformed.Validate(); err == nil {
		t.Fatal("test premise broken: the manifest is not malformed")
	}

	err := Compare(malformed, malformed)
	if err == nil {
		t.Fatal("Compare accepted two identically malformed manifests")
	}
	if !strings.Contains(err.Error(), "declared manifest") || !strings.Contains(err.Error(), "excluded") {
		t.Fatalf("error = %v, want it to blame the declared manifest for the excluded path", err)
	}

	// A malformed actual projection is refused against a valid declaration too.
	root := t.TempDir()
	write(t, root, "app.go", "package app\n", 0o644)
	good := mustBuild(t, root)
	err = Compare(good, malformed)
	if err == nil || !strings.Contains(err.Error(), "actual manifest") {
		t.Fatalf("error = %v, want it to blame the actual manifest", err)
	}
	if err := Compare(good, good); err != nil {
		t.Fatalf("Compare rejected a valid identical pair: %v", err)
	}
}
