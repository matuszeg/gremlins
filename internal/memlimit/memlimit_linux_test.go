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
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/go-gremlins/gremlins/internal/memlimit"
)

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
	cmd := exec.Command("sh", "-c", `ulimit -S -v; ulimit -H -v; echo "$0 $1"; env`, "zero", "one")
	cmd.Env = append(os.Environ(), "MEMLIMIT_TEST=kept")
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
	if got := reportedAS(t, 0); got.soft != "unlimited" || got.hard != "unlimited" {
		t.Skipf("the test runs under an address-space limit already (%s/%s)", got.soft, got.hard)
	}
	cmd := exec.Command("true")
	memlimit.Wrap(cmd, 0)
	if cmd.Path == "/proc/self/exe" || cmd.Env != nil {
		t.Errorf("a zero limit rewrote the command: %s %v", cmd.Path, cmd.Args)
	}
}

// A command that cannot start is not wrapped, so that it fails to start as it
// did, rather than starting a helper that cannot execute it.
func TestWrapLeavesAnUnstartableCommandAlone(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	cmd := exec.Command(missing) //nolint:gosec // G204: test code running its own path
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
	cmd := &exec.Cmd{Path: "./prog", Args: []string{"./prog"}, Dir: dir}
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
	cmd := exec.Command("sh", "-c", "ulimit -H -v")
	memlimit.Wrap(cmd, 4<<30)
	// The helper -- this test binary -- runs under a hard limit of 2 GiB,
	// set by a shell in between, and is asked for 4 GiB for its command.
	testBin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	inner := exec.Command("sh", append([]string{"-c", `ulimit -v 2097152 && exec "$@"`, "sh", testBin}, cmd.Args[1:]...)...) //nolint:gosec // G204: test code
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
	cmd := exec.Command(prog) //nolint:gosec // G204: test code running its own script
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
