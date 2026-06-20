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

// Command compliance is the actor used by the sandbox compliance suite
// (internal/e2e/suites/compliance). It exposes one endpoint per property that
// any ateom sandbox backend must preserve, so the suite can check each one
// independently across a suspend/resume and a pause/resume:
//
//   - /whoami : the per-actor identity read from /run/ate/actor-id.
//   - /mem    : an in-memory counter, to prove RAM is checkpointed and restored.
//   - /fs     : an on-disk counter, to prove the writable filesystem survives.
//
// /mem and /fs increment and return the new value on every call. A value that
// fails to advance across a restore means the backend lost that state.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
)

const (
	// identityFile is the per-actor identity atelet bind-mounts read-only.
	identityFile = "/run/ate/actor-id"
	// fsStateFile lives on the writable rootfs (not a tmpfs), so it is captured
	// in the snapshot's filesystem delta the same way the counter demo's file is.
	fsStateFile = "/compliance-fs-count"
	addr        = ":80"
)

var (
	mu     sync.Mutex
	memVal int
)

// memHandler increments an in-process counter. After a restore it must continue
// from the value captured at checkpoint, not reset to zero.
func memHandler(w http.ResponseWriter, _ *http.Request) {
	mu.Lock()
	memVal++
	v := memVal
	mu.Unlock()
	writeJSON(w, map[string]int{"value": v})
}

// fsHandler increments a counter stored in a file on the writable rootfs. After
// a restore the file must still hold the value written before the checkpoint.
func fsHandler(w http.ResponseWriter, _ *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	v := 0
	if b, err := os.ReadFile(fsStateFile); err == nil {
		v, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	v++
	if err := os.WriteFile(fsStateFile, []byte(strconv.Itoa(v)), 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]int{"value": v})
}

// whoamiHandler reports the actor's own identity, read fresh on each request
// from the bind-mounted file. A read error is returned rather than swallowed so
// a failing assertion explains itself.
func whoamiHandler(w http.ResponseWriter, _ *http.Request) {
	host, _ := os.Hostname()
	resp := map[string]string{"hostname": host}
	if b, err := os.ReadFile(identityFile); err == nil {
		resp["file"] = string(b)
	} else {
		resp["file"] = ""
		resp["error"] = err.Error()
	}
	writeJSON(w, resp)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("compliance: encoding response: %v", err)
	}
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/whoami", whoamiHandler)
	mux.HandleFunc("/mem", memHandler)
	mux.HandleFunc("/fs", fsHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	log.Printf("compliance actor listening on %s", addr)
	if err := (&http.Server{Addr: addr, Handler: mux}).ListenAndServe(); err != nil {
		log.Fatalf("compliance server: %v", err)
	}
}
