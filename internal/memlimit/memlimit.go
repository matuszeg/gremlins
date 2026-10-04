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

// Package memlimit caps the address space of the processes gremlins starts to
// test a mutant, and leaves gremlins' own alone.
//
// A mutant can turn a loop into one that never ends and allocates on every
// pass. With a worker per CPU, a few such test binaries exhaust the machine. A
// cap on each test process lets the runaway binary alone die, the way
// `ulimit -v` around the whole of gremlins used to, without capping gremlins:
// with schemata gremlins type-checks the module in-process, and that needs
// more address space than a test binary should get.
package memlimit

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// Limit is a cap on a process's address space (RLIMIT_AS), in bytes. Zero is
// no cap.
type Limit uint64

// sizePattern is a decimal number, optionally fractional, and an optional
// unit.
var sizePattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([kKmMgGtT]?)(i?[bB])?$`)

// units are binary multiples, so that `2500000K` is `ulimit -v 2500000`,
// which counts KiB.
var units = map[string]int64{"": 1, "k": 1 << 10, "m": 1 << 20, "g": 1 << 30, "t": 1 << 40}

// maxLimit bounds a parsed limit below RLIM_INFINITY, the all-ones value that
// means no limit at all.
var maxLimit = new(big.Rat).SetInt(new(big.Int).SetUint64(1 << 63))

// Parse reads a limit: a number of bytes with an optional unit K, M, G or T,
// each a power of 1024, optionally followed by B or iB, in either case. A
// fractional number is allowed when it comes to whole bytes: 2.5G is, 1.5 is
// not. An empty string and 0 are no limit.
func Parse(s string) (Limit, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	m := sizePattern.FindStringSubmatch(s)
	if m == nil || (m[2] == "" && strings.HasPrefix(m[3], "i")) {
		return 0, fmt.Errorf("invalid memory limit %q: want a number of bytes with an optional K, M, G or T", s)
	}
	n, ok := new(big.Rat).SetString(m[1])
	if !ok {
		return 0, fmt.Errorf("invalid memory limit %q", s)
	}
	n.Mul(n, new(big.Rat).SetInt64(units[strings.ToLower(m[2])]))
	if !n.IsInt() {
		return 0, fmt.Errorf("invalid memory limit %q: not a whole number of bytes", s)
	}
	if n.Cmp(maxLimit) >= 0 {
		return 0, fmt.Errorf("invalid memory limit %q: too large", s)
	}

	return Limit(n.Num().Uint64()), nil
}
