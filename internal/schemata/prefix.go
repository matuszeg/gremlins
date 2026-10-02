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
	"strconv"
	"strings"
)

// ChoosePrefix returns an identifier prefix that no identifier in files
// starts with -- locals, fields and labels included -- so that nothing the
// rewrite generates can collide with, shadow or be captured by a user name.
func ChoosePrefix(files []*ast.File) string {
	var names []string
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				names = append(names, id.Name)
			}

			return true
		})
	}
	for i := 1; ; i++ {
		p := "_gremlins"
		if i > 1 {
			p += strconv.Itoa(i)
		}
		if !anyHasPrefix(names, p) {
			return p
		}
	}
}

func anyHasPrefix(names []string, prefix string) bool {
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}

	return false
}
