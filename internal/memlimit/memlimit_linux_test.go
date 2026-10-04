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

package memlimit_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/go-gremlins/gremlins/internal/memlimit"
)

// childGuard is set on every process these tests start. A test binary that
// finds it exits at once (TestMain): a test that starts this binary must
// never have it run these tests again, which would start it again, without
// end.
const childGuard = "MEMLIMIT_TEST_CHILD"

func TestMain(m *testing.M) {
	if os.Getenv(childGuard) != "" {
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// childTimeout bounds every process these tests start.
const childTimeout = 30 * time.Second

// command is exec.Command for these tests: the process runs in its own
// process group, is killed with the whole group past childTimeout and when
// the test ends, and carries childGuard. Nothing it starts outlives the test.
func command(t *testing.T, name string, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), childTimeout)
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: test code running its own commands
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), childGuard+"=1")
	killGroup := func() error {
		if cmd.Process == nil {
			return nil
		}

		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.Cancel = killGroup
	cmd.WaitDelay = time.Second
	t.Cleanup(func() {
		_ = killGroup()
		cancel()
	})

	return cmd
}

func ownAS(t *testing.T) unix.Rlimit {
	t.Helper()
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_AS, &lim); err != nil {
		t.Fatal(err)
	}

	return lim
}

// childReport is what a wrapped shell reports of itself: its soft and hard
// address-space limits in KiB, as ulimit prints them, and its environment.
type childReport struct {
	soft, hard string
	env        []string
}

// reportedAS runs a shell wrapped with l and returns its report.
func reportedAS(t *testing.T, l memlimit.Limit) childReport {
	t.Helper()
	cmd := command(t, "sh", "-c", `ulimit -S -v; ulimit -H -v; echo "$0 $1"; env`, "zero", "one")
	cmd.Env = append(cmd.Env, "MEMLIMIT_TEST=kept")
	memlimit.Wrap(cmd, l)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 3 || lines[2] != "zero one" {
		t.Fatalf("the wrapped command did not get its arguments: %q", lines)
	}

	return childReport{soft: lines[0], hard: lines[1], env: lines[3:]}
}

func TestWrapCapsTheChildNotTheCaller(t *testing.T) {
	t.Parallel()
	if !memlimit.Enforced {
		t.Fatal("Enforced is false on linux")
	}
	before := ownAS(t)
	got := reportedAS(t, 1<<30)
	if want := strconv.Itoa(1 << 20); got.soft != want || got.hard != want {
		t.Errorf("child ulimit -v = %s soft, %s hard, want %s KiB both", got.soft, got.hard, want)
	}
	if !slices.Contains(got.env, "MEMLIMIT_TEST=kept") {
		t.Error("the wrapped command lost its environment")
	}
	for _, kv := range got.env {
		if strings.HasPrefix(kv, "GREMLINS_INTERNAL_RLIMIT_AS=") {
			t.Errorf("the limit's carrier reached the command: %s", kv)
		}
	}
	if after := ownAS(t); after != before {
		t.Errorf("the caller's RLIMIT_AS changed from %+v to %+v", before, after)
	}
}

func TestWrapZeroLeavesTheCommandAlone(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("true")
	path, args := cmd.Path, slices.Clone(cmd.Args)
	memlimit.Wrap(cmd, 0)
	if cmd.Path != path || !slices.Equal(cmd.Args, args) || cmd.Env != nil {
		t.Errorf("a zero limit rewrote the command: %s %v %v", cmd.Path, cmd.Args, cmd.Env)
	}
}

// A command with no Env gets the PWD Start would have set from its Dir.
func TestWrapKeepsPWDOfANilEnv(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmd := command(t, "sh", "-c", `echo "$PWD"`)
	cmd.Env = nil // what is under test: Start's PWD for a nil Env
	cmd.Dir = dir
	memlimit.Wrap(cmd, 1<<30)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != dir {
		t.Errorf("PWD = %q, want %q", got, dir)
	}
}

// A command with no Args runs with Path as argv[0], as Start runs it.
func TestWrapCommandWithoutArgs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	prog := filepath.Join(dir, "prog")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\nulimit -v > "+out+"\n"), 0o700); err != nil { //nolint:gosec // G306: it must be executable
		t.Fatal(err)
	}
	cmd := command(t, prog)
	cmd.Args = nil
	memlimit.Wrap(cmd, 1<<30)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	got, err := os.ReadFile(out) //nolint:gosec // G304: test code reading its script's output
	if err != nil {
		t.Fatal(err)
	}
	if want := strconv.Itoa(1 << 20); strings.TrimSpace(string(got)) != want {
		t.Errorf("ulimit -v = %s, want %s", got, want)
	}
}

// The limit's variable alone does not make a process the helper: this test
// binary, started with it under its own name, runs as itself.
func TestHelperNeedsItsName(t *testing.T) {
	t.Parallel()
	testBin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := command(t, testBin, "-test.run=^$")
	cmd.Env = append(cmd.Env, "GREMLINS_INTERNAL_RLIMIT_AS=1073741824")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("the test binary with only the variable set: %v: %s", err, out)
	}
}

// A helper started without a program to execute dies by a signal, rather
// than running as the program it is part of.
func TestHelperWithoutAProgramIsSignalled(t *testing.T) {
	t.Parallel()
	testBin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := command(t, testBin)
	cmd.Args = []string{"gremlins-memlimit", "/bin/true"}
	cmd.Env = append(cmd.Env, "GREMLINS_INTERNAL_RLIMIT_AS=1073741824")
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("err = %v, want an exit error: %s", err, out)
	}
	if ws, _ := exitErr.Sys().(syscall.WaitStatus); !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Errorf("helper ended %v, want killed by SIGKILL: %s", ws, out)
	}
}

// A command that cannot start is not wrapped, so that it fails to start as it
// did, rather than starting a helper that cannot execute it.
func TestWrapLeavesAnUnstartableCommandAlone(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	cmd := command(t, missing)
	memlimit.Wrap(cmd, 1<<30)
	if cmd.Path != missing {
		t.Fatalf("Wrap rewrote a command whose program does not exist: %s", cmd.Path)
	}
	if err := cmd.Start(); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Start = %v, want it to fail as unwrapped", err)
	}
}

// A relative program is found from the command's directory, as Start finds
// it.
func TestWrapRelativeToDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "prog"), []byte("#!/bin/sh\nulimit -v\n"), 0o700); err != nil { //nolint:gosec // G306: it must be executable
		t.Fatal(err)
	}
	cmd := command(t, "sh")
	cmd.Path, cmd.Args, cmd.Err, cmd.Dir = "./prog", []string{"./prog"}, nil, dir
	memlimit.Wrap(cmd, 1<<30)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got, want := strings.TrimSpace(string(out)), strconv.Itoa(1<<20); got != want {
		t.Errorf("ulimit -v = %s, want %s", got, want)
	}
}

// A limit above the hard limit the caller runs under cannot be raised to; the
// child gets the caller's hard limit instead of failing to start.
func TestWrapNeverRaises(t *testing.T) {
	t.Parallel()
	cmd := command(t, "sh", "-c", "ulimit -H -v")
	memlimit.Wrap(cmd, 4<<30)
	// The helper -- this test binary -- runs under a hard limit of 2 GiB,
	// set by a shell in between, and is asked for 4 GiB for its command.
	testBin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// bash's exec -a keeps the helper's name, without which the test binary
	// would run as itself, and so run this test again.
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("needs bash for exec -a")
	}
	inner := command(t, bash, append([]string{"-c", `ulimit -v 2097152 && exec -a "$0" "$@"`, cmd.Args[0], testBin}, cmd.Args[1:]...)...)
	inner.Env = cmd.Env
	out, err := inner.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "2097152" {
		t.Errorf("hard ulimit -v = %s, want the caller's 2097152", got)
	}
}

// A helper that cannot execute the command dies by a signal, which reads as
// a run that reached no verdict, never as an exit status a test chose.
func TestHelperThatCannotExecIsSignalled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	prog := filepath.Join(dir, "prog")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // G306: it must be executable
		t.Fatal(err)
	}
	cmd := command(t, prog)
	memlimit.Wrap(cmd, 1<<30)
	if err := os.Remove(prog); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("err = %v, want an exit error", err)
	}
	ws, _ := exitErr.Sys().(syscall.WaitStatus)
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Errorf("helper ended %v, want killed by SIGKILL", ws)
	}
	if !strings.Contains(string(out), "exec "+prog) {
		t.Errorf("helper did not say what it could not execute: %q", out)
	}
}
