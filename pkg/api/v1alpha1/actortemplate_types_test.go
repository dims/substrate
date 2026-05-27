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

package v1alpha1

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestActorTemplateDeepCopy(t *testing.T) {
	in := &ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template",
			Namespace: "test-ns",
		},
		Spec: ActorTemplateSpec{
			WorkerPoolRef: corev1.ObjectReference{
				Namespace: "test-ns",
				Name:      "test-pool",
			},
			SnapshotsConfig: SnapshotsConfig{
				Location: "gs://test-bucket/test-folder",
			},
		},
		Status: ActorTemplateStatus{
			Phase:          PhaseReady,
			GoldenSnapshot: "gs://test-bucket/test-folder/golden",
		},
	}

	out := in.DeepCopy()

	if diff := cmp.Diff(in, out); diff != "" {
		t.Errorf("DeepCopy() mismatch (-want +got):\n%s", diff)
	}
}

func TestContainerSecurityContextDeepCopy(t *testing.T) {
	in := &ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template",
			Namespace: "test-ns",
		},
		Spec: ActorTemplateSpec{
			WorkerPoolRef: corev1.ObjectReference{
				Namespace: "test-ns",
				Name:      "test-pool",
			},
			SnapshotsConfig: SnapshotsConfig{
				Location: "gs://test-bucket/test-folder",
			},
			Containers: []Container{
				{
					Name:  "app",
					Image: "registry.example/app:test",
					SecurityContext: &ContainerSecurityContext{
						Capabilities: &Capabilities{
							Add: []string{"NET_ADMIN", "SETUID", "SETGID"},
						},
						RunAsUser:  ptrInt64(1000),
						RunAsGroup: ptrInt64(1000),
					},
				},
			},
		},
	}

	out := in.DeepCopy()

	if diff := cmp.Diff(in, out); diff != "" {
		t.Errorf("DeepCopy() mismatch (-want +got):\n%s", diff)
	}

	// Mutating the copy must not bleed into the original.
	out.Spec.Containers[0].SecurityContext.Capabilities.Add[0] = "MUTATED"
	if got := in.Spec.Containers[0].SecurityContext.Capabilities.Add[0]; got != "NET_ADMIN" {
		t.Errorf("DeepCopy did not isolate Capabilities.Add slice: original now %q", got)
	}
	*out.Spec.Containers[0].SecurityContext.RunAsUser = 9999
	if got := *in.Spec.Containers[0].SecurityContext.RunAsUser; got != 1000 {
		t.Errorf("DeepCopy did not isolate RunAsUser pointer: original now %d", got)
	}
}

func ptrInt64(v int64) *int64 { return &v }
