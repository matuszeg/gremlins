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

package memlimit

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Enforced reports whether Wrap caps anything on this platform.
const Enforced = true

// limitEnv carries the limit from Wrap to the re-executed helper.
const limitEnv = "GREMLINS_INTERNAL_RLIMIT_AS"

// helperName is the helper's argv[0]. A process is the helper only when it
// has both this name and limitEnv: the variable alone, exported by hand,
// leaves gremlins running as gremlins.
const helperName = "gremlins-memlimit"

// self is the running executable. /proc/self/exe names it even when the file
// has since been replaced or removed.
const self = "/proc/self/exe"

// Wrap makes cmd, which has not started, start with its address space capped
// at l: soft and hard RLIMIT_AS, as `ulimit -v` sets them, so that the
// processes it starts inherit the cap. Zero leaves cmd alone, and so does a
// command that would not start anyway, so that its Start fails as it did.
//
// The cap is set before the program is executed, not after it starts. The
// difference is not a race but what the program does with it: the Go runtime
// sizes the address space it reserves at start-up by the limit it finds, so a
// test binary capped after it started has already reserved more than a binary
// started under the same `ulimit -v`, and a cap the latter runs within kills
// the former before its first test. No system call sets the limit of a child
// between fork and exec, so cmd runs the executable gremlins is running,
// which sets the limit on itself and executes the command in its place:
// same process, same process group, same arguments, environment and
// directory.
//
// A command with no Env gets what Start would have given it: the caller's
// environment, with PWD set to Dir when Dir is set.
func Wrap(cmd *exec.Cmd, l Limit) {
	if l == 0 || cmd.Err != nil || !startable(cmd) {
		return
	}
	args := cmd.Args
	if len(args) == 0 {
		// Start runs a command with no Args with Path as argv[0].
		args = []string{cmd.Path}
	}
	cmd.Args = append([]string{helperName, cmd.Path}, args...)
	cmd.Path = self
	env := cmd.Env
	if env == nil {
		env = withPWD(os.Environ(), cmd.Dir)
	}
	cmd.Env = append(withoutLimit(env), limitEnv+"="+strconv.FormatUint(uint64(l), 10))
}

// withPWD is env with PWD set to dir, as Start sets it for a command whose
// Env is nil and whose Dir is set; env as it is when dir is empty or cannot
// be made absolute, as Start leaves it then.
func withPWD(env []string, dir string) []string {
	if dir == "" {
		return env
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return env
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "PWD=") {
			out = append(out, kv)
		}
	}

	return append(out, "PWD="+abs)
}

// startable reports whether cmd's program is a file Start could execute.
// A relative path is relative to cmd.Dir, as it is for Start.
func startable(cmd *exec.Cmd) bool {
	path := cmd.Path
	if !filepath.IsAbs(path) && cmd.Dir != "" {
		path = filepath.Join(cmd.Dir, path)
	}

	return unix.Access(path, unix.X_OK) == nil
}

func withoutLimit(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, limitEnv+"=") {
			out = append(out, kv)
		}
	}

	return out
}

// init turns a process Wrap started into the helper: it caps itself and
// executes the wrapped command. It never returns from that.
//
//nolint:gochecknoinits // the helper must run before the program it is part of
func init() {
	raw, ok := os.LookupEnv(limitEnv)
	if !ok || len(os.Args) == 0 || os.Args[0] != helperName {
		return
	}
	var err error
	if len(os.Args) < 3 {
		err = fmt.Errorf("memory limit: helper started with %d arguments, want a program and its argv", len(os.Args)-1)
	} else {
		err = execLimited(raw, os.Args[1], os.Args[2:])
	}
	fmt.Fprintf(os.Stderr, "gremlins: %v\n", err)
	// The command never ran. Ending by a signal makes it read as a run that
	// reached no verdict, which is what it is, rather than as an exit status
	// a test could have chosen.
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	os.Exit(1) // not reached: SIGKILL is delivered before kill returns
}

// execLimited caps the running process's address space at raw bytes, never
// above the hard limit it already has, and executes path with args in its
// place. It returns only on failure.
//
// What it can build before the cap it builds before: the process has already
// reserved what its runtime reserves for an unlimited process, and under a
// tight cap any further mapping fails. Exec still converts its arguments to C
// strings after the cap; that is not done by hand because Exec also restores
// the RLIMIT_NOFILE the Go runtime raised, which the command must not
// inherit.
func execLimited(raw, path string, args []string) error {
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return fmt.Errorf("memory limit %q: %w", raw, err)
	}
	env := withoutLimit(os.Environ())
	var own unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_AS, &own); err != nil {
		return fmt.Errorf("memory limit: %w", err)
	}
	v = min(v, own.Max)
	if err := unix.Setrlimit(unix.RLIMIT_AS, &unix.Rlimit{Cur: v, Max: v}); err != nil {
		return fmt.Errorf("memory limit: %w", err)
	}
	if err := unix.Exec(path, args, env); err != nil {
		return fmt.Errorf("memory limit: exec %s: %w", path, err)
	}

	return nil
}
