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

func TestReadBoundedForCache(t *testing.T) {
	tests := []struct {
		name       string
		content    []byte
		limit      int64
		wantCached bool
	}{
		{"below limit", blob(50), 100, true},
		{"equal to limit", blob(100), 100, true},
		{"one over limit", blob(101), 100, false},
		{"far over limit", blob(500), 100, false},
		{"empty with zero limit", blob(0), 0, true},
		{"one byte with zero limit", blob(1), 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &fakeReadCloser{r: bytes.NewReader(tt.content)}
			cacheData, body, err := readBoundedForCache(src, tt.limit)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// The caller must always receive the full image content exactly once,
			// regardless of whether it was cached.
			got, err := io.ReadAll(body)
			if err != nil {
				t.Fatalf("reading body: %v", err)
			}
			if !bytes.Equal(got, tt.content) {
				t.Fatalf("body yielded %d bytes, want the full %d", len(got), len(tt.content))
			}

			if tt.wantCached {
				if !bytes.Equal(cacheData, tt.content) {
					t.Errorf("cacheData = %d bytes, want full content %d", len(cacheData), len(tt.content))
				}
				if !src.closed {
					t.Error("source should be closed on the cacheable path")
				}
				return
			}

			if cacheData != nil {
				t.Errorf("expected nil cacheData on the streaming path, got %d bytes", len(cacheData))
			}
			// On the streaming path the source is closed by the caller via body.
			if err := body.Close(); err != nil {
				t.Errorf("body.Close: %v", err)
			}
			if !src.closed {
				t.Error("source should be closed via body.Close on the streaming path")
			}
		})
	}
}

func TestReadBoundedForCacheReadError(t *testing.T) {
	src := &fakeReadCloser{r: errReader{}}
	cacheData, body, err := readBoundedForCache(src, 100)
	if err == nil {
		t.Fatal("expected an error from a failing reader")
	}
	if cacheData != nil || body != nil {
		t.Error("expected nil results on error")
	}
	if !src.closed {
		t.Error("source should be closed on read error")
	}
}
