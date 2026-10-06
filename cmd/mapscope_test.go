package cmd

import (
	"testing"

	"github.com/go-gremlins/gremlins/internal/diff"
	"github.com/go-gremlins/gremlins/internal/gomodule"
)

const scopeModule = "example.com"

func TestMapScope(t *testing.T) {
	mod := gomodule.GoModule{Name: scopeModule, Root: ".", CallingDir: "."}
	noDependents := func(string) []string { return nil }

	t.Run("no diff maps everything", func(t *testing.T) {
		if mapScope(mod, nil, noDependents) != nil {
			t.Error("want no scope when the run has no diff")
		}
	})

	// Mutants live only in changed production files, so only their packages
	// need a map; a changed test or a non-Go file holds none.
	changes := diff.Diff{
		"internal/api/handler.go":      nil,
		"internal/api/handler_test.go": nil,
		"internal/store/store_test.go": nil,
		"main.go":                      nil,
		"README.md":                    nil,
	}

	t.Run("the packages of the changed production files", func(t *testing.T) {
		in := mapScope(mod, changes, noDependents)
		for pkg, want := range map[string]bool{
			"example.com/internal/api":   true,
			scopeModule:                  true,
			"example.com/internal/store": false,
			"example.com/internal/other": false,
		} {
			if got := in(pkg); got != want {
				t.Errorf("in(%q) = %v, want %v", pkg, got, want)
			}
		}
	})

	// Diff paths are relative to the directory gremlins was pointed at.
	t.Run("from a calling directory below the module root", func(t *testing.T) {
		sub := gomodule.GoModule{Name: scopeModule, Root: ".", CallingDir: "internal"}
		in := mapScope(sub, diff.Diff{"api/handler.go": nil}, noDependents)
		if !in("example.com/internal/api") || in("example.com/api") {
			t.Error("want the import path to include the calling directory")
		}
	})

	// With --cross-package a dependent's tests are selected for the mutant,
	// and selection reads them from the map, so the dependents need mapping too.
	t.Run("with dependents, theirs too", func(t *testing.T) {
		dependents := func(pkg string) []string {
			if pkg == "example.com/internal/api" {
				return []string{"example.com/cmd/server"}
			}

			return nil
		}
		in := mapScope(mod, changes, dependents)
		if !in("example.com/cmd/server") {
			t.Error("want the changed package's dependents in scope")
		}
		if in("example.com/internal/store") {
			t.Error("want packages that are neither changed nor dependents out of scope")
		}
	})
}
