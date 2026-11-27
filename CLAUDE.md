This file provides guidance to Claude Code (claude.ai/code) when
working with code in this repository.

## Project Overview

Gremlins is a mutation testing tool for Go that validates test
effectiveness by introducing mutations to source code and verifying if
tests catch them. It works best on small to medium-sized Go modules
(microservices).

## Essential Commands

### Build & Test

```bash
# Build the binary
make build                    # Creates dist/bin/gremlins

# Run all tests with race detector
make test                     # go test -race ./...

# Run specific package tests
go test -race ./internal/engine/...

# Run single test
go test -race ./internal/engine/ -run TestEngineFindMutations

# Generate coverage report
make cover                    # Creates coverage.out
```

### Linting & Formatting

```bash
# Run linter (golangci-lint)
make lint

# Format imports (with local package priority)
make goimports

# Fix struct field alignment
make fieldalignment
```

### Running Gremlins

```bash
# Basic mutation testing
gremlins unleash              # Or: gremlins run, gremlins r

# Dry-run mode (find mutations without testing)
gremlins unleash --dry-run    # Or: -d

# Configuration file
# Uses .gremlins.yaml in project root
```

### Development Workflow

```bash
# Install required tools
make requirements

# Full development cycle
make all                      # lint + test + build

# Create snapshot release
make snap                     # lint + test + snapshot

# Clean build artifacts
make clean
```

## Architecture

### Core Components

**Engine (`internal/engine/`)**

- Main orchestrator for mutation testing
- Walks AST to find mutation opportunities
- Coordinates mutant execution via worker pool
- Entry point: `Engine.Run(ctx)` traverses files, finds mutants,
  executes tests

**Mutator (`internal/mutator/`)**

- Defines mutation types and their interface
- Types: `ArithmeticBase`, `ConditionalsBoundary`,
  `ConditionalsNegation`, `IncrementDecrement`, `InvertAssignments`,
  `InvertBitwise`, `InvertBitwiseAssignments`, `InvertLogical`,
  `InvertLoopCtrl`, `InvertNegatives`, `RemoveSelfAssignments`
- Status values: `NotCovered`, `Runnable`, `Skipped`, `Lived`,
  `Killed`, `NotViable`, `TimedOut`
- Each mutator implements: `Apply()` (mutate code), `Rollback()`
  (restore original)

**Coverage (`internal/coverage/`)**

- Determines which mutations are covered by tests
- Only covered mutations are tested (uncovered mutations can't be
  caught)

**Executor (`internal/engine/executor.go`)**

- Applies single mutation, runs tests, captures results
- Handles test timeouts and viability checks
- Works with temporary directories to isolate mutations

**Report (`internal/report/`)**

- Formats and outputs mutation testing results
- Supports console output and machine-readable formats
- Calculates efficacy and mutation coverage metrics

**Configuration (`internal/configuration/`)**

- Manages settings via flags, config file (.gremlins.yaml), and
  environment
- Uses Viper for configuration handling
- Controls which mutation types are enabled

### Key Flows

**Mutation Testing Flow:**

1. `cmd/unleash.go` → `run()` initializes components
2. Coverage analysis determines testable code
3. `Engine.Run()` walks file tree, parses Go AST
4. For each token, check `TokenMutantType` mapping for applicable
   mutations
5. Create mutators with status (`Runnable`, `NotCovered`, `Skipped`)
6. Worker pool processes runnable mutants:
   - Copy code to temp workdir
   - Apply mutation via `Mutator.Apply()`
   - Run tests
   - Set status: `Killed` (tests failed), `Lived` (tests passed),
     `TimedOut`, `NotViable` (build failed)
   - Rollback mutation
7. Report results with efficacy/coverage metrics

**Adding New Mutation Type:**

1. Add new constant to `mutator.Type` in `internal/mutator/mutator.go`
2. Update `Types` slice and `String()` method
3. Add token mappings in `internal/engine/mappings.go`
   (`TokenMutantType`)
4. Implement mutation logic in `internal/engine/tokenmutator.go`
   (`ApplyMutation`)
5. Add configuration key in `internal/configuration/`
6. Update flag registration in `cmd/unleash.go`

### Important Patterns

- **Token-based mutations**: Uses Go's AST and token positions to
  identify and apply mutations
- **Workdir isolation**: Each mutation runs in temporary directory
  copy to avoid interference
- **Worker pool**: Concurrent mutation execution with configurable
  parallelism
- **Configuration cascade**: Flags override config file, which
  overrides defaults
- **Context cancellation**: Graceful shutdown on SIGINT/SIGTERM via
  `ctxDoneOnSignal()`

## Testing Conventions

- All test files use `_test.go` suffix
- Tests use table-driven approach where applicable
- Race detector always enabled: `-race` flag
- Test names follow pattern: `Test<ComponentName><Behavior>`
- Use `testdata/` directories for test fixtures

## Module Structure

```text
cmd/                    - CLI commands (main.go, unleash.go, cobra setup)
internal/
  configuration/        - Config handling (Viper-based)
  coverage/            - Test coverage analysis
  diff/                - Git diff integration for incremental testing
  engine/              - Core mutation engine and AST traversal
  exclusion/           - File/pattern exclusion rules
  execution/           - Test execution primitives
  gomodule/            - Go module detection and info
  log/                 - Logging utilities
  mutator/             - Mutation definitions and interface
  report/              - Result formatting and output
```

## Quality Standards

- Go 1.21+ required
- Comprehensive linter configuration (`.golangci.yml`) with 30+
  enabled rules
- Test coverage tracked (Codecov integration)
- All commits must pass: lint, tests, build
- Mutation testing runs on itself (`.gremlins.yaml` configured)

## This fork (matuszeg/gremlins) and its consumers

The Rulewright repos gate on mutation testing with a binary built from
this fork, pinned by commit SHA. Upstream gremlins is not a fallback: it
ignores `//nomutant`, has no `timeout-min`, and books an uncompilable
mutant as KILLED.

- `main` tracks upstream `go-gremlins/gremlins`. `all-changes` is the
  integration branch and the only branch consumers pin. Fork PRs target
  `all-changes`.
- A fix for an upstream issue branches off upstream `main`, never off
  `all-changes`, so each upstream PR stands alone. Upstream is dormant:
  adopt its open PRs into `all-changes` rather than waiting. Adopting
  keeps authorship, but strip any `Co-Authored-By` trailer or the commit
  hook blocks the commit.
- Audit `all-changes` by content, not by `git merge-base --is-ancestor`:
  it carries its own copies of fixes, so ancestry lies.
- Never force-push `all-changes`. A pinned SHA that is orphaned still
  resolves until garbage collection, then every consumer's gate dies.
- Refused, do not re-adopt: scanning test output for a `FAIL` prefix to
  decide a verdict (a suite that prints "FAILED to connect" reads as a
  false KILLED).

### Re-pinning a consumer

Find every pin site with `grep -rn <old-sha>` in the consumer repo. In
the Rulewright monorepo they are `scripts/mutation.sh`,
`scripts/lxc/rulewright-setup.sh`, `CONTRIBUTING.md` and the
`mutation*.yml` workflows; the Go library repos pin in their
`.github/workflows/mutation.yml`. Move them all in one PR. The CI
test-map cache is keyed by the SHA, so after the re-pin lands run the
consumer's `mutation` workflow with `task=seed-test-map` once; the first
run compiles each package once to record compile time.

`~/go/bin/gremlins` holds whichever binary was installed last. Check it
with `go version -m $(command -v gremlins) | grep vcs.revision`, and build
a pinned binary from a clean clone with `-buildvcs=true` so the revision
is stamped.

### Traps

- `gofmt` rewrites a bare `//nomutant` doc comment to `// nomutant`,
  which the directive parser ignores. Use the trailing-colon form,
  `//nomutant:<types> reason`.
- The per-mutant timeout is `max(elapsed * coefficient, timeout-min)`,
  capped by `timeout-max`. A faster suite tightens it, so one mutant can
  TIME OUT on one machine and be KILLED on another.
- `gremlins unleash ./internal/db` (how consumers call it) makes
  `GoModule.Root` relative. Code comparing module paths with
  `go/packages`' absolute `Dir` must absolutise first, or tests with
  absolute roots pass while the real run places zero mutants. Test new
  path logic through `gomodule.Init("./pkg")` after `t.Chdir`.
- A null run (no mutant applied) that fails twice is fatal: that
  package's mutants are ERRORED and the run exits non-zero.
- `--test-selection` pays off only on scoped runs, because the test map
  covers only the scan path. Benchmark the invocation the consumer's
  wrapper script or CI step actually issues, not `gremlins unleash .`:
  whole-module runs measure a shape nobody runs and made selection look
  like a net loss. `--cross-package` disables narrowing; consumers keep
  it off.
- Before a consumer opts into `--map-workers` above 1, measure 2 and 4
  workers on its heaviest packages and compare the cached maps across
  worker counts with timings stripped.

## The test-map cache is soundness-critical

The cache (`internal/coverage/testmap*.go`) reuses a package's test map
across source edits, so a wrong "unchanged" verdict silently skips
tests. Its correctness evidence is the real-toolchain differential suite
(`internal/coverage/differential_internal_test.go`): map, edit, re-map
from cache, map fresh, require equal, and assert which tests re-mapped so
re-mapping everything cannot pass vacuously. Tests using a fake `go`
with canned profiles cannot detect a kept-but-wrong mapping.

When changing invalidation rules, extend the differential suite first.
When review finds holes, check whether they share a shape: a fact the
compiler derives that no source text carries (init order, import
closures, which variables read another package). Take such facts from
`go/types` rather than adding another syntactic pattern.

Every whole re-map logs `testmap: re-mapping all of <pkg>: <reason>`.

## `-race` helper processes need `GORACE=atexit_sleep_ms=0`

Every `-race` binary sleeps about one second at exit (ThreadSanitizer's
`atexit_sleep_ms` default). Tests here that re-execute the test binary as
a fake external command (`exec.Command(os.Args[0], "-test.run=...Helper...")`)
pay that second per helper; a few hundred of them push a package past
`go test`'s 10-minute timeout, which then blames whichever test happened
to be running. Put `GORACE=atexit_sleep_ms=0` in the child's `cmd.Env`
(which replaces the environment wholesale), not the parent's. Before
hunting a `-race` deadlock, compare user+sys time with wall time and
re-run without `-race`; before raising a `-timeout`, count the processes
the package starts.
