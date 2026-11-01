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
	"net"
)

// retryRead runs read, a single-row SELECT, once more when its connection
// timed out while ctx is live. pgconn reports such a timeout as a net.Error
// with Timeout() around the socket read, not as ctx's error, so the query never
// finished and a second run on another pooled connection is safe. Any other
// error, or a timeout once ctx has ended, is returned as is.
func retryRead(ctx context.Context, read func() error) error {
	err := read()
	if err == nil || ctx.Err() != nil || !isConnTimeout(err) {
		return err
	}
	return read()
}

func isConnTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
