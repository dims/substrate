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

// The sandbox server runs inside an actor and executes commands on behalf of
// a client that reaches it through the atenet router.
//
// It serves two contracts on one port:
//
//   - POST /process is the original REPL contract used by ./client.
//   - GET /readyz, POST /exec, POST /upload and GET /download form the sandbox
//     agent contract the NeMo Gym Substrate provider speaks. The binary is
//     static, so an ActorTemplate can mount this image as an image volume into
//     a workload image that was built by someone else and run it as that
//     container's command.
//
// The server executes whatever it is asked to and performs no authorization.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// ProcessRequest is the /process contract: an argv, not a shell string.
type ProcessRequest struct {
	Command []string          `json:"command"`
	EnvVars map[string]string `json:"envvars,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Timeout string            `json:"timeout,omitempty"`
}

// ProcessResponse is the /process reply.
type ProcessResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
	Error    string `json:"error,omitempty"`
}

// ExecRequest is the /exec contract: one shell string, run by the sandbox
// shell, the way the NeMo Gym sandbox SDK defines exec.
type ExecRequest struct {
	Command  string            `json:"command"`
	Cwd      string            `json:"cwd,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	User     string            `json:"user,omitempty"`
	TimeoutS float64           `json:"timeout_s,omitempty"`
}

// ExecResponse is the /exec reply. A nonzero ExitCode with an empty ErrorType
// is the command's own exit status. ErrorType "timeout" or "sandbox" means
// the command did not run to completion, and ExitCode is then
// runtimeExitCode. Evicting is set on the first reply after the worker has
// announced that it is going away, so the client can have the actor
// suspended in time. It is reported once: the flag would otherwise survive in
// the memory snapshot and follow the actor to its next worker.
type ExecResponse struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code"`
	ErrorType string `json:"error_type,omitempty"`
	Evicting  bool   `json:"evicting,omitempty"`
}

// evicting records that the worker sent SIGTERM: its pod is going away and
// the actor must be suspended within the grace period.
var evicting atomic.Bool

const (
	// runtimeExitCode is reported when no process exit status exists: the
	// command never started or was killed by the agent. It mirrors the
	// sentinel the NeMo Gym sandbox SDK uses.
	runtimeExitCode = 125

	// maxOutputBytes caps each of stdout and stderr. Rollout logs that read
	// a whole repository would otherwise be sent back through the router.
	maxOutputBytes = 8 << 20

	// waitDelay bounds how long Wait keeps collecting output after the context
	// ends. The process group is killed at once (cmd.Cancel); a descendant that
	// left the group and still holds the output pipes is abandoned after this.
	waitDelay = 5 * time.Second
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "80"
	}
	evictions := make(chan os.Signal, 1)
	signal.Notify(evictions, syscall.SIGTERM)
	go logEvictions(evictions)
	log.Printf("sandbox agent serving on port %s (shell %q)", port, shellPath())
	log.Fatal(http.ListenAndServe(":"+port, newMux()))
}

// logEvictions keeps the server alive through an eviction notice. The worker
// sends SIGTERM to the actor's main process when its pod is going away, and
// the actor then has a grace period in which it can still be suspended; a
// process that exits cannot be. Only the client can ask for the suspend, so
// the server records the signal, reports it on the next /exec reply, and
// keeps serving.
func logEvictions(signals <-chan os.Signal) {
	for sig := range signals {
		evicting.Store(true)
		log.Printf("received %v: worker is being evicted; continuing to serve so the actor can be suspended", sig)
	}
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", handleReadyz)
	mux.HandleFunc("POST /process", handleProcess)
	mux.HandleFunc("POST /exec", handleExec)
	mux.HandleFunc("POST /upload", handleUpload)
	mux.HandleFunc("GET /download", handleDownload)
	return mux
}

// shellPath picks the shell /exec runs commands under. SANDBOX_SHELL wins;
// otherwise bash when the workload image has it, since SWE-bench harnesses
// source conda scripts that dash cannot run; otherwise sh.
func shellPath() string {
	if s := os.Getenv("SANDBOX_SHELL"); s != "" {
		return s
	}
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	return "sh"
}

func handleReadyz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, "ok\n")
}

func handleProcess(w http.ResponseWriter, r *http.Request) {
	var req ProcessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	if len(req.Command) == 0 {
		http.Error(w, "Command required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	if req.Timeout != "" {
		duration, err := time.ParseDuration(req.Timeout)
		if err != nil {
			http.Error(w, fmt.Sprintf("Invalid timeout: %v", err), http.StatusBadRequest)
			return
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, duration)
		defer cancel()
	}

	result := run(ctx, req.Command[0], req.Command[1:], req.Cwd, req.EnvVars)
	writeJSON(w, ProcessResponse{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode, Error: result.ErrorType})
}

func handleExec(w http.ResponseWriter, r *http.Request) {
	var req ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		http.Error(w, "command required", http.StatusBadRequest)
		return
	}
	// Every container in an actor runs as one fixed identity, so there is no
	// user to switch to. Refuse rather than run as the wrong one.
	switch req.User {
	case "", "root", "0":
	default:
		http.Error(w, fmt.Sprintf("user %q is not supported: the actor runs as one fixed identity", req.User), http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	if req.TimeoutS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutS*float64(time.Second)))
		defer cancel()
	}

	resp := run(ctx, shellPath(), []string{"-c", req.Command}, req.Cwd, req.Env)
	resp.Evicting = evicting.Swap(false)
	writeJSON(w, resp)
}

// run executes argv and reports what happened in the /exec shape. The process
// gets its own group so that a timeout kills the children a shell forked too.
func run(ctx context.Context, name string, args []string, cwd string, env map[string]string) ExecResponse {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = waitDelay

	stdout := &cappedBuffer{limit: maxOutputBytes}
	stderr := &cappedBuffer{limit: maxOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	resp := ExecResponse{Stdout: stdout.String(), Stderr: stderr.String()}

	switch {
	case err == nil:
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		resp.ExitCode = runtimeExitCode
		resp.ErrorType = "timeout"
		resp.Stderr += "\n[command timed out]"
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			resp.ExitCode = exitErr.ExitCode()
		} else {
			// The command never ran: bad cwd, missing interpreter, context
			// canceled by the client going away.
			resp.ExitCode = runtimeExitCode
			resp.ErrorType = "sandbox"
			resp.Stderr += "\n" + err.Error()
		}
	}
	resp.Stderr = strings.TrimPrefix(resp.Stderr, "\n")
	return resp
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path query parameter required", http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, copyErr := io.Copy(f, r.Body)
	closeErr := f.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path query parameter required", http.StatusBadRequest)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if info.IsDir() {
		http.Error(w, "path is a directory", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(info.Size()))
	_, _ = io.Copy(w, f)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writing response: %v", err)
	}
}

// cappedBuffer keeps the first limit bytes written and notes that the rest
// was dropped.
type cappedBuffer struct {
	buf       strings.Builder
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	room := b.limit - b.buf.Len()
	if room <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		b.truncated = true
		p = p[:room]
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	if b.truncated {
		return b.buf.String() + "\n[output truncated]"
	}
	return b.buf.String()
}
