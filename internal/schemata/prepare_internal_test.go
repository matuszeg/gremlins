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
	"testing"

	"github.com/go-gremlins/gremlins/internal/mutator"
)

// TestNetFailedChecksTheMutantsOwnPackage holds the mutants of a package
// whose rewritten files failed to write -- BuildAll records that error under
// the package itself -- to the per-mutant path even when their selection
// names only a dependent package that built: no binary compiles them in.
func TestNetFailedChecksTheMutantsOwnPackage(t *testing.T) {
	t.Parallel()
	const writeErr = "schemata: write p/p.go: permission denied"
	testCases := map[string]struct {
		need   map[string][]string
		failed map[string]string
		want   []string
		// red, before and wantRed: the red packages, reasons the mutants
		// already had, and the red package each should name (nil: none).
		red     map[string]bool
		before  []string
		wantRed []string
	}{
		"own_package_write_failed_selection_only_dependent": {
			need:   map[string][]string{"m/p": {"m/dep"}, "m/q": {"m/q"}},
			failed: map[string]string{"m/p": writeErr},
			want:   []string{writeErr, writeErr, ""},
		},
		"selected_package_failed": {
			need:   map[string][]string{"m/p": {"m/dep"}, "m/q": {"m/q"}},
			failed: map[string]string{"m/dep": "build failed"},
			want:   []string{"build failed", "build failed", ""},
		},
		"nothing_failed": {
			need: map[string][]string{"m/p": {"m/dep"}, "m/q": {"m/q"}},
			want: []string{"", "", ""},
		},
		// #1397: the build failure of p would send its mutants down the
		// per-mutant path, where dep's red test would "kill" them.
		"a_red_selected_package_wins_over_an_earlier_build_failure": {
			need:    map[string][]string{"m/p": {"m/dep"}, "m/q": {"m/q"}},
			failed:  map[string]string{"m/p": "build failed", "m/dep": nullFailed + "--- FAIL: TestX"},
			red:     map[string]bool{"m/dep": true},
			want:    []string{nullFailed + "--- FAIL: TestX", nullFailed + "--- FAIL: TestX", ""},
			wantRed: []string{"m/dep", "m/dep", ""},
		},
		"a_red_package_wins_over_a_reason_already_given": {
			need:    map[string][]string{"m/p": {"m/p"}, "m/q": {"m/q"}},
			failed:  map[string]string{"m/q": nullFailed + "boom"},
			red:     map[string]bool{"m/q": true},
			before:  []string{"", "", "schemata: no mutation site"},
			want:    []string{"", "", nullFailed + "boom"},
			wantRed: []string{"", "", "m/q"},
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := &preparer{
				muts:    make([]mutator.Mutator, 3),
				reasons: make([]string, 3),
				red:     make([]string, 3),
				pkgOf:   []string{"m/p", "m/p", "m/q"},
			}
			copy(p.reasons, tc.before)
			p.netFailed(tc.need, tc.failed, tc.red)
			for i, want := range tc.want {
				if p.reasons[i] != want {
					t.Errorf("mutant %d (%s): reason %q, want %q", i, p.pkgOf[i], p.reasons[i], want)
				}
				if wantRed := append(tc.wantRed, "", "", "")[i]; p.red[i] != wantRed {
					t.Errorf("mutant %d (%s): red %q, want %q", i, p.pkgOf[i], p.red[i], wantRed)
				}
			}
		})
	}
}
