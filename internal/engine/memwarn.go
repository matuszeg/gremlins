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

package engine

import (
	"strings"
	"sync"

	"github.com/go-gremlins/gremlins/internal/log"
	"github.com/go-gremlins/gremlins/internal/memlimit"
)

// outOfMemoryMarkers are what a process prints when an address-space limit
// stops it: the Go runtime when an allocation fails ("out of memory",
// "cannot allocate memory") or when it cannot reserve its start-up address
// space ("failed to reserve ...", "cannot reserve ..."), and the dynamic
// loader when it cannot map a program at all ("failed to map segment").
var outOfMemoryMarkers = []string{
	"out of memory",
	"cannot allocate memory",
	"failed to reserve",
	"cannot reserve",
	"failed to map segment",
}

// showsOutOfMemory reports whether s holds one of outOfMemoryMarkers.
func showsOutOfMemory(s string) bool {
	for _, m := range outOfMemoryMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}

	return false
}

// memoryLimitWarning says, once per run, that --test-memory-limit stopped a
// process that was building or running tests before any test could judge a
// mutant: a schema build or a null run that failed, or a mutant booked NOT
// VIABLE, whose output shows it ran out of memory. NOT VIABLE mutants leave
// the efficacy denominator, so a limit too tight for the go command, the
// compiler or the linker would otherwise make a run look cleaner than it is.
//
// A mutant KILLED by the limit is no reason to warn: that is the limit doing
// its job.
type memoryLimitWarning struct {
	once  sync.Once
	raw   string
	limit memlimit.Limit
}

func newMemoryLimitWarning(raw string, limit memlimit.Limit) *memoryLimitWarning {
	return &memoryLimitWarning{raw: raw, limit: limit}
}

// noteIfOutOfMemory warns, the first time it is called with output that shows
// running out of memory under a limit. Without a limit it never warns: what
// ran out of memory then ran out of the machine's, not of the flag's.
func (w *memoryLimitWarning) noteIfOutOfMemory(output string) {
	if showsOutOfMemory(output) {
		w.note()
	}
}

// note warns, the first time it is called under a limit, that a process ran
// out of memory under it.
func (w *memoryLimitWarning) note() {
	if w == nil || w.limit == 0 {
		return
	}
	w.once.Do(func() {
		log.Errorf("--test-memory-limit=%s: a build or test process ran out of memory under the limit "+
			"before a test could judge a mutant; mutants it made NOT VIABLE leave the efficacy denominator, "+
			"so the run can look cleaner than it is. Raise the limit if the go command, the compiler or the linker needs more\n",
			w.raw)
	})
}
