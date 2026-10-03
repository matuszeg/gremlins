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
	"go/ast"
	"strings"
	"testing"
)

// TestLayoutSpansWithRepeatedText holds layoutSpans to what it can know: a
// child of a rewritten site is found by its text, searched forward from the
// end of the previous child. Siblings with equal text are told apart by the
// cursor; a child whose text the rewrite repeats (as an op= switch repeats
// its operands in every arm) is found at the first copy, so an error in a
// later copy is attributed to the parent. That costs the parent a drop
// (restoreChildren gives the child its retry), never a broken build.
func TestLayoutSpansWithRepeatedText(t *testing.T) {
	t.Parallel()
	const file = "p.go"
	child := func(text string) *siteNode { return &siteNode{site: Site{Node: &ast.Ident{}}, rendered: text} }
	cases := map[string]struct {
		parent    string
		children  []string
		wantStart []int // each child's span start, -1 for no span
		errAt     int   // an error offset, relative to the parent's start
		wantBlame int   // index in children of the site blamed, -1 for the parent
	}{
		"siblings_with_equal_text_each_found_in_order": {
			parent: "f(X, X)", children: []string{"X", "X"}, wantStart: []int{2, 5}, errAt: 5, wantBlame: 1,
		},
		"text_repeated_in_the_parent_is_found_at_the_first_copy": {
			parent: "case 1: X; default: X", children: []string{"X"}, wantStart: []int{8}, errAt: 20, wantBlame: -1,
		},
		"error_in_the_first_copy_blames_the_child": {
			parent: "case 1: X; default: X", children: []string{"X"}, wantStart: []int{8}, errAt: 8, wantBlame: 0,
		},
		"child_text_absent_from_the_parent_has_no_span": {
			parent: "f(Y)", children: []string{"X"}, wantStart: []int{-1}, errAt: 2, wantBlame: -1,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := &siteNode{site: Site{Node: &ast.Ident{}}, rendered: tc.parent}
			for _, c := range tc.children {
				root.children = append(root.children, child(c))
			}
			const base = 10 // the parent's offset in its file
			var spans []renderedSpan
			layoutChildren(root.children, base, 0, true, root.rendered, &spans)
			spans = append(spans, renderedSpan{site: root.site, start: base, end: base + len(tc.parent)})

			for i, c := range root.children {
				start := -1
				for _, s := range spans {
					if s.site.Node == c.site.Node {
						start = s.start - base
					}
				}
				if start != tc.wantStart[i] {
					t.Errorf("child %d (%q) starts at %d, want %d", i, c.rendered, start, tc.wantStart[i])
				}
			}
			overlay := map[string][]byte{file: []byte(strings.Repeat(" ", base) + tc.parent)}
			bad, unattributable := attribute([]typeError{{file: file, offset: base + tc.errAt, msg: "boom"}}, overlay, map[string][]renderedSpan{file: spans})
			if unattributable != "" {
				t.Fatalf("unattributable: %s", unattributable)
			}
			blamed := root.site.Node
			if tc.wantBlame >= 0 {
				blamed = root.children[tc.wantBlame].site.Node
			}
			if _, ok := bad[mutantKey{blamed, 0}]; !ok || len(bad) != 1 {
				t.Errorf("blamed %v, want only %v", bad, blamed)
			}
		})
	}
}
