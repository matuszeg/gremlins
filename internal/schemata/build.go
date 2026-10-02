/*
 * Copyright 2026 The Gremlins Authors
 *
 *    Licensed under the Apache License, Version 2.0 (the "License");
 *    you may not use this file except in compliance with the License.
 *    You may obtain a copy of the License at
 *
 *        http://www.apache.org/licenses/LICENSE-2.0
 *
 *    Unless required by applicable law or agreed to in writing, software
 *    distributed under the License is distributed on an "AS IS" BASIS,
 *    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *    See the License for the specific language governing permissions and
 *    limitations under the License.
 */

package schemata

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-gremlins/gremlins/internal/engine/workdir"
)

// ErrNoTestBinary reports a package whose `go test -c` succeeded without
// writing a binary: it has no test files.
var ErrNoTestBinary = errors.New("schemata: package has no test binary (no test files)")

// schemaDirID is the dealer identifier of the schema copy.
const schemaDirID = "schemata"

// waitDelay bounds how long a killed `go test -c` may hold its output open
// through the compiler processes it started.
const waitDelay = 5 * time.Second

// Build is the schema copy of a module and the test binaries built from it.
type Build struct {
	// Dir is the copy of the module holding the rewritten files.
	Dir string
	// Binaries maps the import path of each package that built to its test
	// binary.
	Binaries map[string]string
	// Rewritten lists, sorted and relative to the module root, every file
	// the copy rewrites or adds.
	Rewritten []string
}

// BuildAll copies the module at modRoot into workDir, writes the rewritten
// files into the copy -- rewritten maps a package's import path to the files
// RewritePackage returned for it, keyed by absolute path under modRoot --
// and compiles the test binary of every package in testPkgs with `go test
// -c`, at most runtime.NumCPU() at a time, all within allowance. A package
// whose files cannot be written or whose binary does not build has its error
// in the returned map and no binary; the other packages keep theirs. If the
// module cannot be copied, every package has that error.
func BuildAll(ctx context.Context, modRoot, workDir, tags string, rewritten map[string]map[string][]byte,
	testPkgs []string, allowance time.Duration,
) (Build, map[string]error) {
	errs := map[string]error{}
	failAll := func(err error) (Build, map[string]error) {
		for _, p := range testPkgs {
			errs[p] = err
		}

		return Build{}, errs
	}
	dir, err := workdir.NewCachedDealer(workDir, modRoot).Get(schemaDirID)
	if err != nil {
		return failAll(fmt.Errorf("schemata: copy %s: %w", modRoot, err))
	}
	binDir, err := os.MkdirTemp(workDir, "schemata-bin-*")
	if err != nil {
		return failAll(fmt.Errorf("schemata: binary dir: %w", err))
	}

	b := Build{Dir: dir, Binaries: map[string]string{}}
	for _, p := range slices.Sorted(maps.Keys(rewritten)) {
		rels, err := writeFiles(modRoot, dir, rewritten[p])
		b.Rewritten = append(b.Rewritten, rels...)
		if err != nil {
			errs[p] = err
		}
	}
	slices.Sort(b.Rewritten)

	ctx, cancel := context.WithTimeout(ctx, allowance)
	defer cancel()
	names := binaryNames(testPkgs)
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	failed := maps.Clone(errs) // read below while the builds write errs
	for _, p := range testPkgs {
		if failed[p] != nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			bin := filepath.Join(binDir, names[p])
			err := buildTest(ctx, dir, bin, tags, p)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[p] = err

				return
			}
			b.Binaries[p] = bin
		}()
	}
	wg.Wait()

	return b, errs
}

// writeFiles writes files, keyed by absolute path under modRoot, to the same
// places under dir, and returns their paths relative to the module root.
func writeFiles(modRoot, dir string, files map[string][]byte) ([]string, error) {
	var rels []string
	for _, path := range slices.Sorted(maps.Keys(files)) {
		rel, err := filepath.Rel(modRoot, path)
		if err != nil || !filepath.IsAbs(path) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return rels, fmt.Errorf("schemata: %s is not a file under %s", path, modRoot)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), files[path], 0o600); err != nil {
			return rels, fmt.Errorf("schemata: write %s: %w", rel, err)
		}
		rels = append(rels, rel)
	}

	return rels, nil
}

// buildTest compiles the test binary of pkg, in the module copy dir, to bin.
func buildTest(ctx context.Context, dir, bin, tags, pkg string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("schemata: build %s: %w", pkg, err)
	}
	args := []string{"test", "-c", "-vet=off", "-o", bin}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, pkg)
	cmd := exec.CommandContext(ctx, "go", args...) //nolint:gosec // G204: a fixed tool, the package list is the caller's
	cmd.Dir = dir
	cmd.WaitDelay = waitDelay
	out, err := cmd.CombinedOutput()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("schemata: build %s: %w\n%s", pkg, ctxErr, out)
	}
	if err != nil {
		return fmt.Errorf("schemata: build %s: %w\n%s", pkg, err, out)
	}
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("%w: %s", ErrNoTestBinary, pkg)
	}

	return nil
}

// binaryNames gives each import path a distinct file name for its test
// binary: the path with every character outside [A-Za-z0-9._-] replaced by
// '_', suffixed with a counter when two paths sanitise alike.
func binaryNames(pkgs []string) map[string]string {
	sorted := slices.Clone(pkgs)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	names := make(map[string]string, len(sorted))
	used := map[string]bool{}
	for _, p := range sorted {
		base := strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
				return r
			}

			return '_'
		}, p)
		name := base + ".test"
		for i := 2; used[name]; i++ {
			name = base + "-" + strconv.Itoa(i) + ".test"
		}
		used[name] = true
		names[p] = name
	}

	return names
}
