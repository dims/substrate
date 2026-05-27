//  Copyright 2026 Google LLC
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package main

import (
	"reflect"
	"testing"
)

func TestResolveCapabilities(t *testing.T) {
	defaults := []string{"CAP_AUDIT_WRITE", "CAP_KILL", "CAP_NET_BIND_SERVICE"}

	cases := []struct {
		name string
		adds []string
		want []string
	}{
		{
			name: "nil adds yields defaults only",
			adds: nil,
			want: defaults,
		},
		{
			name: "empty adds yields defaults only",
			adds: []string{},
			want: defaults,
		},
		{
			name: "prefix-less names normalised and appended",
			adds: []string{"NET_ADMIN", "SETUID", "SETGID"},
			want: append(append([]string{}, defaults...), "CAP_NET_ADMIN", "CAP_SETUID", "CAP_SETGID"),
		},
		{
			name: "already-prefixed names accepted verbatim",
			adds: []string{"CAP_NET_ADMIN"},
			want: append(append([]string{}, defaults...), "CAP_NET_ADMIN"),
		},
		{
			name: "lowercase normalised to uppercase",
			adds: []string{"cap_net_admin", "setuid"},
			want: append(append([]string{}, defaults...), "CAP_NET_ADMIN", "CAP_SETUID"),
		},
		{
			name: "duplicates across defaults and adds collapse",
			adds: []string{"CAP_KILL", "NET_ADMIN", "CAP_NET_ADMIN"},
			want: append(append([]string{}, defaults...), "CAP_NET_ADMIN"),
		},
		{
			name: "blank entries ignored",
			adds: []string{"", "  ", "NET_ADMIN"},
			want: append(append([]string{}, defaults...), "CAP_NET_ADMIN"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveCapabilities(tc.adds)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("resolveCapabilities(%v) = %v, want %v", tc.adds, got, tc.want)
			}
		})
	}
}
