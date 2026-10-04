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

package engine

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-gremlins/gremlins/internal/log"
	"github.com/go-gremlins/gremlins/internal/memlimit"
)

// memLimitDealer is a dealer whose null runs are capped at limit, with the
// run's memory-limit warning for raw.
func memLimitDealer(t *testing.T, raw string, limit memlimit.Limit) MutantExecutorDealer {
	t.Helper()

	return MutantExecutorDealer{
		execContext:       exec.CommandContext,
		testExecutionTime: 10 * time.Second,
		wdDealer:          wdStub{dir: t.TempDir()},
		testMemoryLimit:   limit,
		limitWarning:      newMemoryLimitWarning(raw, limit),
	}
}

// The null run's process runs under the limit.
func TestNullRunIsCapped(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "out")
	bin := writeScript(t, "ulimit -v > "+out)
	d := memLimitDealer(t, "1G", 1<<30)
	if err := d.nullRun(context.Background(), bin, t.TempDir(), "", nil); err != nil {
		t.Fatalf("nullRun: %v", err)
	}
	got, err := os.ReadFile(out) //nolint:gosec // G304: test code reading its script's output
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "1048576" {
		t.Errorf("the null run ran under ulimit -v %s, want 1048576", got)
	}
}

// A null run that runs out of memory under the limit warns, once however
// many do; without a limit it never warns.
//
// It is not parallel: it captures the global log.
func TestNullRunOutOfMemoryWarnsOnce(t *testing.T) {
	bin := writeScript(t, "echo 'fatal error: out of memory'\nexit 2")
	testCases := map[string]struct {
		raw   string
		limit memlimit.Limit
		want  int
	}{
		"limited":   {raw: "1G", limit: 1 << 30, want: 1},
		"unlimited": {raw: "", limit: 0, want: 0},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			errs := &bytes.Buffer{}
			log.Reset()
			log.Init(io.Discard, errs)
			defer log.Reset()
			d := memLimitDealer(t, tc.raw, tc.limit)
			for range 3 {
				if err := d.nullRun(context.Background(), bin, t.TempDir(), "", nil); err == nil {
					t.Fatal("nullRun of a failing binary returned nil")
				}
			}
			if n := strings.Count(errs.String(), "--test-memory-limit="); n != tc.want {
				t.Errorf("%d memory-limit warnings, want %d:\n%s", n, tc.want, errs.String())
			}
		})
	}
}

// A null run that fails for another reason does not warn.
//
// It is not parallel: it captures the global log.
func TestNullRunFailureWithoutOutOfMemoryDoesNotWarn(t *testing.T) {
	bin := writeScript(t, "echo '--- FAIL: TestA (0.00s)'\nexit 1")
	errs := &bytes.Buffer{}
	log.Reset()
	log.Init(io.Discard, errs)
	defer log.Reset()
	d := memLimitDealer(t, "1G", 1<<30)
	if err := d.nullRun(context.Background(), bin, t.TempDir(), "", nil); err == nil {
		t.Fatal("nullRun of a failing binary returned nil")
	}
	if strings.Contains(errs.String(), "--test-memory-limit=") {
		t.Errorf("a failure that is not out of memory warned:\n%s", errs.String())
	}
}
