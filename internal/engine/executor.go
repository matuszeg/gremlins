/*
 * Copyright 2022 The Gremlins Authors
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
	"context"
	"errors"
	"fmt"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-gremlins/gremlins/internal/configuration"
	"github.com/go-gremlins/gremlins/internal/coverage"
	"github.com/go-gremlins/gremlins/internal/engine/workdir"
	"github.com/go-gremlins/gremlins/internal/engine/workerpool"
	"github.com/go-gremlins/gremlins/internal/gomodule"
	"github.com/go-gremlins/gremlins/internal/log"
	"github.com/go-gremlins/gremlins/internal/mutator"
)

// DefaultTimeoutCoefficient is the default multiplier for the timeout length
// of each test run.
const DefaultTimeoutCoefficient = 5

// noTimeoutMax is the timeout ceiling meaning "no ceiling": the per-mutant
// timeout is whatever the coefficient produces. This is the default, so the
// behaviour of a run that does not set --timeout-max is unchanged.
const noTimeoutMax = time.Duration(0)

// ExecutorDealer is the initializer for new workerpool.Executor.
type ExecutorDealer interface {
	NewExecutor(mut mutator.Mutator, outCh chan<- mutator.Mutator, wg *sync.WaitGroup) workerpool.Executor
}

// MutantExecutorDealer is a ExecutorDealer for the initialisation of a mutantExecutor.
//
// By default, it sets uses exec.Command to perform the tests on the source
// code. This can be overridden, for example in tests.
//
// The apply and rollback functions are wrappers around the TokenMutator apply and
// rollback. These can be overridden with nop functions in tests. Not an
// ideal setup. In the future we can think of a better way to handle this.
type MutantExecutorDealer struct {
	wdDealer          workdir.Dealer
	execContext       execContext
	testMap           TestSelector
	dependents        DependentFinder
	runCtx            context.Context
	mod               gomodule.GoModule
	buildTags         string
	testExecutionTime time.Duration
	dryRun            bool
	integrationMode   bool
	crossPackage      bool
	testCPU           int
}

// SetRunCtx wires the engine's root context into the dealer so that each
// mutantExecutor it produces can observe cancellation (e.g. SIGTERM from
// a CI runner) and mark in-flight mutants accordingly instead of falling
// through to the default mutator.Lived branch.
func (m *MutantExecutorDealer) SetRunCtx(ctx context.Context) {
	m.runCtx = ctx
}

// ExecutorDealerOption is the defining option for the initialisation of a ExecutorDealer.
type ExecutorDealerOption func(j MutantExecutorDealer) MutantExecutorDealer

// WithExecContext overrides the default exec.Command with a custom executor.
func WithExecContext(c execContext) ExecutorDealerOption {
	return func(m MutantExecutorDealer) MutantExecutorDealer {
		m.execContext = c

		return m
	}
}

// TestSelector answers, for a position in the code, which tests execute it, and
// whether a package's tests are known at all.
//
// It is the executor's whole view of the test map: an executor asks what covers
// this mutant and whether it may trust the answer for this package, and nothing
// else about how the map was built.
type TestSelector interface {
	// Mapped reports whether the tests of a package are known in full.
	Mapped(pkg string) bool

	// TestsFor returns the tests that execute the given position.
	TestsFor(pos token.Position) []coverage.TestID
}

// DependentFinder answers which packages depend on a package, through their own
// code or through their tests.
//
// It is what --cross-package needs and all it needs: which packages a mutation
// could break is a question about imports, answerable statically, with no
// coverage and no test runs.
type DependentFinder interface {
	Dependents(pkg string) []string
}

// WithDependents turns on cross-package testing: a mutant is tested against the
// packages that depend on the one it is in, not only that one.
func WithDependents(d DependentFinder) ExecutorDealerOption {
	return func(m MutantExecutorDealer) MutantExecutorDealer {
		m.dependents = d

		return m
	}
}

// WithTestSelection turns on test selection: instead of the whole suite of the
// mutated package, each mutant runs the tests the selector says execute its
// line, wherever those tests live.
func WithTestSelection(sel TestSelector) ExecutorDealerOption {
	return func(m MutantExecutorDealer) MutantExecutorDealer {
		m.testMap = sel

		return m
	}
}

// NewExecutorDealer initialises a MutantExecutorDealer.
func NewExecutorDealer(mod gomodule.GoModule, wdd workdir.Dealer, elapsed time.Duration, opts ...ExecutorDealerOption) *MutantExecutorDealer {
	buildTags := configuration.Get[string](configuration.UnleashTagsKey)
	dryRun := configuration.Get[bool](configuration.UnleashDryRunKey)
	integrationMode := configuration.Get[bool](configuration.UnleashIntegrationMode)
	crossPackage := configuration.Get[bool](configuration.UnleashCrossPackageKey)
	testCPU := configuration.Get[int](configuration.UnleashTestCPUKey)
	tCoefficient := configuration.Get[int](configuration.UnleashTimeoutCoefficientKey)

	coefficient := DefaultTimeoutCoefficient
	if tCoefficient != 0 {
		coefficient = tCoefficient
	}

	if testCPU != 0 && integrationMode {
		testCPU /= testCPU
	}

	// Use a minimum of 1 second for timeout calculation to prevent
	// unreasonably short timeouts when coverage runs very quickly
	baseTime := elapsed
	if baseTime < time.Second {
		baseTime = time.Second
	}

	// The floor under the whole product, not under the baseline.
	//
	// A mutant's cost has little to do with how long the healthy suite
	// takes: a passing suite runs success paths, while a mutant pushes
	// tests onto their failure paths, which are usually fixed waits —
	// a poll with a deadline, a select on a timer — that the baseline
	// never pays. So coefficient x elapsed gets TIGHTER as a package
	// gets faster, and a fast package can end up with a bound no
	// mutant can finish inside. Measured downstream: a suite that went
	// from 6.3s to 543ms took 30 of 32 mutants from killed to timed
	// out, none of them hung.
	//
	// Zero means unset, which leaves the previous behaviour exactly as
	// it was.
	timeout := baseTime * time.Duration(coefficient)
	if floor := configuration.GetDuration(configuration.UnleashTimeoutMinKey); timeout < floor {
		timeout = floor
	}

	jd := MutantExecutorDealer{
		mod:               mod,
		crossPackage:      crossPackage,
		wdDealer:          wdd,
		buildTags:         buildTags,
		dryRun:            dryRun,
		integrationMode:   integrationMode,
		testCPU:           testCPU,
		// The floor is applied first and the ceiling second, so
		// --timeout-max wins a contradictory pair rather than the order
		// of the two flags deciding it.
		testExecutionTime: cappedExecutionTime(timeout),
		execContext:       exec.CommandContext,
	}

	for _, opt := range opts {
		jd = opt(jd)
	}

	return &jd
}

// cappedExecutionTime clamps a coefficient-derived per-mutant timeout to the
// absolute ceiling set by --timeout-max, if one is set.
//
// The coefficient-derived timeout is proportional to how long the package's own
// tests take, which is unrelated to how much damage a runaway mutant can do in
// that time. Mutating a loop-advance statement (i++ -> i--) inside a scanner
// loop whose body appends per iteration produces a mutant that never terminates
// and allocates until the machine is out of memory. On a CI runner the OOM
// killer then reaps the runner agent itself, so the job dies with no verdict at
// all — not a LIVED mutant, not a TIMED OUT one, just a dead runner. The
// timeout is the only defence against that, and scaling it by test duration
// hands the longest leash to the slowest-testing packages, which is backwards.
//
// A ceiling bounds the exposure independently of the baseline: past it the
// mutant is killed and recorded as TIMED OUT, which is the honest verdict for a
// mutant that did not terminate.
//
// Zero (the default) means no ceiling, so runs that do not set the flag behave
// exactly as before. A malformed or non-positive value is reported and ignored
// rather than silently treated as "no cap", because a safety ceiling that
// quietly does nothing is worse than one that was never configured.
func cappedExecutionTime(d time.Duration) time.Duration {
	raw := configuration.Get[string](configuration.UnleashTimeoutMaxKey)
	if raw == "" {
		return d
	}

	max, err := time.ParseDuration(raw)
	if err != nil {
		log.Errorf("invalid %s %q: %v; running without a timeout ceiling\n",
			configuration.UnleashTimeoutMaxKey, raw, err)

		return d
	}
	if max <= noTimeoutMax {
		log.Errorf("invalid %s %q: must be positive; running without a timeout ceiling\n",
			configuration.UnleashTimeoutMaxKey, raw)

		return d
	}

	if d > max {
		return max
	}

	return d
}

// NewExecutor returns a new workerpool.Executor for the given mutator.Mutator.
// It gets an output channel of mutator.Mutator and a sync.WaitGroup. The channel
// will stream the results of the executor, and the wait group will be done when the
// executor is complete.
func (m MutantExecutorDealer) NewExecutor(mut mutator.Mutator, outCh chan<- mutator.Mutator, wg *sync.WaitGroup) workerpool.Executor {
	runCtx := m.runCtx
	if runCtx == nil {
		runCtx = context.Background()
	}
	mj := mutantExecutor{
		mutant:            mut,
		outCh:             outCh,
		wg:                wg,
		wdDealer:          m.wdDealer,
		module:            m.mod,
		testMap:           m.testMap,
		dependents:        m.dependents,
		crossPackage:      m.crossPackage,
		dryRun:            m.dryRun,
		integrationMode:   m.integrationMode,
		buildTags:         m.buildTags,
		execContext:       m.execContext,
		testCPU:           m.testCPU,
		testExecutionTime: m.testExecutionTime,
		runCtx:            runCtx,
	}

	return &mj
}

type execContext = func(ctx context.Context, name string, args ...string) *exec.Cmd

type mutantExecutor struct {
	mutant            mutator.Mutator
	testMap           TestSelector
	dependents        DependentFinder
	wdDealer          workdir.Dealer
	outCh             chan<- mutator.Mutator
	wg                *sync.WaitGroup
	execContext       execContext
	runCtx            context.Context
	module            gomodule.GoModule
	buildTags         string
	testExecutionTime time.Duration
	dryRun            bool
	integrationMode   bool
	crossPackage      bool
	testCPU           int
}

// Start is the implementation of the workerpool.Executor definition and is the
// method responsible for performing the actual mutation testing.
// The executor runs on its mutator.Mutator.
// If it is RUNNABLE, and it is not in dry-run mode, it will apply the mutation,
// run the tests and mark the TokenMutator as either KILLED or LIVED depending
// on the result. If the tests pass, it means the TokenMutator survived, so it
// will be LIVED, if the tests fail, the TokenMutator will be KILLED.
// The timeout of the test is managed outside the run of the test, using
// a context with timeout. This is done because the Go test command doesn't
// make it easy to distinguish failures from timeouts.
func (m *mutantExecutor) Start(w *workerpool.Worker) {
	defer m.wg.Done()
	workerName := fmt.Sprintf("%s-%d", w.Name, w.ID)
	rootDir, err := m.wdDealer.Get(workerName)
	if err != nil {
		log.Errorf("failed to get working directory for worker %s: %v", workerName, err)
		panic(fmt.Sprintf("failed to get working directory for worker %s: %v", workerName, err))
	}

	workingDir := filepath.Join(rootDir, m.module.CallingDir)
	m.mutant.SetWorkdir(workingDir)

	if m.mutant.Status() == mutator.NotCovered || m.mutant.Status() == mutator.Skipped || m.dryRun {
		m.outCh <- m.mutant

		return
	}

	if err := m.mutant.Apply(); err != nil {
		log.Errorf("failed to apply mutation at %s - %s\n\t%v", m.mutant.Position(), m.mutant.Status(), err)

		return
	}

	m.mutant.SetStatus(m.runTests(rootDir, m.mutant.Pkg()))

	if err := m.mutant.Rollback(); err != nil {
		// What should we do now?
		log.Errorf("failed to restore mutation at %s - %s\n\t%v", m.mutant.Position(), m.mutant.Status(), err)
	}

	m.outCh <- m.mutant
}

// testRun is the single `go test` invocation a mutant is judged by: the
// packages to run, and which tests within them. An empty tests slice means
// whole suites, which is what runs when there is no test selection.
//
// It is one invocation and not one per package on purpose. `go test` takes many
// packages with one -run, builds them together, and most of what a mutant costs
// is that build: measured on four packages of Rulewright's backend with a -run
// matching nothing at all, four invocations took 4147ms against 2115ms for one.
// The tests were never the expense; the invocations were.
type testRun struct {
	pkgs  []string
	tests []string
}

func (m *mutantExecutor) runTests(rootDir, pkg string) mutator.Status {
	// Root the test ctx in the engine's runCtx so that an external
	// cancellation (e.g. SIGTERM from a CI runner) propagates into the
	// `go test` subprocess and we can distinguish "the runner is shutting
	// us down" from "the test ran past its deadline".
	ctx, cancel := context.WithTimeout(m.runCtx, m.testExecutionTime)
	defer cancel()

	return m.runTestCommand(ctx, rootDir, m.selectTests(pkg))
}

// selectTests decides what to run for the mutant, along two independent axes.
//
// Which PACKAGES: the mutated one, and with --cross-package the packages that
// depend on it, because those are the ones a mutation can break. That is the
// gap package scoping leaves — go-gremlins/gremlins#224, a LIVED verdict that
// was correct for what it measured — and answering it needs nothing but the
// import graph.
//
// Which TESTS within them: all of them, or with --test-selection only the ones
// coverage says execute the mutated line. Narrowing inside a package it was
// already going to build and pay fixtures for cannot cost more than not
// narrowing; measured on Rulewright's backend it is 24% of the test executions.
//
// The two compose: neither flag is today's behaviour, both together is the
// narrowest run that still sees the callers.
//
// Every path that cannot answer confidently widens rather than narrows, to the
// whole suites of the packages it settled on: never wrong, only slow.
func (m *mutantExecutor) selectTests(pkg string) testRun {
	pkgs := []string{pkg}
	if m.crossPackage {
		pkgs = append(pkgs, m.dependents.Dependents(pkg)...)
	}
	wholeSuites := testRun{pkgs: pkgs}
	if m.testMap == nil || m.integrationMode {
		return wholeSuites
	}
	// A package the map could not see whole might hold the very test that kills
	// this mutant, and skipping it would turn a killed mutant into a LIVED one.
	if !m.testMap.Mapped(pkg) {
		return wholeSuites
	}
	tests := within(m.testMap.TestsFor(m.mutant.Position()), pkgs)
	if len(tests) == 0 {
		// An uncovered mutant never reaches here, so an empty answer means the
		// map is incomplete — coverage is not always deterministic — rather than
		// that no test exercises the line.
		return wholeSuites
	}

	var sel testRun
	seenPkg := make(map[string]struct{}, len(tests))
	seenName := make(map[string]struct{}, len(tests))
	names := make([]string, 0, len(tests))
	for _, id := range tests {
		if _, ok := seenPkg[id.Pkg]; !ok {
			seenPkg[id.Pkg] = struct{}{}
			sel.pkgs = append(sel.pkgs, id.Pkg)
		}
		// The -run pattern applies to every package listed, so a name shared by
		// two packages runs in both even where only one covers the line. That
		// runs more tests than strictly needed, never fewer, so it can only cost
		// time — and it saves an invocation per package, which costs more.
		if _, ok := seenName[id.Name]; !ok {
			seenName[id.Name] = struct{}{}
			sel.tests = append(sel.tests, id.Name)
		}
		names = append(names, id.String())
	}
	m.mutant.SetTestsRun(names)

	return sel
}

// within keeps the tests that live in one of the packages being run. Tests
// outside them are not this run's business: without --cross-package that means
// the mutated package alone, and with it the packages that depend on it.
func within(tests []coverage.TestID, pkgs []string) []coverage.TestID {
	in := make(map[string]struct{}, len(pkgs))
	for _, p := range pkgs {
		in[p] = struct{}{}
	}
	kept := tests[:0:0]
	for _, id := range tests {
		if _, ok := in[id.Pkg]; ok {
			kept = append(kept, id)
		}
	}

	return kept
}

func (m *mutantExecutor) runTestCommand(ctx context.Context, rootDir string, sel testRun) mutator.Status {
	cmd := m.execContext(ctx, "go", m.getTestArgs(sel)...)
	cmd.Dir = m.mutant.Workdir()
	if m.integrationMode {
		cmd.Dir = rootDir
	}
	cmd.Env = append(cmd.Env, os.Environ()...)
	cmd.Env = append(cmd.Env, fmt.Sprintf("GOTMPDIR=%s", m.wdDealer.WorkDir()))

	// Set up process group for killing entire process tree
	setupProcessGroup(cmd)

	// go reports a test binary that died by signal in its OUTPUT and still exits 1,
	// the same code an ordinary test failure uses, so the exit status alone would
	// credit a destroyed run to the test suite. Capture the output to tell them apart.
	//
	// The capture goes to a file rather than an in-process buffer on purpose: os/exec
	// hands a file descriptor straight to the child, where an io.Writer would make
	// Wait block on a copier that a surviving grandchild can hold open.
	outFile, ferr := os.CreateTemp(m.wdDealer.WorkDir(), "gremlins-test-output-")
	if ferr != nil {
		log.Errorf("failed to capture the test output of %s: %v\n", m.mutant.Position(), ferr)
	} else {
		defer func() {
			name := outFile.Name()
			_ = outFile.Close()
			_ = os.Remove(name)
		}()
		cmd.Stdout = outFile
		cmd.Stderr = outFile
	}

	err := run(ctx, cmd)

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return mutator.TimedOut
	}
	// If the parent runCtx was cancelled (Ctrl-C or runner SIGTERM) the
	// `go test` child was killed before reaching a verdict. Reporting
	// these as LIVED misrepresents the data — they were never tested.
	// The configured shutdown status (default NotCovered) is the truthful
	// outcome.
	if m.runCtx.Err() != nil {
		return shutdownStatus()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		status := getTestFailedStatus(exitErr.ExitCode())
		if status == mutator.Killed && testBinaryTerminatedBySignal(testOutput(outFile)) {
			log.Errorf("test run for %s reached no verdict: the test binary was terminated by a signal\n",
				m.mutant.Position())

			return mutator.Errored
		}
		if status == mutator.Errored {
			// The error carries the signal name ("signal: killed"), which is the only
			// thing that tells an OOM kill apart from a crash, and neither of them is
			// visible in the status alone.
			log.Errorf("test run for %s reached no verdict: %v\n", m.mutant.Position(), exitErr)
		}

		return status
	}

	return mutator.Lived
}

// shutdownStatus returns the status to record for mutants that were
// in-flight when the run was cancelled. The choice is driven by the
// `unleash.on-shutdown-status` config key (CLI flag
// --on-shutdown-status). Unrecognised values fall back to NotCovered,
// the truthful default ("we never finished running this mutant").
func shutdownStatus() mutator.Status {
	v := configuration.Get[string](configuration.UnleashOnShutdownStatusKey)
	if s, ok := mutator.ParseShutdownStatus(v); ok {
		return s
	}
	return mutator.NotCovered
}

// maxCapturedOutput bounds how much of the test output is read back. What is
// looked for there is written by go itself at the end of a failing package, and
// a chatty suite can produce far more than is worth holding in memory.
const maxCapturedOutput = 64 << 10

// testOutput reads back the tail of what the test command wrote, or nil if the
// output was never captured or cannot be read. Nil is the safe answer: it leaves
// the exit code to speak for itself, which is the behaviour without a capture.
func testOutput(f *os.File) []byte {
	if f == nil {
		return nil
	}
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	offset := int64(0)
	if info.Size() > maxCapturedOutput {
		offset = info.Size() - maxCapturedOutput
	}
	buf := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(buf, offset); err != nil {
		return nil
	}

	return buf
}

// goSignalledTestBinary matches the two lines go writes when the test binary it
// spawned was terminated by a signal: the reason, then the FAIL summary for the
// package. The pairing is what makes the match evidence — a test that prints
// "signal: something" of its own accord has not been killed.
var goSignalledTestBinary = regexp.MustCompile(`(?m)^signal: [^\n]+\nFAIL\b`)

// testBinaryTerminatedBySignal reports whether go said the test binary it ran was
// terminated by a signal. There is no exit code for this — go uses 1, the code an
// ordinary test failure uses — so its output is the only channel that carries it.
func testBinaryTerminatedBySignal(output []byte) bool {
	return goSignalledTestBinary.Match(output)
}

func (m *mutantExecutor) getTestArgs(sel testRun) []string {
	// A mutant must be judged by tests that actually ran against it, never by a cached
	// result: -count=1 makes the run ineligible for Go's test cache.
	args := []string{"test", "-count=1"}
	if m.buildTags != "" {
		args = append(args, "-tags", m.buildTags)
	}
	// Here we add some seconds to the timeout to be sure it's gremlins that catches the test
	// timeout and not the test itself. The timeout on the test prevents the test.* processes
	// from hanging forever.
	args = append(args, "-timeout", (2*time.Second + m.testExecutionTime).String())
	args = append(args, "-failfast")

	if m.testCPU != 0 {
		args = append(args, "-cpu", fmt.Sprintf("%d", m.testCPU))
	}

	// An empty selection is the whole suite: no -run at all, exactly the command
	// that runs without selection.
	if len(sel.tests) > 0 {
		args = append(args, "-run", "^("+strings.Join(sel.tests, "|")+")$")
	}

	if m.integrationMode {
		return append(args, "./...")
	}

	return append(args, sel.pkgs...)
}

func run(ctx context.Context, cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}

	// Ensure cleanup happens regardless of how we exit
	defer func() {
		if cmd.Process != nil {
			// Always kill the process group to catch any child processes
			// This is safe even if the process already exited
			_ = killProcessGroup(cmd)
			// Release OS resources
			_ = cmd.Process.Release()
		}
	}()

	// Monitor context cancellation in parallel with process execution
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-ctx.Done():
		// Context cancelled/timed out - kill the entire process group
		// Do this BEFORE the parent process exits to catch children
		_ = killProcessGroup(cmd)
		// Wait for the process to actually exit
		<-done

		return ctx.Err()
	case err := <-done:
		// Process completed normally
		return err
	}
}

// getTestFailedStatus maps the exit of the test command to the status of the
// mutant it was testing.
//
// A negative exit code is os/exec reporting a process that did not exit on its
// own but was terminated by a signal — an OOM kill, most often, since a mutant
// can turn ordinary code into an allocation bomb and a memory cap then kills the
// test process. That run produced no verdict, so it is neither a mutant the
// tests failed to kill nor one they killed.
func getTestFailedStatus(exitCode int) mutator.Status {
	switch {
	case exitCode == 1:
		return mutator.Killed
	case exitCode == 2:
		return mutator.NotViable
	case exitCode < 0:
		return mutator.Errored
	default:
		return mutator.Lived
	}
}
