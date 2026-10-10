// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeExec(t *testing.T, resp *http.Response) ExecResponse {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out ExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReadyz(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestExecRunsShellCommandWithCwdAndEnv(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()
	// macOS temp dirs live behind a symlink that pwd resolves.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	out := decodeExec(t, postJSON(t, srv.URL+"/exec", ExecRequest{
		Command: "echo \"$GREETING from $(pwd)\"; echo oops >&2; exit 3",
		Cwd:     dir,
		Env:     map[string]string{"GREETING": "hello"},
	}))
	if out.ExitCode != 3 || out.ErrorType != "" {
		t.Fatalf("exit=%d error_type=%q", out.ExitCode, out.ErrorType)
	}
	if want := "hello from " + dir; !strings.Contains(out.Stdout, want) {
		t.Fatalf("stdout %q lacks %q", out.Stdout, want)
	}
	if strings.TrimSpace(out.Stderr) != "oops" {
		t.Fatalf("stderr %q", out.Stderr)
	}
}

func TestExecTimeoutKillsAndReportsSentinel(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()

	out := decodeExec(t, postJSON(t, srv.URL+"/exec", ExecRequest{Command: "sleep 30", TimeoutS: 0.2}))
	if out.ExitCode != runtimeExitCode || out.ErrorType != "timeout" {
		t.Fatalf("exit=%d error_type=%q stderr=%q", out.ExitCode, out.ErrorType, out.Stderr)
	}
}

func TestExecTimeoutKillsBackgroundChildren(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()
	marker := filepath.Join(t.TempDir(), "survivor")

	// The shell forks a child and then waits; killing only the shell would
	// leave the child to write the marker after the timeout.
	out := decodeExec(t, postJSON(t, srv.URL+"/exec", ExecRequest{
		Command:  "(sleep 1; touch " + marker + ") & sleep 30",
		TimeoutS: 0.2,
	}))
	if out.ErrorType != "timeout" {
		t.Fatalf("error_type %q", out.ErrorType)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("background child survived the timeout")
	}
}

func TestExecMissingCwdIsSandboxError(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()

	out := decodeExec(t, postJSON(t, srv.URL+"/exec", ExecRequest{Command: "true", Cwd: "/no/such/dir"}))
	if out.ExitCode != runtimeExitCode || out.ErrorType != "sandbox" {
		t.Fatalf("exit=%d error_type=%q stderr=%q", out.ExitCode, out.ErrorType, out.Stderr)
	}
}

func TestExecRefusesOtherUsers(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/exec", ExecRequest{Command: "id", User: "nobody"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestUploadThenDownloadRoundTrip(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "nested", "seed.txt")

	resp, err := http.Post(srv.URL+"/upload?path="+path, "application/octet-stream", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("upload status %d", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/download?path=" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "payload" {
		t.Fatalf("download status %d body %q", resp.StatusCode, body)
	}
}

func TestDownloadMissingIs404(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/download?path=" + filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestProcessContractStillWorks(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()
	resp := postJSON(t, srv.URL+"/process", ProcessRequest{Command: []string{"sh", "-c", "exit 2"}})
	defer resp.Body.Close()
	var out ProcessResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 2 {
		t.Fatalf("exit %d", out.ExitCode)
	}
}

func TestCappedBufferTruncates(t *testing.T) {
	b := &cappedBuffer{limit: 4}
	_, _ = b.Write([]byte("abcdef"))
	if got := b.String(); got != "abcd\n[output truncated]" {
		t.Fatalf("%q", got)
	}
}

func TestEvictionSignalDoesNotStopServing(t *testing.T) {
	signals := make(chan os.Signal, 1)
	done := make(chan struct{})
	go func() {
		logEvictions(signals)
		close(done)
	}()
	signals <- syscall.SIGTERM
	close(signals)
	<-done

	t.Cleanup(func() { evicting.Store(false) })

	srv := httptest.NewServer(newMux())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d after eviction signal", resp.StatusCode)
	}
	// The next reply carries the notice so the client can suspend the actor;
	// later replies do not, so a restored actor is not told again.
	out := decodeExec(t, postJSON(t, srv.URL+"/exec", ExecRequest{Command: "true"}))
	if !out.Evicting {
		t.Fatal("exec reply does not report the eviction")
	}
	if again := decodeExec(t, postJSON(t, srv.URL+"/exec", ExecRequest{Command: "true"})); again.Evicting {
		t.Fatal("eviction reported twice")
	}
}
