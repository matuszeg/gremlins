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
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// buildInputsOf hashes everything the package's test binary is built from
// except the package itself: the source of every dependency somebody could
// edit, plus the module's requirements and the toolchain.
//
// Per-test invalidation needs this, and the build ID cannot supply it. The
// build ID folds the package's own source together with every dependency's, so
// a moved build ID says something changed and never says where. That
// distinction is the whole of the difference between sound and nearly sound: a
// changed dependency can send a test down a path it did not take before, and no
// profile of this package records a dependency's lines, so the change looks
// clean to every mapping the cache holds. Narrowing on the package's own
// fingerprint while a dependency also moved would keep mappings that are stale,
// and a stale mapping means a test that could kill a mutant is never selected —
// the mutant reports LIVED and the gate goes red on something nobody can
// reproduce.
//
// So a run narrows only while this is unchanged, and re-maps the whole package
// the moment it is not.
//
// Directories are read rather than build IDs asked for: a dependency's identity
// according to Go means building it, and this has to be cheap enough to run
// before deciding whether there is work to skip. Measured at ~330ms for a
// package with 430 transitive dependencies, twelve of them in the module.
//
// What is left out is left out because something else in the hash pins it.
// GOROOT is pinned by the toolchain version. The module cache is pinned by
// go.sum, whose contents are here — and a dependency vendored or replaced into
// a local directory is not in the module cache, so it is read like any other
// source.
func (c *Coverage) buildInputsOf(pkg *testPackage) (string, bool) {
	env, ok := c.goEnv()
	if !ok {
		return "", false
	}
	dirs, ok := c.dependencyDirs(pkg)
	if !ok {
		return "", false
	}

	parts := []string{env.version}
	for _, name := range []string{"go.mod", "go.sum"} {
		// Absent is a fact about the module, not a failure: a module with no
		// go.sum has nothing pinned, and saying so consistently is enough.
		data, err := os.ReadFile(filepath.Join(c.mod.Root, name)) //nolint:gosec // G304: the module root Gremlins was pointed at
		if err != nil {
			parts = append(parts, name+"\x00absent")

			continue
		}
		parts = append(parts, name+"\x00"+hashOf(data))
	}
	for _, dir := range dirs {
		sum, dirOK := c.hashDir(dir)
		if !dirOK {
			return "", false
		}
		parts = append(parts, dir+"\x00"+sum)
	}

	return hashOf([]byte(strings.Join(parts, "\x00"))), true
}

// dependencyDirs is where the source of everything this package's tests link
// lives, minus the package's own directory and minus what the toolchain and the
// module cache already pin.
//
// The listing is of the test binary's dependencies, not the package's: a test
// file's imports are linked too, and a change in one of them moves the build ID
// exactly as any other dependency does.
func (c *Coverage) dependencyDirs(pkg *testPackage) ([]string, bool) {
	out, err := c.cmdContext("go", "list", "-deps", "-test", "-f", "{{.Dir}}", pkg.importPath).CombinedOutput()
	if err != nil {
		return nil, false
	}

	env, ok := c.goEnv()
	if !ok {
		return nil, false
	}
	own, err := filepath.Abs(pkg.dir)
	if err != nil {
		own = pkg.dir
	}

	seen := map[string]struct{}{}
	var dirs []string
	for _, line := range strings.Split(string(out), "\n") {
		dir := strings.TrimSpace(line)
		// `go list` writes build diagnostics to the same stream, and a
		// synthesised test package can report no directory at all.
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		if dir == own || under(dir, env.root) || under(dir, env.modCache) {
			continue
		}
		if _, dup := seen[dir]; dup {
			continue
		}
		seen[dir] = struct{}{}
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	return dirs, true
}

// hashDir hashes every regular file of a directory, by name and content.
//
// It is the whole directory rather than the Go files a build would select,
// because a build tag decides which of them that is and the tags can change
// between runs. Reading one extra file is cheaper than being wrong about which
// ones matter.
//
// Results are kept for the run: a module's packages share dependencies, and
// hashing one twice would be the cost of mapping a second package that happens
// to import the same thing.
func (c *Coverage) hashDir(dir string) (string, bool) {
	if sum, done := c.dirHashes[dir]; done {
		return sum, sum != ""
	}
	sum, ok := hashDirectory(dir)
	if c.dirHashes == nil {
		c.dirHashes = map[string]string{}
	}
	if !ok {
		c.dirHashes[dir] = ""

		return "", false
	}
	c.dirHashes[dir] = sum

	return sum, true
}

func hashDirectory(dir string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	parts := make([]string, 0, len(names))
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // G304: a directory `go list` reported as a dependency
		if err != nil {
			return "", false
		}
		parts = append(parts, name+"\x00"+hashOf(data))
	}

	return hashOf([]byte(strings.Join(parts, "\x00"))), true
}

// goEnvironment is the part of the Go environment that decides what a build
// produces without appearing in any source file.
type goEnvironment struct {
	root     string
	modCache string
	version  string
}

// goEnv asks the toolchain about itself, once per run.
func (c *Coverage) goEnv() (goEnvironment, bool) {
	if c.env != nil {
		return *c.env, c.env.version != ""
	}
	env := goEnvironment{}
	out, err := c.cmdContext("go", "env", "GOROOT", "GOMODCACHE", "GOVERSION").CombinedOutput()
	if err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) == 3 {
			env = goEnvironment{
				root:     strings.TrimSpace(lines[0]),
				modCache: strings.TrimSpace(lines[1]),
				version:  strings.TrimSpace(lines[2]),
			}
		}
	}
	c.env = &env

	return env, env.version != ""
}

// under reports whether a path is inside a directory, by path rather than by
// inode: this decides whether something is the toolchain's or the module
// cache's, and both answers only have to be conservative in one direction —
// a directory wrongly treated as editable is hashed, which costs nothing but
// the reading.
func under(path, dir string) bool {
	if dir == "" {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}

	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
