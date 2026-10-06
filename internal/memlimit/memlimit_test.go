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

package memlimit_test

import (
	"testing"

	"github.com/go-gremlins/gremlins/internal/memlimit"
)

func TestParse(t *testing.T) {
	t.Parallel()
	ok := map[string]memlimit.Limit{
		"":         0,
		"0":        0,
		"\t0\n":    0,
		"1048576":  1 << 20,
		"1024B":    1024,
		"4k":       4 << 10,
		"4K":       4 << 10,
		"4KiB":     4 << 10,
		"2500000K": 2500000 << 10, // ulimit -v 2500000
		"2500M":    2500 << 20,
		"2500MB":   2500 << 20,
		"2.5G":     5 << 29,
		"2.5GiB":   5 << 29,
		"1T":       1 << 40,
		"0.5k":     512,
	}
	for in, want := range ok {
		got, err := memlimit.Parse(in)
		if err != nil {
			t.Errorf("Parse(%q): %v", in, err)

			continue
		}
		if got != want {
			t.Errorf("Parse(%q) = %d, want %d", in, got, want)
		}
	}
	for _, in := range []string{"-1", "-1G", "G", "1X", "1.5", "0.1B", "1e9", "1GG", "abc", "99999999999T", "1.0000000001K"} {
		if got, err := memlimit.Parse(in); err == nil {
			t.Errorf("Parse(%q) = %d, want an error", in, got)
		}
	}
}

// TestShowsOutOfMemory holds each marker as out-of-memory output, and output
// with none of them as not.
func TestShowsOutOfMemory(t *testing.T) {
	t.Parallel()
	for _, m := range memlimit.OutOfMemoryMarkers {
		if !memlimit.ShowsOutOfMemory("fatal error: runtime: " + m + "\n") {
			t.Errorf("output with %q not seen as out of memory", m)
		}
	}
	if memlimit.ShowsOutOfMemory("./x.go:3:1: undefined: y\n") {
		t.Error("a compile error seen as out of memory")
	}
}
