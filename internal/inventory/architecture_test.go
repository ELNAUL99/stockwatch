package inventory_test

import (
	"go/build"
	"strings"
	"testing"
)

// TestDomainDependsOnNothingButStdlib enforces the layering rule as a test
// rather than as a comment nobody re-reads.
//
// The domain package defines the interfaces it needs and storage implements
// them, so the arrow points inward: storage -> inventory, never the reverse.
// Go already rejects an import *cycle* at compile time, but it would happily let
// inventory import storage one-directionally — which is exactly the mistake this
// catches. A stray "just this once" import of storage or httpapi fails CI here.
//
// Note the package clause: inventory_test, not inventory. Go allows a test file
// to sit in an external test package in the same directory, which gives the test
// the same view of the package an outside caller has. Using it here is
// deliberate — importing the package under test would itself add an import edge.
func TestDomainDependsOnNothingButStdlib(t *testing.T) {
	const domainPkg = "github.com/ELNAUL99/stockwatch/internal/inventory"

	pkg, err := build.Import(domainPkg, "", 0)
	if err != nil {
		t.Fatalf("import %s: %v", domainPkg, err)
	}

	// Non-test imports only. Test files may import whatever they need.
	for _, imported := range pkg.Imports {
		t.Run(imported, func(t *testing.T) {
			if strings.HasPrefix(imported, "github.com/ELNAUL99/stockwatch/") {
				t.Errorf("domain imports internal package %q; the domain must not "+
					"depend on storage, httpapi, or any other layer", imported)
			}
			// A stdlib path has no dot in its first element. Anything else is a
			// third-party module, which the domain must also stay clear of —
			// pgx belongs in storage.
			if first, _, _ := strings.Cut(imported, "/"); strings.Contains(first, ".") {
				t.Errorf("domain imports third-party package %q; keep the domain "+
					"on the standard library", imported)
			}
		})
	}
}
