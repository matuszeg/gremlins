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

package engine_test

import (
	"strings"
	"testing"

	"github.com/go-gremlins/gremlins/internal/configuration"
	"github.com/go-gremlins/gremlins/internal/engine"
	"github.com/go-gremlins/gremlins/internal/memlimit"
)

func TestTestMemoryLimitSetting(t *testing.T) {
	testCases := map[string]struct {
		set     any
		want    memlimit.Limit
		wantErr bool
	}{
		"unset":     {set: nil, want: 0},
		"bytes":     {set: "2621440000", want: 2500 << 20},
		"suffixed":  {set: "2500M", want: 2500 << 20},
		"malformed": {set: "2.5X", wantErr: true},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			viperSet(map[string]any{})
			if tc.set != nil {
				configuration.Set(configuration.UnleashTestMemoryLimitKey, tc.set)
			}
			defer viperReset()
			got, err := engine.TestMemoryLimit()
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "--test-memory-limit") {
					t.Errorf("err = %v, want one naming --test-memory-limit", err)
				}

				return
			}
			if err != nil || got != tc.want {
				t.Errorf("TestMemoryLimit() = %d, %v, want %d", got, err, tc.want)
			}
		})
	}
}
