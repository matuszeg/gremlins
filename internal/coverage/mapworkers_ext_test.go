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

package coverage_test

import (
	"os/exec"
	"testing"

	"github.com/spf13/viper"

	"github.com/go-gremlins/gremlins/internal/configuration"
	"github.com/go-gremlins/gremlins/internal/coverage"
	"github.com/go-gremlins/gremlins/internal/gomodule"
)

// A cold map runs one test at a time unless told otherwise: concurrent test
// processes multiply the memory and database connections a suite uses, and
// only its owner knows what it can take.
func TestMapWorkersDefaultsToOne(t *testing.T) {
	c := coverage.NewWithCmd(exec.Command, t.TempDir(), gomodule.GoModule{})
	if got := coverage.MapWorkersOf(c); got != 1 {
		t.Errorf("want 1, got %d", got)
	}
}

// Zero or less is one, never "as many as there are tests".
func TestMapWorkersBelowOneIsOne(t *testing.T) {
	t.Cleanup(func() { viper.Set(configuration.UnleashMapWorkersKey, nil) })
	for _, n := range []int{0, -3} {
		viper.Set(configuration.UnleashMapWorkersKey, n)
		c := coverage.NewWithCmd(exec.Command, t.TempDir(), gomodule.GoModule{})
		if got := coverage.MapWorkersOf(c); got != 1 {
			t.Errorf("map-workers %d: want 1, got %d", n, got)
		}
	}
}

func TestMapWorkersIsRead(t *testing.T) {
	t.Cleanup(func() { viper.Set(configuration.UnleashMapWorkersKey, nil) })
	viper.Set(configuration.UnleashMapWorkersKey, 4)
	c := coverage.NewWithCmd(exec.Command, t.TempDir(), gomodule.GoModule{})
	if got := coverage.MapWorkersOf(c); got != 4 {
		t.Errorf("want 4, got %d", got)
	}
}

func TestWithMapWorkersHoldsAFloorOfOne(t *testing.T) {
	c := coverage.NewWithCmd(exec.Command, t.TempDir(), gomodule.GoModule{}, coverage.WithMapWorkers(0))
	if got := coverage.MapWorkersOf(c); got != 1 {
		t.Errorf("want 1, got %d", got)
	}
	c = coverage.NewWithCmd(exec.Command, t.TempDir(), gomodule.GoModule{}, coverage.WithMapWorkers(3))
	if got := coverage.MapWorkersOf(c); got != 3 {
		t.Errorf("want 3, got %d", got)
	}
}
