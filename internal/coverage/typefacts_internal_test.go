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
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/go-gremlins/gremlins/internal/gomodule"
)

// The order is the type-checker's, test files and external tests included,
// and a package with an error says nothing rather than something partial.
func TestLoadTypesFromSourceReadsTheInitialisationOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	root := t.TempDir()
	for rel, content := range map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.22\n",
		// b waits for a, which is declared after it; the blanks are named
		// by file and by place, and b.go sorts after a.go.
		"p/a.go":      "package p\n\nfunc f() int { return 1 }\n\nvar _ = f()\n\nvar b = a + 1\n\nvar a = 1\n\nvar _ = f()\n",
		"p/b.go":      "package p\n\nvar _ = f()\n",
		"p/p_test.go": "package p\n\nvar t1 = b\n",
		"p/x_test.go": "package p_test\n\nimport \"example.com/m/p\"\n\nvar x = p.Name\n",
		"p/name.go":   "package p\n\nconst Name = \"p\"\n",
		"bad/bad.go":  "package bad\n\nvar v = undefined\n",
	} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c := &Coverage{mod: gomodule.GoModule{Name: "example.com/m", Root: root}}

	got := c.loadTypesFromSource(true, []string{"example.com/m/p", "example.com/m/bad"})

	want := map[string]typeFacts{"example.com/m/p": {initOrder: []string{
		"_@a.go#0", "a", "b", "_@a.go#1", "_@b.go#0", "t1", xtestPrefix + "x",
	}}}
	if diff := cmp.Diff(want, got, cmp.AllowUnexported(typeFacts{})); diff != "" {
		t.Errorf("unexpected facts (-want +got):\n%s", diff)
	}
	if plain := c.loadTypesFromSource(false, []string{"example.com/m/p"}); len(plain["example.com/m/p"].initOrder) != 5 {
		t.Errorf("want the package as another binary links it, without its tests, got %v", plain)
	}
}

func TestSameInitOrderComparesWhatBothInitialise(t *testing.T) {
	t.Parallel()

	testCases := map[string]struct {
		was, now []string
		// first and second are the variables initOrderSwap names, empty
		// when the orders agree.
		first, second string
	}{
		"the same":               {was: []string{"a", "b"}, now: []string{"a", "b"}},
		"one added and one gone": {was: []string{"a", "gone", "b"}, now: []string{"new", "a", "b"}},
		"two trading places":     {was: []string{"c", "a", "b"}, now: []string{"c", "b", "a"}, first: "a", second: "b"},
		// A name twice in one list is not something a type-checker says,
		// and it is not taken as agreement, on either side.
		"a name listed twice before": {was: []string{"a", "a"}, now: []string{"a"}, first: "a", second: "a"},
		"a name listed twice after":  {was: []string{"b"}, now: []string{"b", "b"}, first: "b", second: "b"},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			agree := tc.first == ""
			if got := sameInitOrder(tc.was, tc.now); got != agree {
				t.Errorf("sameInitOrder(%v, %v) = %v, want %v", tc.was, tc.now, got, agree)
			}
			first, second, swapped := initOrderSwap(tc.was, tc.now)
			if first != tc.first || second != tc.second || swapped == agree {
				t.Errorf("initOrderSwap(%v, %v) = %q, %q, %v, want %q, %q, %v",
					tc.was, tc.now, first, second, swapped, tc.first, tc.second, !agree)
			}
		})
	}
}
