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
	"errors"
	"go/token"
	"testing"

	"github.com/go-gremlins/gremlins/internal/mutator"
)

func TestClassifyDirect(t *testing.T) {
	t.Parallel()

	exitErr := errors.New("exit status 1")
	pos := token.Position{Filename: "a.go", Line: 3, Column: 4}

	testCases := map[string]struct {
		err         error
		exitCode    int
		sawTimeout  bool
		reached     bool
		deadlineHit bool
		cancelled   bool
		want        mutator.Status
	}{
		"test_timeout_marker":                {err: exitErr, exitCode: 2, sawTimeout: true, reached: true, want: mutator.RunTimedOut},
		"timeout_marker_wins_over_deadline":  {err: exitErr, exitCode: -1, sawTimeout: true, deadlineHit: true, want: mutator.RunTimedOut},
		"timeout_marker_wins_over_cancelled": {err: exitErr, exitCode: 2, sawTimeout: true, cancelled: true, want: mutator.RunTimedOut},
		"marker_without_error_is_ignored":    {exitCode: 0, sawTimeout: true, reached: true, want: mutator.Lived},
		"deadline_hit":                       {err: exitErr, exitCode: -1, reached: true, deadlineHit: true, want: mutator.TimedOut},
		"deadline_wins_over_cancelled":       {err: exitErr, exitCode: -1, deadlineHit: true, cancelled: true, want: mutator.TimedOut},
		"cancelled":                          {err: exitErr, exitCode: -1, cancelled: true, want: shutdownStatus()},
		"signalled":                          {err: exitErr, exitCode: -1, reached: true, want: mutator.Errored},
		"start_failure":                      {err: exitErr, exitCode: -1, want: mutator.Errored},
		"pass_reached":                       {exitCode: 0, reached: true, want: mutator.Lived},
		"pass_without_reach":                 {exitCode: 0, want: mutator.NotCovered},
		"fail_reached":                       {err: exitErr, exitCode: 1, reached: true, want: mutator.Killed},
		"fail_without_reach":                 {err: exitErr, exitCode: 1, want: mutator.Errored},
		"panic_exit_2_reached":               {err: exitErr, exitCode: 2, reached: true, want: mutator.Killed},
		"panic_exit_2_without_reach":         {err: exitErr, exitCode: 2, want: mutator.Errored},
		"other_exit_code_reached":            {err: exitErr, exitCode: 3, reached: true, want: mutator.Killed},
		"other_exit_code_without_reach":      {err: exitErr, exitCode: 3, want: mutator.Errored},
		"os_exit_42_reached":                 {err: exitErr, exitCode: 42, reached: true, want: mutator.Killed},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := classifyDirect(tc.err, tc.exitCode, tc.sawTimeout, tc.reached, tc.deadlineHit, tc.cancelled, pos)
			if got != tc.want {
				t.Errorf("classifyDirect() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCombineRuns(t *testing.T) {
	t.Parallel()

	var (
		runTimedOut = runResult{status: mutator.RunTimedOut}
		timedOut    = runResult{status: mutator.TimedOut}
		signalled   = runResult{status: mutator.Errored, noVerdict: true}
		killed      = runResult{status: mutator.Killed}
		failed      = runResult{status: mutator.Errored}
		lived       = runResult{status: mutator.Lived}
		notCovered  = runResult{status: mutator.NotCovered}
	)
	testCases := map[string]struct {
		runs []runResult
		want mutator.Status
	}{
		"no_runs":                         {want: mutator.NotCovered},
		"run_timeout_beats_backstop":      {runs: []runResult{timedOut, runTimedOut}, want: mutator.RunTimedOut},
		"backstop_beats_no_verdict":       {runs: []runResult{signalled, timedOut}, want: mutator.TimedOut},
		"no_verdict_beats_kill":           {runs: []runResult{killed, signalled}, want: mutator.Errored},
		"kill_beats_unreached_failure":    {runs: []runResult{failed, killed}, want: mutator.Killed},
		"unreached_failure_beats_lived":   {runs: []runResult{lived, failed}, want: mutator.Errored},
		"reached_first":                   {runs: []runResult{lived, notCovered}, want: mutator.Lived},
		"reached_last":                    {runs: []runResult{notCovered, lived}, want: mutator.Lived},
		"none_reached":                    {runs: []runResult{notCovered, notCovered}, want: mutator.NotCovered},
		"kill_then_timeout":               {runs: []runResult{killed, runTimedOut}, want: mutator.RunTimedOut},
		"timeout_then_kill_order_ignored": {runs: []runResult{runTimedOut, killed, lived}, want: mutator.RunTimedOut},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := combineRuns(tc.runs); got != tc.want {
				t.Errorf("combineRuns() = %v, want %v", got, tc.want)
			}
		})
	}
}
