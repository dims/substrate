// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package atepg

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// readTimeout is what pgconn wraps when a socket read hits its deadline.
type readTimeout struct{}

func (readTimeout) Error() string   { return "read tcp 10.0.0.8:39868->10.0.0.7:5432: i/o timeout" }
func (readTimeout) Timeout() bool   { return true }
func (readTimeout) Temporary() bool { return true }

func TestRetryRead(t *testing.T) {
	t.Parallel()
	timeout := fmt.Errorf("timeout: %w", readTimeout{})
	other := errors.New("relation does not exist")
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tc := range []struct {
		name      string
		ctx       context.Context
		errs      []error
		wantErr   error
		wantCalls int
	}{
		{name: "first read succeeds", ctx: t.Context(), errs: []error{nil}, wantCalls: 1},
		{name: "timeout then success", ctx: t.Context(), errs: []error{timeout, nil}, wantCalls: 2},
		{name: "timeout twice", ctx: t.Context(), errs: []error{timeout, timeout}, wantErr: timeout, wantCalls: 2},
		{name: "other error", ctx: t.Context(), errs: []error{other}, wantErr: other, wantCalls: 1},
		{name: "timeout after ctx ended", ctx: canceled, errs: []error{timeout}, wantErr: timeout, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			err := retryRead(tc.ctx, func() error {
				err := tc.errs[calls]
				calls++
				return err
			})
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("retryRead() = %v, want %v", err, tc.wantErr)
			}
			if calls != tc.wantCalls {
				t.Errorf("read called %d times, want %d", calls, tc.wantCalls)
			}
		})
	}
}
