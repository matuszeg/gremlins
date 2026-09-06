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
// The rule is that a test's mapping is still valid if no changed line falls
// inside the profile it recorded. For a change within the mapped package this
// is sound rather than heuristic: a test whose executed lines are all unchanged
// cannot behave differently, because any change that redirected its path would
// itself have to be a change to a line it executed.
//
// It is deliberately not a call graph. The alternative — SSA plus reachability
// from each test root — is a much larger build, and it is defeated by
// reflection exactly where a package is most likely to use it.
//
// Two things this cannot see, both of which return false and re-map the whole
// package:
//
//   - A change outside the package. Without --cross-package a test binary is
//     built with -coverpkg for its own package alone, so no profile covers a
//     dependency and a change there looks clean to every test. The build ID
//     catches it, and a package whose own fingerprint is unchanged after its
//     build ID moved was changed from outside.
//   - A change outside a declaration. `const timeout = 5` becoming `10` changes
//     a line no coverage block contains, while the tests that execute the use
//     site do change behaviour. Everything that is not an attributable
//     declaration is in the shell, and the shell is all-or-nothing.
//
// Both fail in the safe direction: too many tests re-mapped, never too few. A
// mapping wrongly kept would mean a test that could kill a mutant is not run,
// so the mutant reports LIVED — a red result nobody can reproduce, which is why
// every case that is not clearly sound resolves to re-mapping the package.
func reusable(cached cachedPackage, now fingerprint) (map[string]Profile, bool) {
	was := cached.Fingerprint
	// An entry written before the package was fingerprinted, or one whose
	// fingerprint could not be taken, says nothing about what changed.
	if was.Shell == "" || was.Shell != now.Shell {
		return nil, false
	}
	// The build ID moved for a reason outside this package as well as, or
	// instead of, one inside it — and a dependency's lines are in no profile
	// here, so which mappings it reached cannot be worked out.
	if was.Inputs == "" || was.Inputs != now.Inputs {
		return nil, false
	}

	dirtyLines := map[string][]span{}
	dirtyTests := map[string]bool{}
	moved := map[string][]span{}
	// Whether anything in the package itself changed. If nothing did and the
	// build ID still moved, the change was in a dependency — which no profile
	// of this package covers, so nothing here can say which tests it reached.
	changed := false

	for key, before := range was.Decls {
		after, present := now.Decls[key]
		switch {
		case !present:
			// A removed method changes which interfaces its receiver satisfies,
			// and a removed init stops running for every test.
			if before.Kind != "" {
				return nil, false
			}
			changed = true
			markDirty(before, dirtyLines, dirtyTests)
		case after.Hash != before.Hash:
			// An init runs before every test in the binary, so its change is
			// not confined to the lines it occupies.
			if before.Kind == kindInit || after.Kind == kindInit {
				return nil, false
			}
			changed = true
			markDirty(before, dirtyLines, dirtyTests)
		case before.Test == "":
			moved[before.File] = append(moved[before.File],
				span{start: before.Start, end: before.End, delta: after.Start - before.Start})
		}
	}

	// The shell agreeing means every declaration it holds is unchanged, so they
	// are paired by position and contribute only where they have moved to. A
	// count that disagrees means the pairing would be a guess.
	for file, before := range was.Others {
		after := now.Others[file]
		if len(after) != len(before) {
			return nil, false
		}
		for i, decl := range before {
			moved[file] = append(moved[file],
				span{start: decl.Start, end: decl.End, delta: after[i].Start - decl.Start})
		}
	}

	// An added free function cannot change an existing path: reaching it takes
	// a call, and the call site is a change of its own. An added method or init
	// can, without any line changing where it is read.
	for key, after := range now.Decls {
		if _, had := was.Decls[key]; had {
			continue
		}
		if after.Kind != "" {
			return nil, false
		}
		changed = true
	}
	if !changed {
		return nil, false
	}

	kept := make(map[string]Profile, len(cached.Tests))
	for name, profile := range cached.Tests {
		if dirtyTests[name] || touches(profile, dirtyLines) {
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

// markDirty records what a changed declaration invalidates: its lines, or — for
// a test, whose file no profile covers — the mapping that bears its name.
func markDirty(decl declPrint, lines map[string][]span, tests map[string]bool) {
	if decl.Test != "" {
		tests[decl.Test] = true

		return
	}
	lines[decl.File] = append(lines[decl.File], span{start: decl.Start, end: decl.End})
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
