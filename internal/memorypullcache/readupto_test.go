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

package memorypullcache

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

type fakeReadCloser struct {
	r      io.Reader
	closed bool
}

func (f *fakeReadCloser) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *fakeReadCloser) Close() error               { f.closed = true; return nil }

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestReadUpTo(t *testing.T) {
	tests := []struct {
		name     string
		content  []byte
		limit    int64
		wantMore bool
	}{
		{"below limit", blob(50), 100, false},
		{"equal to limit", blob(100), 100, false},
		{"one over limit", blob(101), 100, true},
		{"far over limit", blob(500), 100, true},
		{"empty with zero limit", blob(0), 0, false},
		{"one byte with zero limit", blob(1), 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &fakeReadCloser{r: bytes.NewReader(tt.content)}
			buf, more, err := readUpTo(src, tt.limit)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if more != tt.wantMore {
				t.Fatalf("more = %v, want %v", more, tt.wantMore)
			}

			// Reconstruct the full content the caller would observe and confirm
			// it equals the original image exactly once, in both branches.
			var full []byte
			if more {
				// Streaming branch: caller reads buf, then the remainder of src.
				if src.closed {
					t.Error("source must stay open on the streaming branch")
				}
				rest, err := io.ReadAll(src)
				if err != nil {
					t.Fatalf("reading remainder: %v", err)
				}
				full = append(append([]byte{}, buf...), rest...)
				if int64(len(buf)) != tt.limit+1 {
					t.Errorf("buffered prefix = %d bytes, want limit+1 = %d", len(buf), tt.limit+1)
				}
			} else {
				if !src.closed {
					t.Error("source must be closed on the buffered branch")
				}
				full = buf
			}
			if !bytes.Equal(full, tt.content) {
				t.Fatalf("reconstructed %d bytes, want the full %d", len(full), len(tt.content))
			}
		})
	}
}

func TestReadUpToReadError(t *testing.T) {
	src := &fakeReadCloser{r: errReader{}}
	buf, more, err := readUpTo(src, 100)
	if err == nil {
		t.Fatal("expected an error from a failing reader")
	}
	if buf != nil || more {
		t.Error("expected nil buf and more=false on error")
	}
	if !src.closed {
		t.Error("source should be closed on read error")
	}
}
