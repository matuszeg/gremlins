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
	"go/token"
	"strings"
)

// span is a range of lines a declaration occupied when the map was made, and
// how far it has moved since.
type span struct {
	start int
	end   int
	delta int
}

// reusable decides which of a cached package's mappings survive the change that
// moved its build ID, and returns them in the current line numbering.
//
// # The rule
//
// A package and every dependency it instruments are read as entities (see
// pkgPrint): each function, method, type spec, var spec and const group, and
// each file's import table. Comparing two prints, a change is one of three
// things.
//
//   - A function body. A test that did not execute the changed body cannot
//     behave differently on its account: any change that redirected its path
//     would itself have to be a change to a line it executed. The profile
//     records the package's lines a test executed and Deps the dependency
//     entities, so the tests whose record touches the body are dirty and no
//     others.
//   - A changed name: an entity added or removed, a type, var or const group
//     whose print changed, a function whose signature changed, a method added,
//     removed or re-signed (which changes its receiver type, and so the
//     interfaces it satisfies), and every name an import of the file now binds
//     differently. What follows from it is the closure below.
//   - Anything else, which re-maps the whole package, and every package that
//     has the changed one in scope (see the end of this comment).
//
// The closure starts from the changed names and adds every entity whose header
// mentions one, to a fixpoint. A header is what a name's meaning reaches
// through without being executed: a type spec, a var spec, a const group, a
// function's signature — and all of a test-file entity, because test files are
// never instrumented, so a helper's body is never in any record either. A test
// is then dirty when it executed — in its profile or its Deps — a body that
// changed or an entity whose text mentions an affected name, when its own
// declaration changed or mentions one, or when it reaches a test-file entity
// that does (which the closure has already made an affected name).
//
// # Why that is enough
//
// A test is affected by a change it did not execute only if it reaches the
// changed thing by name, or the change is to what runs before any test does.
// The routes by name are few: a value of a type built somewhere, a constant
// read, a function passed as a value — as huma.Register takes a handler and
// reflects over its signature — a type nested in another, an interface a type
// now satisfies. Each of those mentions the name somewhere a test's record
// sees: in code it executed (the profile or Deps), in a header the closure
// follows (a type nested in a type, a function returning one), or in the
// test's own source. A method declared through an alias belongs to the type
// the alias names, which code can name without naming the alias, so a local
// type's alias carries the type's name as well as its own. An added name is a
// changed name because it can rebind an existing mention — a new
// package-level `min` captures every `min(x, 10)` in the package without a
// byte of them changing — and it is the mention that is followed, so that case
// needs nothing of its own. Reflection inside a package that is not
// instrumented only ever sees types that instrumented code named.
//
// What runs before any test is the initialisation of every linked package,
// and it reaches every test without a name. Three things decide it, and each
// is held somewhere a change to it re-maps the package. What runs: an init or
// a var initialised by a call is an entity that re-maps on any change, and so
// is a var whose initialiser formats a value — fmt.Errorf calls an operand's
// String or Error method there and then, so it is allowed as a plain entity
// only over literals and errors errors.New made. Which packages are linked:
// every linked import path is in Inputs, the standard library's included,
// because a package newly linked — crypto/md5 registering MD5 with crypto —
// changes what untouched code does. And in what order: a package is
// initialised after everything it imports, so a file's set of imported paths
// changing can move it, and with it what an init or a var reading another
// package sees; when that happens and anything instrumented observes the
// order, the package is re-mapped.
//
// Matching is by name and deliberately over-approximate. Within one package
// directory — the package, its test files and its external test package — any
// identifier matches. Across packages only an exported name can be referred
// to, and it matches any identifier of that spelling, whatever the import
// alias; a dependency's unexported change reaches the rest of the binary only
// through its own exported entities, which the closure follows. Shadowing, a
// field or local of the same spelling, an unrelated package: each can only add
// dirty tests.
//
// # What still re-maps the whole package
//
// Everything the closure has no name to follow, wherever in the binary's
// in-scope packages it happens:
//
//   - a change to what the binary is built from that no profile records: a
//     dependency outside every main module, a main module's go.mod, the set of
//     packages linked, the toolchain, the build environment, the compile flags
//     (Inputs), or the set of instrumented dependencies itself;
//   - a file's set of imported paths changing, in any instrumented package,
//     while anything instrumented observes the order of initialisation: an
//     init, a var initialised by a call or by reading another package, or
//     TestMain;
//   - an init added, removed, changed, or mentioning an affected name; a var
//     whose initialiser calls something at initialisation, on the same terms;
//     TestMain on the same terms: each runs for every test;
//   - anything in Whole: a non-Go file or data below the directory (an
//     embedded file among them), a build constraint, a file named for a
//     platform or as a test file, a blank or dot import, a cgo file, a
//     directive outside every declaration, a file that does not parse;
//   - a declaration with no name to pair it by — a blank var, a key two
//     declarations share — changing, and one carrying //go:linkname;
//   - a kept block that falls in no unchanged entity, which means the profile
//     and the print disagree about the package.
//
// All of it fails in the safe direction: too many tests re-mapped, never too
// few. A mapping wrongly kept would mean a test that could kill a mutant is
// not run, so the mutant reports LIVED — a red result nobody can reproduce,
// which is why every case that is not clearly sound resolves to re-mapping.
//
// # What it cannot see
//
// A test's behaviour can hang on something no print holds, and then a kept
// mapping can be wrong. Each of these is a known exposure, shared with reusing
// a map by build ID alone: a file read at run time from outside the package
// directory; a file of the package's own source read at run time — a test
// that parses its package's .go files sees a change to any of them, where the
// closure sees only the entities that change names; a binary built and run by
// a test; lines executed in a subprocess; and a nondeterministic path that
// reuse freezes as whichever way it went when the mapping was made.
//
// It is deliberately not a call graph. The alternative — SSA plus
// reachability from each test root — is a much larger build, and it is
// defeated by reflection exactly where a package is most likely to use it.
//
// When nothing at all changed — every print and Inputs agree — every mapping
// is kept. The build ID still moved, because Go folds the checkout path into
// it, and the path decides nothing a test executes; that is the case of a
// cache restored into another runner's work directory. The mappings are still
// moved rather than kept as they are, because blank lines between
// declarations move a function without changing its print.
func reusable(cached cachedPackage, now fingerprint) (map[string]Profile, bool) {
	was := cached.Fingerprint
	// An entry written before the package was fingerprinted, or one whose
	// fingerprint could not be taken, says nothing about what changed.
	if was.Whole == "" {
		return nil, false
	}
	// Something the test binary is built from besides this package and its
	// instrumented dependencies moved, and its lines are in no record here,
	// so which mappings it reached cannot be worked out.
	if was.Inputs == "" || was.Inputs != now.Inputs || len(was.Deps) != len(now.Deps) {
		return nil, false
	}
	cl := &closure{exported: map[string]bool{}}
	own := cl.add(was.pkgPrint, now.pkgPrint)
	deps := map[string]*pkgDelta{}
	for importPath, before := range was.Deps {
		after, present := now.Deps[importPath]
		if !present {
			return nil, false
		}
		deps[importPath] = cl.add(before, after)
	}
	if cl.whole || !cl.close() || cl.reordered && cl.observesInit() {
		return nil, false
	}

	dirtyTests, tested := own.dirtyTests(cl)
	dirtyLines, moved := own.dirtyLines(cl)
	dirtyDeps := map[string]bool{}
	for importPath, d := range deps {
		d.dirtyKeys(cl, importPath, dirtyDeps)
	}

	kept := make(map[string]Profile, len(cached.Tests))
	for name, profile := range cached.Tests {
		// A mapping no test declaration accounts for is one this cannot say
		// anything about.
		if !tested[name] || dirtyTests[name] || touches(profile, dirtyLines) ||
			touchesAny(cached.Deps[name], dirtyDeps) {
			continue
		}
		shifted, ok := shift(profile, moved)
		if !ok {
			return nil, false
		}
		kept[name] = shifted
	}

	return kept, true
}

// closure is the set of affected names across every package a test binary
// instruments, and whether anything found so far re-maps the whole package.
type closure struct {
	pkgs []*pkgDelta
	// exported is every affected name that another package can refer to.
	exported map[string]bool
	whole    bool
	// reordered says some package's set of imported paths changed, which
	// can move when packages are initialised relative to each other even
	// though every one of them is linked before and after.
	reordered bool
}

// pkgDelta is one package's two prints and what the change did to it.
type pkgDelta struct {
	was, now pkgPrint
	// changed is every key whose own print changed, that was added or
	// removed, or that mentions a name its file's imports now bind
	// differently: its lines, or its key in Deps, are dirty whatever else
	// holds.
	changed map[string]bool
	// names is every affected name of the package.
	names map[string]bool
	// reached is every key whose names are already in names.
	reached map[string]bool
}

// add compares one package's prints, recording its changed names, and
// returns what it found. A change that is not attributable sets whole.
func (cl *closure) add(was, now pkgPrint) *pkgDelta {
	d := &pkgDelta{was: was, now: now, changed: map[string]bool{}, names: map[string]bool{}, reached: map[string]bool{}}
	cl.pkgs = append(cl.pkgs, d)
	if was.Whole != now.Whole {
		cl.whole = true

		return d
	}
	if !samePaths(was.Imports, now.Imports) {
		cl.reordered = true
	}
	rebound := reboundNames(was.Imports, now.Imports)
	keys := map[string]bool{}
	for key := range was.Decls {
		keys[key] = true
	}
	for key := range now.Decls {
		keys[key] = true
	}
	for key := range keys {
		before, had := was.Decls[key]
		after, has := now.Decls[key]
		// A dependency's entities carry no file, so its keys, which start
		// with one, are what names it.
		file := key[:strings.LastIndex(key, ":")]
		// The key carries what kind of entity it is — a var that starts
		// calling something at initialisation is another key — and a
		// directive is in the print, so the hash is all there is to compare.
		textChanged := !had || !has || before.Hash != after.Hash
		r := rebound[file]
		reboundText := has && r.reaches(after.text)
		if !textChanged && !reboundText {
			continue
		}
		if (had && wholeOnChange(before)) || (has && wholeOnChange(after)) {
			cl.whole = true

			return d
		}
		d.changed[key] = true
		// A body alone changed: attributed through the records of the tests
		// that executed it, and through nothing else.
		bodyOnly := had && has && (after.Kind == "" || after.Kind == kindMethod) && !isTestFile(file) &&
			before.Sig == after.Sig && !r.reaches(after.header)
		if !bodyOnly {
			cl.affect(d, key, before.Names, after.Names)
		}
	}

	return d
}

// close follows headers from the changed names to a fixpoint, reporting false
// when an entity that runs for every test turns out to mention one.
func (cl *closure) close() bool {
	for grew := true; grew; {
		grew = false
		for _, d := range cl.pkgs {
			for key, e := range d.now.Decls {
				if d.reached[key] || !cl.reaches(d, e.header) {
					continue
				}
				if e.Kind == kindInit || e.Kind == kindRun {
					return false
				}
				cl.affect(d, key, e.Names)
				grew = true
			}
		}
	}

	return true
}

// observesInit reports whether anything in the binary's instrumented packages
// can see the order packages are initialised in: an init, a var initialised by
// a call or by reading another package, or TestMain. Without any, an order
// that moved changes nothing a test executes.
func (cl *closure) observesInit() bool {
	for _, d := range cl.pkgs {
		for _, decls := range []map[string]declPrint{d.was.Decls, d.now.Decls} {
			for _, e := range decls {
				if e.Kind == kindInit || e.Kind == kindRun || e.Observes {
					return true
				}
			}
		}
	}

	return false
}

// samePaths reports whether two prints' files import the same packages, file
// by file, whatever names they bind them to. A package is initialised after
// everything it imports, so those sets are its place in the order. They are
// compared per file rather than per package because a directory's prints
// hold two packages, the package and its external tests, and a path one
// gains can be one the other already imports; a path moved from one file of
// a package to another only costs a re-map.
func samePaths(was, now map[string]map[string]string) bool {
	paths := func(table map[string]string) map[string]bool {
		out := map[string]bool{}
		for _, path := range table {
			out[path] = true
		}

		return out
	}
	files := map[string]bool{}
	for f := range was {
		files[f] = true
	}
	for f := range now {
		files[f] = true
	}
	for f := range files {
		before, after := paths(was[f]), paths(now[f])
		if len(before) != len(after) {
			return false
		}
		for path := range before {
			if !after[path] {
				return false
			}
		}
	}

	return true
}

// affect records a key's names as affected.
func (cl *closure) affect(d *pkgDelta, key string, names ...[]string) {
	d.reached[key] = true
	for _, list := range names {
		for _, name := range list {
			d.names[name] = true
			if token.IsExported(name) {
				cl.exported[name] = true
			}
		}
	}
}

// reaches reports whether identifiers of package d mention an affected name:
// one of d's own, or any package's exported one.
func (cl *closure) reaches(d *pkgDelta, idents map[string]bool) bool {
	for id := range idents {
		if d.names[id] || cl.exported[id] {
			return true
		}
	}

	return false
}

// dirtyTests is the tests whose own declaration changed or mentions an
// affected name, and every test a declaration accounts for.
func (d *pkgDelta) dirtyTests(cl *closure) (map[string]bool, map[string]bool) {
	dirty, tested := map[string]bool{}, map[string]bool{}
	for key, after := range d.now.Decls {
		if after.Test == "" {
			continue
		}
		tested[after.Test] = true
		if d.changed[key] || cl.reaches(d, after.text) {
			dirty[after.Test] = true
		}
	}
	for key, before := range d.was.Decls {
		if before.Test != "" && d.changed[key] {
			dirty[before.Test] = true
		}
	}

	return dirty, tested
}

// dirtyLines is, per file, the lines of the package's entities a test that
// executed them must be re-mapped for — changed, removed, or mentioning an
// affected name — and where every other entity has moved to.
func (d *pkgDelta) dirtyLines(cl *closure) (map[string][]span, map[string][]span) {
	dirty, moved := map[string][]span{}, map[string][]span{}
	for key, before := range d.was.Decls {
		if isTestFile(before.File) {
			continue
		}
		after, present := d.now.Decls[key]
		if d.changed[key] || !present || cl.reaches(d, after.text) {
			dirty[before.File] = append(dirty[before.File], span{start: before.Start, end: before.End})

			continue
		}
		moved[before.File] = append(moved[before.File],
			span{start: before.Start, end: before.End, delta: after.Start - before.Start})
	}

	return dirty, moved
}

// dirtyKeys adds a dependency's dirty entity keys — changed, removed, or
// mentioning an affected name — and, when there is any, the key a test
// records for executing the dependency somewhere it could not be placed.
func (d *pkgDelta) dirtyKeys(cl *closure, importPath string, into map[string]bool) {
	for key := range d.was.Decls {
		after, present := d.now.Decls[key]
		if d.changed[key] || !present || cl.reaches(d, after.text) {
			into[key] = true
			into[depWhole(importPath)] = true
		}
	}
}

// wholeOnChange reports whether a change to an entity re-maps the whole
// package: it runs for every test, it has no name to pair it by, or it binds
// a symbol of another package by //go:linkname.
func wholeOnChange(d declPrint) bool {
	return d.Kind == kindInit || d.Kind == kindRun || d.Kind == kindBlank || d.Linked
}

func isTestFile(file string) bool {
	return strings.HasSuffix(file, "_test.go")
}

// rebinding is what one file's import table change rebinds: a set of local
// names, or everything, when a name involved could not be resolved.
type rebinding struct {
	all   bool
	names map[string]bool
}

func (r rebinding) reaches(idents map[string]bool) bool {
	if r.all {
		return true
	}
	for name := range r.names {
		if idents[name] {
			return true
		}
	}

	return false
}

// reboundNames compares two prints' import tables, file by file, and returns
// the local names each file now binds differently: added, removed, or bound
// to another path. An added or removed name matters too — it can shadow, or
// stop shadowing, a predeclared identifier in the file. An unnamed import
// whose package name the listing did not give is recorded under "?" and its
// path, and if one of those is involved the whole file is rebound, because
// which name it binds is unknown.
func reboundNames(was, now map[string]map[string]string) map[string]rebinding {
	out := map[string]rebinding{}
	files := map[string]bool{}
	for f := range was {
		files[f] = true
	}
	for f := range now {
		files[f] = true
	}
	for f := range files {
		r := rebinding{names: map[string]bool{}}
		for _, pair := range [][2]map[string]string{{was[f], now[f]}, {now[f], was[f]}} {
			for name, path := range pair[0] {
				if other, ok := pair[1][name]; ok && other == path {
					continue
				}
				if strings.HasPrefix(name, "?") {
					r.all = true
				}
				r.names[name] = true
			}
		}
		if len(r.names) > 0 {
			out[f] = r
		}
	}

	return out
}

// touches reports whether a profile executed any line a changed declaration
// occupied.
func touches(profile Profile, dirty map[string][]span) bool {
	for file, blocks := range profile {
		for _, block := range blocks {
			for _, s := range dirty[file] {
				if block.StartLine <= s.end && block.EndLine >= s.start {
					return true
				}
			}
		}
	}

	return false
}

// shift translates a profile into the current line numbering.
//
// A kept profile records where its blocks were, and the declarations around
// them may have grown or shrunk since; leaving the old numbers in place would
// answer about the wrong lines. Every block of a kept profile is inside a
// declaration whose content is unchanged — a block anywhere else would have
// made the test dirty — so each one moves by exactly the distance its
// declaration moved.
//
// A block that lands in no unchanged declaration is not guessed at: it means
// the profile and the fingerprint disagree about the package, and the answer is
// to re-map it.
func shift(profile Profile, moved map[string][]span) (Profile, bool) {
	shifted := make(Profile, len(profile))
	for file, blocks := range profile {
		for _, block := range blocks {
			delta, ok := deltaFor(moved[file], block)
			if !ok {
				return nil, false
			}
			block.StartLine += delta
			block.EndLine += delta
			shifted[file] = append(shifted[file], block)
		}
	}

	return shifted, true
}

func deltaFor(spans []span, block Block) (int, bool) {
	for _, s := range spans {
		if block.StartLine >= s.start && block.EndLine <= s.end {
			return s.delta, true
		}
	}

	return 0, false
}
