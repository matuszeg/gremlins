//go:build linux

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

package engine_test

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/go-gremlins/gremlins/internal/configuration"
	"github.com/go-gremlins/gremlins/internal/coverage"
	"github.com/go-gremlins/gremlins/internal/gomodule"
	"github.com/go-gremlins/gremlins/internal/log"
	"github.com/go-gremlins/gremlins/internal/mutator"
)

// TestTestMemoryLimit runs the engine over testdata/memlimit without and with
// --schemata under three limits, and expects the limit to reach the test
// processes and nothing else.
//
// Under a three-GiB limit the runaway loop's INCREMENT_DECREMENT mutant dies
// of the limit and is KILLED, as it was under `ulimit -v` around the whole of
// gremlins; so is the mutant that allocates nearly four GiB and passes. Under
// eight GiB the second LIVES, which shows the limit is what killed it; the
// runaway one is left out of that run, so that it never runs under a limit
// it could take seconds to reach. Under 300 MiB the go command itself cannot
// start, every mutant is NOT VIABLE, and the run warns once that the limit
// did it. A KILLED mutant is no reason to warn, so the three-GiB run does not.
// Both paths agree on every mutant, and gremlins' own address-space limit is
// the same after each run as before it.
//
// The limited run's cap leaves the toolchain room. It was one GiB, around a
// two-GiB mutant, and under load the go command, compiler or linker sometimes
// needed more than that (seen at 1.5 GiB too): a build died NOT VIABLE on one
// path, or a test binary before reaching its mutant (ERRORED) on the other,
// and the two paths disagreed. The fixture now spans four GiB so the cap can
// be three.
//
// Every fixture process runs under a limit: the runaway mutant must never run
// without one.
//
// It is not parallel: it sets the global configuration and the global log.
func TestTestMemoryLimit(t *testing.T) {
	modRoot, err := filepath.Abs("testdata/memlimit")
	if err != nil {
		t.Fatal(err)
	}
	mod := gomodule.GoModule{Name: "memlimit", Root: modRoot, CallingDir: "."}
	prof := parityProfile(t, mod)
	runaway := site{file: "runaway/runaway.go", prefix: "\tfor i := 0", mutType: mutator.IncrementDecrement}
	spare := site{file: "spare/spare.go", prefix: "\tbuf := make", mutType: mutator.ArithmeticBase}

	uncovered := maps.Clone(prof)
	delete(uncovered, runaway.file)
	testCases := map[string]struct {
		settings     map[string]any
		profile      coverage.Profile
		wantRunaway  mutator.Status
		wantSpare    mutator.Status
		wantWarnings int
	}{
		"limited": {
			settings:    map[string]any{configuration.UnleashTestMemoryLimitKey: "3G"},
			profile:     prof,
			wantRunaway: mutator.Killed,
			wantSpare:   mutator.Killed,
		},
		"generous": {
			settings:    map[string]any{configuration.UnleashTestMemoryLimitKey: "8G"},
			profile:     uncovered,
			wantRunaway: mutator.NotCovered,
			wantSpare:   mutator.Lived,
		},
		"too_small": {
			settings:     map[string]any{configuration.UnleashTestMemoryLimitKey: "300M"},
			profile:      prof,
			wantRunaway:  mutator.NotViable,
			wantSpare:    mutator.NotViable,
			wantWarnings: 1,
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			var byPath [2]map[string]mutator.Status
			for i, withSchemata := range []bool{false, true} {
				before := ownAddressSpace(t)
				errs := captureErrors(t)
				got, _, _ := runParityWith(t, mod, tc.profile, withSchemata, tc.settings)
				log.Reset()
				warning := fmt.Sprintf("--test-memory-limit=%s:", tc.settings[configuration.UnleashTestMemoryLimitKey])
				if n := strings.Count(errs.String(), warning); n != tc.wantWarnings {
					t.Errorf("schemata=%t: %d memory-limit warnings, want %d:\n%s", withSchemata, n, tc.wantWarnings, errs.String())
				}
				if after := ownAddressSpace(t); after != before {
					t.Errorf("schemata=%t: gremlins' own RLIMIT_AS went from %+v to %+v", withSchemata, before, after)
				}
				if st := runaway.status(t, modRoot, got); st != tc.wantRunaway {
					t.Errorf("schemata=%t: runaway loop mutant is %s, want %s", withSchemata, st, tc.wantRunaway)
				}
				if st := spare.status(t, modRoot, got); st != tc.wantSpare {
					t.Errorf("schemata=%t: four-GiB mutant is %s, want %s", withSchemata, st, tc.wantSpare)
				}
				byPath[i] = got
			}
			if !maps.Equal(byPath[0], byPath[1]) {
				t.Errorf("verdicts differ: without --schemata %v, with %v", byPath[0], byPath[1])
			}
		})
	}
}

// lockedBuffer is a bytes.Buffer the engine's workers can log to at once.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// captureErrors points the global log's error output at a buffer until the
// test ends.
func captureErrors(t *testing.T) *lockedBuffer {
	t.Helper()
	b := &lockedBuffer{}
	log.Reset()
	log.Init(io.Discard, b)
	t.Cleanup(log.Reset)

	return b
}

// site names the one mutant of a type on the line of a fixture file that
// starts with prefix.
type site struct {
	file, prefix string
	mutType      mutator.Type
}

// status is the site's mutant's status in statuses, which runParityWith keys
// "file:line:col TYPE".
func (s site) status(t *testing.T, modRoot string, statuses map[string]mutator.Status) mutator.Status {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(modRoot, s.file)) //nolint:gosec // G304: the fixture
	if err != nil {
		t.Fatal(err)
	}
	line := 0
	for i, l := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(l, s.prefix) {
			line = i + 1

			break
		}
	}
	var found []mutator.Status
	for k, st := range statuses {
		if strings.HasPrefix(k, fmt.Sprintf("%s:%d:", s.file, line)) && strings.HasSuffix(k, " "+s.mutType.String()) {
			found = append(found, st)
		}
	}
	if line == 0 || len(found) != 1 {
		t.Fatalf("%s line %q: %d %s mutants in %v, want 1", s.file, s.prefix, len(found), s.mutType, statuses)
	}

	return found[0]
}

func ownAddressSpace(t *testing.T) unix.Rlimit {
	t.Helper()
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_AS, &lim); err != nil {
		t.Fatal(err)
	}

	return lim
}
