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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-gremlins/gremlins/internal/engine/workerpool"
	"github.com/go-gremlins/gremlins/internal/gomodule"
	"github.com/go-gremlins/gremlins/internal/log"
	"github.com/go-gremlins/gremlins/internal/mutator"
	"github.com/go-gremlins/gremlins/internal/procgroup"
	"github.com/go-gremlins/gremlins/internal/schemata"
)

// schemaBackstopGrace is how far past -test.timeout the per-run context
// deadline sits. The binary is prebuilt, so nothing but the run itself needs
// bounding; the grace only lets the binary's own watchdog fire first and print
// its marker, which is the better evidence.
const schemaBackstopGrace = 5 * time.Second

// NewSchemaExecutor returns a workerpool.Executor that judges mut, whose ID in
// the schema build b is id, by running b's prebuilt test binaries with that
// mutant switched on, instead of mutating the source and running go test.
//
// The executor never modifies the worker's copy of the module, which keeps
// the original source: the binaries run in it so that tests reading source
// files or testdata see what they always see. A selected package with no
// binary in b, or a run in integration mode, is not guessed at: the mutant is
// handed whole to the executor NewExecutor returns, and judged exactly as
// without schemata.
func (m MutantExecutorDealer) NewSchemaExecutor(mut mutator.Mutator, id int, b *schemata.Build,
	outCh chan<- mutator.Mutator, wg *sync.WaitGroup,
) workerpool.Executor {
	legacy, _ := m.NewExecutor(mut, outCh, wg).(*mutantExecutor)
	overlays := m.overlays
	if overlays == nil {
		// A dealer not made by NewExecutorDealer: correct, but the overlay
		// is written per mutant.
		overlays = newOverlayCache()
	}
	counts := m.schemaCounts
	if counts == nil {
		// Likewise: correct, but the counts reach no summary.
		counts = &schemaCounts{}
	}

	return &schemaExecutor{legacy: legacy, id: id, build: b, overlays: overlays, counts: counts}
}

// schemaExecutor runs one mutant against the schema test binaries. It holds
// the legacy executor both for its configuration and test selection, and as
// the fallback for a mutant it cannot run.
type schemaExecutor struct {
	legacy   *mutantExecutor
	build    *schemata.Build
	overlays *overlayCache
	counts   *schemaCounts
	id       int
}

// Start is the workerpool.Executor entry point.
func (s *schemaExecutor) Start(w *workerpool.Worker) {
	m := s.legacy
	switch {
	case s.build == nil:
		s.fallBack(w, "no schema build")

		return
	case m.integrationMode:
		// Integration mode runs every package of the module; the binaries
		// cover only the packages that were built.
		s.fallBack(w, "integration mode runs every package of the module")

		return
	}
	workerName := fmt.Sprintf("%s-%d", w.Name, w.ID)
	rootDir, err := m.wdDealer.Get(workerName)
	if err != nil {
		log.Errorf("failed to get working directory for worker %s: %v", workerName, err)
		panic(fmt.Sprintf("failed to get working directory for worker %s: %v", workerName, err))
	}
	m.mutant.SetWorkdir(filepath.Join(rootDir, m.module.CallingDir))

	if m.mutant.Status() == mutator.NotCovered || m.mutant.Status() == mutator.Skipped || m.dryRun {
		defer m.wg.Done()
		m.outCh <- m.mutant

		return
	}

	sel := m.selectTests(m.mutant.Pkg())
	// Absolute, like every path a binary run is given: it runs in a package
	// directory, and the overlay's keys must name the files the go command
	// sees.
	rootDir, err = filepath.Abs(rootDir)
	if err != nil {
		s.fallBack(w, err.Error())

		return
	}
	runs, err := s.plan(rootDir, sel)
	if err == nil {
		var overlay string
		overlay, err = s.overlays.get(s.build, rootDir, m.wdDealer.WorkDir())
		for i := range runs {
			runs[i].overlay = overlay
		}
	}
	if err != nil {
		s.fallBack(w, err.Error())

		return
	}

	// The reach file sits in the work directory, outside every module copy,
	// where no test walking its package's tree sees it; the worker's name
	// keeps two workers judging one mutant apart.
	// Absolute: the binary runs in its package's directory.
	reach, err := filepath.Abs(filepath.Join(m.wdDealer.WorkDir(), "schemata-reached-"+workerName+"-"+strconv.Itoa(s.id)))
	if err != nil {
		s.fallBack(w, err.Error())

		return
	}

	defer m.wg.Done()
	s.counts.judged.Add(1)
	m.mutant.SetStatus(s.runAll(reach, sel.tests, runs))
	m.outCh <- m.mutant
}

// fallBack hands the mutant whole to the legacy executor, counting it for the
// run's summary and saying why.
func (s *schemaExecutor) fallBack(w *workerpool.Worker, reason string) {
	s.counts.fallbacks.Add(1)
	log.Errorf("mutant at %s runs through go test instead of the schema binaries: %s\n", s.legacy.mutant.Position(), reason)
	s.legacy.Start(w)
}

// binaryRun is one package's test binary and where it runs.
type binaryRun struct {
	bin, dir, overlay string
}

// plan resolves each selected package to its binary and its directory in the
// worker copy, in selection order. Any package it cannot resolve fails the
// whole plan: running the others alone could miss the test that kills the
// mutant.
func (s *schemaExecutor) plan(rootDir string, sel testRun) ([]binaryRun, error) {
	runs := make([]binaryRun, 0, len(sel.pkgs))
	for _, pkg := range sel.pkgs {
		bin, ok := s.build.Binaries[pkg]
		if !ok {
			return nil, fmt.Errorf("no schema test binary for %s", pkg)
		}
		dir, ok := packageDir(rootDir, s.legacy.module, pkg)
		if !ok {
			return nil, fmt.Errorf("%s is not a package of module %s", pkg, s.legacy.module.Name)
		}
		runs = append(runs, binaryRun{bin: bin, dir: dir})
	}

	return runs, nil
}

// packageDir maps an import path of mod to its directory under rootDir, a
// copy of the module root.
func packageDir(rootDir string, mod gomodule.GoModule, pkg string) (string, bool) {
	if pkg == mod.Name {
		return rootDir, true
	}
	rel, ok := strings.CutPrefix(pkg, mod.Name+"/")
	if !ok || rel == "" {
		return "", false
	}

	return filepath.Join(rootDir, filepath.FromSlash(rel)), true
}

// runAll runs the packages in order and stops at the first verdict other
// than LIVED or NOT COVERED. Past them all, the mutant LIVED if any run
// reached its site, and is NOT COVERED if none did. reach is the file the
// mutant's site creates when it runs.
func (s *schemaExecutor) runAll(reach string, tests []string, runs []binaryRun) mutator.Status {
	anyReached := false
	for _, r := range runs {
		if s.legacy.runCtx.Err() != nil {
			return shutdownStatus()
		}
		status, reached, cancelled := s.runOne(reach, tests, r)
		if cancelled || (status != mutator.Lived && status != mutator.NotCovered) {
			return status
		}
		anyReached = anyReached || reached
	}
	if anyReached {
		return mutator.Lived
	}

	return mutator.NotCovered
}

// runOne runs one test binary with the mutant switched on and classifies the
// run. It also reports whether the mutant's site ran and whether the run was
// cancelled, which ends the mutant whatever the status says. The reach file
// is removed both before the run and after it.
func (s *schemaExecutor) runOne(reach string, tests []string, r binaryRun) (mutator.Status, bool, bool) {
	m := s.legacy
	pos := m.mutant.Position()
	// A reach file left by an earlier package's run, or an earlier run of
	// this worker, would credit this run with a reach it did not make.
	if err := os.Remove(reach); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Errorf("test run for %s reached no verdict: cannot clear %s: %v\n", pos, reach, err)

		return mutator.Errored, false, false
	}

	ctx, cancel := context.WithTimeout(m.runCtx, m.testExecutionTime+schemaBackstopGrace)
	defer cancel()
	cmd := m.execContext(ctx, r.bin, s.binaryArgs(tests)...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(),
		"GREMLINS_MUTANT="+strconv.Itoa(s.id),
		"GREMLINS_REACHED="+reach,
		overlayGOFLAGS(r.overlay),
	)
	scanner := newOutputScanner()
	cmd.Stdout = scanner
	cmd.Stderr = scanner
	cmd.WaitDelay = outputDrainGrace
	procgroup.Setup(cmd)

	err := run(ctx, cmd)

	exitCode := -1 // never started: classifyDirect's contract
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	_, statErr := os.Stat(reach)
	reached := statErr == nil
	if err := os.Remove(reach); err != nil && !errors.Is(err, fs.ErrNotExist) {
		// The next run clears it before it starts; this run's verdict stands.
		log.Errorf("cannot remove the reach file %s: %v\n", reach, err)
	}
	cancelled := m.runCtx.Err() != nil
	deadlineHit := errors.Is(ctx.Err(), context.DeadlineExceeded)
	status := classifyDirect(err, exitCode, scanner.sawTestTimeout(), reached, deadlineHit, cancelled, pos)

	return status, reached, cancelled
}

// binaryArgs are the test binary's flags: the go test flags the legacy
// executor passes, in their -test. form, and the -test.paniconexit0 go test
// adds itself. Build tags and -vet do not apply to a binary that is already
// built.
func (s *schemaExecutor) binaryArgs(tests []string) []string {
	m := s.legacy
	// go test always passes -test.paniconexit0, so a test that calls
	// os.Exit(0) fails the run instead of passing it.
	args := []string{"-test.count=1", "-test.timeout", m.testExecutionTime.String(), "-test.failfast", "-test.paniconexit0"}
	if len(tests) > 0 {
		args = append(args, "-test.run", "^("+strings.Join(tests, "|")+")$")
	}
	if m.testCPU != 0 {
		args = append(args, "-test.cpu", strconv.Itoa(m.testCPU))
	}

	return args
}

// overlayGOFLAGS is the GOFLAGS setting that adds -overlay=overlay to the
// user's own GOFLAGS.
func overlayGOFLAGS(overlay string) string {
	return "GOFLAGS=" + strings.TrimSpace(os.Getenv("GOFLAGS")+" -overlay="+overlay)
}

// overlayCache holds, per schema build and worker copy, the overlay file that
// points the worker copy's rewritten files at the schema copy's. A test that
// runs the go command itself then builds what the binary was built from.
type overlayCache struct {
	files map[overlayKey]string
	mu    sync.Mutex
}

type overlayKey struct {
	build *schemata.Build
	root  string
}

func newOverlayCache() *overlayCache {
	return &overlayCache{files: map[overlayKey]string{}}
}

// get returns the overlay file of build for the worker copy root, writing it
// into tmpDir the first time it is asked for.
func (c *overlayCache) get(build *schemata.Build, root, tmpDir string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := overlayKey{build: build, root: root}
	if path, ok := c.files[key]; ok {
		return path, nil
	}
	replace := make(map[string]string, len(build.Rewritten))
	for _, rel := range build.Rewritten {
		replace[filepath.Join(root, rel)] = filepath.Join(build.Dir, rel)
	}
	data, err := json.Marshal(struct{ Replace map[string]string }{replace})
	if err != nil {
		return "", fmt.Errorf("overlay: %w", err)
	}
	f, err := os.CreateTemp(tmpDir, "schemata-overlay-*.json")
	if err != nil {
		return "", fmt.Errorf("overlay: %w", err)
	}
	path := f.Name()
	_, err = f.Write(data)
	if err == nil {
		// Absolute: it goes in GOFLAGS of a binary run in a package directory.
		path, err = filepath.Abs(path)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("overlay: %w", err)
	}
	// GOFLAGS is split on spaces and has no quoting.
	if strings.ContainsAny(path, " \t\n") {
		_ = os.Remove(path)

		return "", fmt.Errorf("overlay path %q has whitespace and cannot go in GOFLAGS", path)
	}
	c.files[key] = path

	return path, nil
}

// classifyDirect turns the observations of one direct run of a prebuilt test
// binary (GREMLINS_MUTANT=<id>) into a mutant status. It is the counterpart of
// the go test classification in runTestCommand and getTestFailedStatus.
//
// The checks run in order. A timeout marker with an error means the test
// binary's own -test.timeout fired; the deadline means the per-mutant backstop
// killed it; a cancelled run follows the on-shutdown-status setting. A
// negative exit code means a signal ended the process, which is no verdict.
// Any positive exit code -- 1 (test failure), 2 (panic) or whatever a test's
// os.Exit chose -- kills the mutant if the reach file shows the mutant's site
// ran, as go test, which folds every failing binary exit into its own exit 1,
// would. A failure without reach is a broken baseline or environment, so it is
// ERRORED, never a kill. A pass without reach is NOT COVERED.
//
// exitCode must be cmd.ProcessState.ExitCode(), which is -1 both for a
// signalled process and when the process never started (missing binary, bad
// Dir, fork failure); callers must not default it to 0 on a start failure. err
// is deliberately not consulted past the timeout check: err == exec.ErrWaitDelay
// with exit code 0 is a pass, and an err-based guard would misclassify it.
//
// pos is only used to name the mutant in the log line.
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
	case reached:
		return mutator.Killed
	default:
		log.Errorf("test run for %s failed without reaching the mutant (exit %d)\n", pos, exitCode)

		return mutator.Errored
	}
}
