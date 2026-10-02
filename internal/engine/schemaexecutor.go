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
	"go/token"

	"github.com/go-gremlins/gremlins/internal/log"
	"github.com/go-gremlins/gremlins/internal/mutator"
)

// classifyDirect turns the observations of one direct run of a prebuilt test
// binary (GREMLINS_MUTANT=<id>) into a mutant status. It is the counterpart of
// the go test classification in runTestCommand and getTestFailedStatus.
//
// The checks run in order. A timeout marker with an error means the test
// binary's own -test.timeout fired; the deadline means the per-mutant backstop
// killed it; a cancelled run follows the on-shutdown-status setting. A
// negative exit code means a signal ended the process, which is no verdict.
// Exit 1 (test failure) and 2 (panic) kill the mutant only if the reach file
// shows the mutant's site ran: a failure without reach is a broken baseline or
// environment, so it is ERRORED, never a kill. A pass without reach is NOT
// COVERED. pos is only used to name the mutant in the log line.
func classifyDirect(err error, exitCode int, sawTimeout, reached, deadlineHit, runCancelled bool, pos token.Position) mutator.Status {
	switch {
	case err != nil && sawTimeout:
		return mutator.RunTimedOut
	case deadlineHit:
		return mutator.TimedOut
	case runCancelled:
		return shutdownStatus()
	case exitCode < 0:
		log.Errorf("test run for %s reached no verdict: the test binary was terminated by a signal\n", pos)

		return mutator.Errored
	case exitCode == 0:
		if reached {
			return mutator.Lived
		}

		return mutator.NotCovered
	case exitCode == 1 || exitCode == 2:
		if reached {
			return mutator.Killed
		}
		log.Errorf("test run for %s failed without reaching the mutant (exit %d)\n", pos, exitCode)

		return mutator.Errored
	default:
		log.Errorf("test run for %s reached no verdict: unexpected exit code %d\n", pos, exitCode)

		return mutator.Errored
	}
}
