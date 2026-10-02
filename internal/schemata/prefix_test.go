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
	"go/parser"
	"go/token"
	"testing"
)

func TestChoosePrefix(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		src  string
		want string
	}{
		"default_free": {src: "package p\nfunc f() { x := 1; _ = x }\n", want: "_gremlins"},
		"prefix_taken": {src: "package p\nvar _gremlinsX = 1\n", want: "_gremlins2"},
		"local_taken":  {src: "package p\nfunc f() { _gremlins := 1; _ = _gremlins }\n", want: "_gremlins2"},
		"two_taken":    {src: "package p\nvar _gremlinsA, _gremlins2B = 1, 2\n", want: "_gremlins3"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, err := parser.ParseFile(token.NewFileSet(), "p.go", tc.src, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := ChoosePrefix([]*ast.File{f}); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
