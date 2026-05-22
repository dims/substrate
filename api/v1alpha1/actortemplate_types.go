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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type PhaseType string

// Define your phases as constants
const (
	PhaseInitial           PhaseType = ""
	PhaseResumeGoldenActor PhaseType = "ResumeGoldenActor"
	PhaseWaitGoldenActor   PhaseType = "WaitGoldenActor"
	PhaseReady             PhaseType = "Ready"
	PhaseFailed            PhaseType = "Failed"
)

// A single application container that you want to run within a WorkerPool.
type Container struct {
	// Name of the container.
	// +required
	Name string `json:"name"`

	// Image to use for the worker replicas.
	Image string `json:"image,omitempty"`

	// Entrypoint array. Not executed within a shell.
	// +optional
	// +listType=atomic
	Command []string `json:"command,omitempty"`

	// List of ports to expose from the container.
	Ports []corev1.ContainerPort `json:"ports,omitempty"`

	// Environment variables to set in the worker replicas.
	Env []corev1.EnvVar `json:"env,omitempty"`

	// SecurityContext holds Substrate-honored security settings for the
	// container. Workloads such as NVIDIA OpenShell's `openshell-sandbox`
	// supervisor require additional capabilities (`CAP_NET_ADMIN`,
	// `CAP_SETUID`, `CAP_SETGID`) on top of the small default set
	// (`CAP_AUDIT_WRITE`, `CAP_KILL`, `CAP_NET_BIND_SERVICE`). Setting
	// this is opt-in per container.
	//
	// Only `Capabilities.Add` is honored today; `Drop` is reserved for a
	// follow-up that introduces a per-template default allow-list.
	//
	// +optional
	SecurityContext *ContainerSecurityContext `json:"securityContext,omitempty"`
}

// ContainerSecurityContext is the Substrate subset of K8s `SecurityContext`.
//
// Substrate intentionally does not expose the full K8s shape because gVisor
// implements user/group/MAC primitives differently from the host kernel and
// because the actor lifecycle (checkpoint/restore) constrains what
// security-state can be mutated across the snapshot boundary. Fields here
// are the ones atelet's OCI bundle builder can honor without violating
// either constraint.
type ContainerSecurityContext struct {
	// Capabilities adjustments applied on top of the default sandbox set.
	// +optional
	Capabilities *Capabilities `json:"capabilities,omitempty"`
}

// Capabilities mirrors `corev1.Capabilities` but with the field types
// kept primitive so the same shape can be carried verbatim through the
// `ateletpb` / `ateompb` protos without an additional conversion layer.
type Capabilities struct {
	// Capabilities to grant in addition to the default set. Each entry
	// is a Linux capability name with or without the `CAP_` prefix
	// (e.g. `NET_ADMIN` or `CAP_NET_ADMIN`).
	// +optional
	// +listType=atomic
	Add []string `json:"add,omitempty"`

	// Capabilities to drop from the default set. Currently unused; the
	// default set is small enough that drops do not change behavior.
	// Reserved for a per-template default allow-list.
	// +optional
	// +listType=atomic
	Drop []string `json:"drop,omitempty"`
}

type SnapshotsConfig struct {
	// Location to store snapshots in.
	// +required
	Location string `json:"location"`
}

// ActorTemplateSpec defined desired spec of an actor.
type ActorTemplateSpec struct {
	// PauseImage is the container to use as the root sandbox container.
	//
	// Typically, set it to [1] for on-gcp, and [2] for off-gcp
	//
	//   - [1] gcr.io/gke-release/pause@sha256:bcbd57ba5653580ec647b16d8163cdd1112df3609129b01f912a8032e48265da
	//   - [2] registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4
	//
	// +required
	PauseImage string `json:"pauseImage,omitempty"`

	// Containers is the workload definition.
	//
	// +optional
	Containers []Container `json:"containers,omitempty"`

	// Snapshots configuration for the actor.
	// +required
	SnapshotsConfig SnapshotsConfig `json:"snapshotsConfig"`

	// Name of the worker pool to use for the actor.
	// +required
	WorkerPoolRef corev1.ObjectReference `json:"workerPoolRef"`

	// Parameters for fetching the runsc binary to use.
	//
	// +required
	Runsc RunscConfig `json:"runsc,omitempty"`
}

type GCPAuthenticationConfig struct {
}

// Authentication configuration for atelet to download static files.
//
// If no members are set, then atelet will use anonymous authentication.
type AuthenticationConfig struct {
	// Use GCP application-default credentials.
	GCP *GCPAuthenticationConfig `json:"gcp,omitempty"`
}

type RunscPlatformConfig struct {
	// The SHA256 hash of the binary to download.  Used both to name the
	// downloaded file (for preventing conflicts), and to check the integrity of
	// the downloaded file.
	//
	// +required
	SHA256Hash string `json:"sha256Hash,omitempty"`

	// A gs:// URL pointing to a runsc binary that can be downloaded (possibly
	// with atelet's credentials).
	//
	// +required
	URL string `json:"url,omitempty"`
}

type RunscConfig struct {
	// Configuration for the amd64 binary.
	//
	// +optional
	AMD64 *RunscPlatformConfig `json:"amd64,omitempty"`

	// Configuration for the arm64 binary.
	//
	// +optional
	ARM64 *RunscPlatformConfig `json:"arm64,omitempty"`

	// How should atelet authenticate to download the runsc binary?
	Authentication AuthenticationConfig `json:"authentication,omitempty"`
}

type ActorTemplateStatus struct {
	// Phase of the actor template.
	// +optional
	Phase PhaseType `json:"phase,omitempty"`

	GoldenActorID        string      `json:"goldenActorID,omitempty"`
	TakeGoldenSnapshotAt metav1.Time `json:"takeGoldenSnapshotAt,omitempty"`
	GoldenSnapshot       string      `json:"goldenSnapshot,omitempty"`

	// conditions defines the status conditions array
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +genclient
// +kubebuilder:object:generate=true
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=actortemplate
// +kubebuilder:subresource:status
type ActorTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec defines the desired state of ActorTemplate
	// +required
	Spec ActorTemplateSpec `json:"spec"`

	// status is the observed state of ActorTemplate
	// +optional
	Status ActorTemplateStatus `json:"status,omitempty"`
}

// ActorTemplateList contains a list of ActorTemplates.
// +kubebuilder:object:generate=true
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=actortemplate
type ActorTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ActorTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ActorTemplate{}, &ActorTemplateList{})
}
