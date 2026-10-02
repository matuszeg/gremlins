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
	"errors"
	"go/ast"
	"go/token"
	"sort"
	"strings"
)

var (
	// errDuplicateSite reports a site whose range equals an earlier site's.
	errDuplicateSite = errors.New("schemata: duplicate site range")
	// errOverlap reports a site that partially overlaps another site.
	errOverlap = errors.New("schemata: site partially overlaps another site")
	// errBadRange reports a site whose node range lies outside the source.
	errBadRange = errors.New("schemata: site range outside source")
	// errNewlineChanged reports a rewrite that adds or removes a newline,
	// which would move every line after it.
	errNewlineChanged = errors.New("schemata: rewrite changes the line count")
)

// Rewriter returns the replacement text for site s. inner(n) returns the
// source text of n with every site nested inside n already rendered.
type Rewriter func(s Site, inner func(ast.Node) string) (string, error)

// SiteError is a site that was left as its original text, and why.
type SiteError struct {
	Site Site
	Err  error
}

// siteNode is a site in the containment forest.
type siteNode struct {
	site       Site
	start, end int
	children   []*siteNode
	rendered   string
}

// Render returns src with every site replaced by its rewrite. Edits are
// spliced into the original bytes: text outside the sites is byte-identical
// and every line keeps its number. Sites nested in another site are rendered
// first and reach the outer rewrite through inner. A site whose rewrite fails
// keeps its original text (with its nested sites still rendered) and is
// reported as a SiteError.
func Render(fset *token.FileSet, src []byte, sites []Site, rw Rewriter) ([]byte, []SiteError) {
	roots, errs := buildForest(fset, src, sites)
	for _, r := range roots {
		errs = renderNode(fset, src, r, rw, errs)
	}

	return []byte(splice(src, 0, len(src), roots)), errs
}

// buildForest nests each site under the smallest site containing it.
func buildForest(fset *token.FileSet, src []byte, sites []Site) ([]*siteNode, []SiteError) {
	var errs []SiteError
	nodes := make([]*siteNode, 0, len(sites))
	for _, s := range sites {
		start, end, ok := nodeRange(fset, src, s.Node)
		if !ok {
			errs = append(errs, SiteError{Site: s, Err: errBadRange})

			continue
		}
		nodes = append(nodes, &siteNode{site: s, start: start, end: end})
	}
	// Outer sites before the sites they contain; stable keeps caller order
	// among identical ranges so the later duplicate is the one reported.
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].start != nodes[j].start {
			return nodes[i].start < nodes[j].start
		}

		return nodes[i].end > nodes[j].end
	})

	var roots, stack []*siteNode
	for _, n := range nodes {
		for len(stack) > 0 && stack[len(stack)-1].end <= n.start {
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 {
			roots = append(roots, n)
			stack = append(stack, n)

			continue
		}
		top := stack[len(stack)-1]
		switch {
		case top.start == n.start && top.end == n.end:
			errs = append(errs, SiteError{Site: n.site, Err: errDuplicateSite})
		case n.end > top.end:
			errs = append(errs, SiteError{Site: n.site, Err: errOverlap})
		default:
			top.children = append(top.children, n)
			stack = append(stack, n)
		}
	}

	return roots, errs
}

// renderNode renders n's children, then n itself, into n.rendered.
func renderNode(fset *token.FileSet, src []byte, n *siteNode, rw Rewriter, errs []SiteError) []SiteError {
	for _, c := range n.children {
		errs = renderNode(fset, src, c, rw, errs)
	}
	inner := func(x ast.Node) string {
		start, end, ok := nodeRange(fset, src, x)
		if !ok {
			return ""
		}

		return splice(src, start, end, n.children)
	}
	original := splice(src, n.start, n.end, n.children)
	text, err := rw(n.site, inner)
	switch {
	case err != nil:
		errs = append(errs, SiteError{Site: n.site, Err: err})
		text = original
	case strings.Count(text, "\n") != strings.Count(string(src[n.start:n.end]), "\n"):
		errs = append(errs, SiteError{Site: n.site, Err: errNewlineChanged})
		text = original
	}
	n.rendered = text

	return errs
}

// splice returns src[start:end] with every site in sites (sorted, disjoint)
// that lies within the range replaced by its rendered text. A range that lies
// strictly inside a site is spliced from that site's children instead.
func splice(src []byte, start, end int, sites []*siteNode) string {
	var b strings.Builder
	pos := start
	for _, s := range sites {
		switch {
		case s.end <= start || s.start >= end:
			continue
		case s.start >= start && s.end <= end:
			b.Write(src[pos:s.start])
			b.WriteString(s.rendered)
			pos = s.end
		case s.start <= start && s.end >= end:
			return splice(src, start, end, s.children)
		}
	}
	b.Write(src[pos:end])

	return b.String()
}

// nodeRange returns n's byte range within src, unadjusted by //line
// directives.
func nodeRange(fset *token.FileSet, src []byte, n ast.Node) (int, int, bool) {
	if n == nil || !n.Pos().IsValid() || !n.End().IsValid() {
		return 0, 0, false
	}
	start := fset.PositionFor(n.Pos(), false).Offset
	end := fset.PositionFor(n.End(), false).Offset
	if start < 0 || end < start || end > len(src) {
		return 0, 0, false
	}

	return start, end, true
}
