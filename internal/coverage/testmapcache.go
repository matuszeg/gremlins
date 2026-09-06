/*
 * Copyright 2022 The Gremlins Authors
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

package coverage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cacheVersion is the shape of a cache file. A file written by a different
// version is discarded rather than migrated: it costs one rebuild, and the
// alternative is reading a map whose meaning has changed.
//
// Version 3 records the package's source fingerprint beside its mappings, so
// that a changed package can keep the mappings the change could not have
// touched. Version 2 was one file per package. Version 1 was one file per
// module, which meant a run had to write back every package it had not looked
// at or lose them — and a scoped run, which is the recommended workflow, looks
// at one.
const cacheVersion = 3

// cachedPackage is one package's mapping, and the build ID of the test binary
// it was produced from.
//
// The build ID is the key, and Go computes it over the package's own source AND
// every dependency's, transitively. So an unchanged build ID means nothing the
// package's tests execute has changed, and the coverage they produced cannot
// have changed either. Touching a package three levels down invalidates exactly
// the binaries that link it, without Gremlins doing any dependency analysis of
// its own.
//
// What the build ID cannot see is state outside the build: a test whose
// coverage depends on a database, a clock, or the network can map differently
// on two runs of the same binary. That is the same non-determinism the map has
// without a cache, held for longer.
//
// The build ID being coarse is why Fingerprint is here as well: it records what
// the package's source looked like when the mappings were made, so that a run
// whose build ID has moved can still ask which of them the change reached. An
// entry may have none — a package whose directory could not be read — in which
// case a changed build ID re-maps the whole package, as it always did.
//
// ImportPath is stored as well as hashed into the file name, so that a file
// found under the wrong name is a miss rather than another package's answer.
type cachedPackage struct {
	Tests       map[string]Profile `json:"tests"`
	ImportPath  string             `json:"import_path"`
	BuildID     string             `json:"build_id"`
	Fingerprint fingerprint        `json:"fingerprint"`
	Version     int                `json:"version"`
}

// cacheKey covers what changes the meaning of every entry at once rather than
// per package: what the mappings were measured against, and the build tags that
// decide which files exist at all.
//
// It names a directory rather than living inside the files, so that a
// --cross-package map and a narrow one cannot be mistaken for one another even
// though they describe the same packages.
func cacheKey(scope, buildTags string) string {
	sum := sha256.Sum256([]byte(scope + "\x00" + buildTags))

	return hex.EncodeToString(sum[:])
}

// perPackageScope stands for "each test binary was instrumented over its own
// package", which is what happens without --cross-package. It is not a package
// pattern and cannot collide with one: a --coverpkg value carrying a NUL cannot
// be typed on a command line or written in a config file.
const perPackageScope = "\x00per-package"

// cacheScope is what the mappings under one key were measured against, and so
// what an entry has to agree with to be readable.
//
// It is exactly the -coverpkg each test binary was compiled with, which is what
// decides whose lines can appear in a profile — hence the delegation, so the two
// cannot drift apart. Without --cross-package that is the package's own import
// path, and the answer here collapses to a constant: every package's mapping is
// then a function of that package alone.
//
// What is deliberately NOT here is how much of the module was scanned. It
// decides which packages get mapped and nothing about what a mapping says, so a
// run scoped to one package reads the file a whole-module run wrote for it
// rather than starting empty.
func (c *Coverage) cacheScope() string {
	return c.testMapCoverPkg(perPackageScope)
}

// cacheDirPath is where this module's per-package map files live. It is outside
// the module, under the user's cache directory unless a caller names another,
// so that a checkout stays clean and two checkouts of the same module do not
// share a map.
func (c *Coverage) cacheDirPath(key string) (string, error) {
	base := c.cacheDir
	if base == "" {
		var err error
		base, err = os.UserCacheDir()
		if err != nil {
			return "", err
		}
	}
	modName, modRoot := c.mod.Name, c.mod.Root
	root, err := filepath.Abs(modRoot)
	if err != nil {
		root = modRoot
	}
	sum := sha256.Sum256([]byte(modName + "\x00" + root))

	return filepath.Join(base, "gremlins", "testmap", hex.EncodeToString(sum[:]), key), nil
}

// cacheFilePath is the file holding one package's mapping. The import path is
// hashed rather than used directly: it contains separators, and on a
// case-insensitive filesystem two distinct import paths can name one file.
func cacheFilePath(dir, importPath string) string {
	sum := sha256.Sum256([]byte(importPath))

	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json")
}

// loadCachedPackage reads one package's mapping, reporting whether there is a
// usable one.
//
// Every failure reads as a miss rather than an error: a missing, unreadable,
// corrupt, or stale-versioned file costs a re-map of that package, which is
// exactly what happens without a cache at all. There is no failure here worth
// stopping a run for.
//
// A package that has gone away is never asked about — reads are keyed by an
// import path taken from the current `go list` — so its file is dead disk
// rather than a stale answer, and reclaiming it is housekeeping, not
// correctness.
func loadCachedPackage(dir, importPath string) (cachedPackage, bool) {
	data, err := os.ReadFile(cacheFilePath(dir, importPath))
	if err != nil {
		return cachedPackage{}, false
	}
	var p cachedPackage
	if err := json.Unmarshal(data, &p); err != nil {
		return cachedPackage{}, false
	}
	if p.Version != cacheVersion || p.ImportPath != importPath || p.Tests == nil || p.BuildID == "" {
		return cachedPackage{}, false
	}

	return p, true
}

// save writes one package's mapping through a temporary file in the same
// directory, so that an interrupted run leaves the previous file rather than a
// half-written one.
//
// A run writes only the packages it mapped. It has no reason to touch another
// package's file, so a scoped run cannot evict the rest of the module's map,
// and two runs scoped to different packages cannot clobber each other.
func (p cachedPackage) save(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "testmap-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)

		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)

		return err
	}

	return os.Rename(name, cacheFilePath(dir, p.ImportPath))
}

// buildID asks Go for the identity of a compiled binary. It is the hash Go
// itself uses to decide whether a build is up to date, which is precisely the
// question the cache needs answered.
func (c *Coverage) buildID(binary string) (string, error) {
	out, err := c.cmdContext("go", "tool", "buildid", binary).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w\n%s", err, out)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fmt.Errorf("go tool buildid returned nothing for %s", binary)
	}

	return id, nil
}
