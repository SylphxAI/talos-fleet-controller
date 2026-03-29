/*
Copyright 2026 SylphxAI.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TalosSpec defines the desired Talos OS version and schematic.
type TalosSpec struct {
	// version is the desired Talos Linux version (e.g., "v1.12.5").
	// When this differs from the node's actual version, TFC triggers
	// a rolling upgrade via talosctl upgrade.
	// +required
	Version string `json:"version"`

	// schematic is the Talos Factory schematic ID for the installer image.
	// Includes system extensions (e.g., iscsi-tools, qemu-guest-agent).
	// +required
	Schematic string `json:"schematic"`

	// factoryURL is the Talos Factory base URL for pulling installer images.
	// +optional
	// +kubebuilder:default="https://factory.talos.dev"
	FactoryURL string `json:"factoryURL,omitempty"`
}

// NodeOverride defines per-node config patches for fields that differ
// between nodes (IP, MAC, hostname). Merged on top of the base machineConfig.
type NodeOverride struct {
	// nodeSelector selects which node(s) this override applies to.
	// Typically matches a single node by hostname.
	// +required
	NodeSelector *metav1.LabelSelector `json:"nodeSelector"`

	// machineConfig is a strategic merge patch applied on top of the base
	// machineConfig for matching nodes. Used for per-node fields like
	// hostname, MAC address, VLAN IP, IPv6 prefix.
	// +required
	MachineConfig MachineConfigSource `json:"machineConfig"`
}

// MachineConfigSource defines where to read the machine config patch from.
type MachineConfigSource struct {
	// inline is a raw YAML strategic merge patch applied to the machine config.
	// Cluster secrets (CA, etcd, SA key) are never touched — only machine.*
	// and cluster.controlPlane.endpoint fields are patched.
	// +optional
	Inline string `json:"inline,omitempty"`

	// secretRef references a Secret containing a machine config patch
	// under the key "machineConfig". Use this for configs containing
	// sensitive values (registry credentials, etc.).
	// +optional
	SecretRef *SecretReference `json:"secretRef,omitempty"`
}

// SecretReference is a reference to a Secret in the same namespace.
type SecretReference struct {
	// name is the name of the Secret.
	// +required
	Name string `json:"name"`
}

// UpdateStrategy controls how nodes are updated.
type UpdateStrategy struct {
	// maxUnavailable is the maximum number of nodes that can be
	// unavailable during an update. For control plane nodes, TFC
	// always verifies etcd quorum regardless of this setting.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	MaxUnavailable int `json:"maxUnavailable,omitempty"`

	// drainTimeout is the maximum duration to wait for workload drain
	// before proceeding. Respects PodDisruptionBudgets.
	// +optional
	// +kubebuilder:default="300s"
	DrainTimeout metav1.Duration `json:"drainTimeout,omitempty"`

	// healthCheckTimeout is the maximum duration to wait for a node
	// to become Ready after an update before marking it as failed.
	// +optional
	// +kubebuilder:default="120s"
	HealthCheckTimeout metav1.Duration `json:"healthCheckTimeout,omitempty"`

	// configApplyMode controls how machine config changes are applied.
	// - auto: live apply if possible, reboot if required (default)
	// - no-reboot: fail if reboot would be required
	// - reboot: always reboot after applying
	// - staged: save config, apply on next reboot
	// +optional
	// +kubebuilder:default="auto"
	// +kubebuilder:validation:Enum=auto;no-reboot;reboot;staged
	ConfigApplyMode string `json:"configApplyMode,omitempty"`
}

// MaintenanceWindow defines when updates are allowed to proceed.
type MaintenanceWindow struct {
	// schedule is a cron expression defining when updates may start.
	// Uses standard 5-field cron syntax (minute hour day month weekday).
	// +required
	Schedule string `json:"schedule"`

	// duration is how long the maintenance window stays open.
	// Updates in progress when the window closes will complete.
	// +required
	Duration metav1.Duration `json:"duration"`
}

// FleetNodeSetSpec defines the desired state of a set of Talos nodes.
type FleetNodeSetSpec struct {
	// nodeSelector selects which K8s nodes this FleetNodeSet manages.
	// Only nodes matching ALL labels are included.
	// +required
	NodeSelector *metav1.LabelSelector `json:"nodeSelector"`

	// talos defines the desired Talos OS version and schematic.
	// +required
	Talos TalosSpec `json:"talos"`

	// machineConfig is the base machine config patch applied to all
	// matching nodes. Per-node differences are handled by nodeOverrides.
	// +optional
	MachineConfig *MachineConfigSource `json:"machineConfig,omitempty"`

	// nodeOverrides are per-node config patches merged on top of the
	// base machineConfig. Use for node-specific fields (IP, MAC, hostname).
	// +optional
	NodeOverrides []NodeOverride `json:"nodeOverrides,omitempty"`

	// updateStrategy controls how updates are rolled out.
	// +optional
	UpdateStrategy UpdateStrategy `json:"updateStrategy,omitempty"`

	// maintenanceWindow restricts when updates may start.
	// If unset, updates proceed immediately.
	// +optional
	MaintenanceWindow *MaintenanceWindow `json:"maintenanceWindow,omitempty"`

	// paused stops all update operations. Nodes in progress will complete,
	// but no new updates will start. Use as an emergency brake.
	// +optional
	Paused bool `json:"paused,omitempty"`

	// dryRun computes diffs and reports status without applying changes.
	// +optional
	DryRun bool `json:"dryRun,omitempty"`
}

// NodePhase represents the update state of a single node.
// +kubebuilder:validation:Enum=Synced;Pending;Cordoning;Draining;Applying;Upgrading;HealthChecking;Failed
type NodePhase string

const (
	NodePhaseSynced         NodePhase = "Synced"
	NodePhasePending        NodePhase = "Pending"
	NodePhaseCordoning      NodePhase = "Cordoning"
	NodePhaseDraining       NodePhase = "Draining"
	NodePhaseApplying       NodePhase = "Applying"
	NodePhaseUpgrading      NodePhase = "Upgrading"
	NodePhaseHealthChecking NodePhase = "HealthChecking"
	NodePhaseFailed         NodePhase = "Failed"
)

// FleetNodeStatus represents the observed state of a single managed node.
type FleetNodeStatus struct {
	// name is the K8s Node name.
	Name string `json:"name"`

	// phase is the current update phase of this node.
	Phase NodePhase `json:"phase"`

	// currentVersion is the actual Talos version running on this node.
	// +optional
	CurrentVersion string `json:"currentVersion,omitempty"`

	// desiredVersion is the Talos version this node should be running.
	// +optional
	DesiredVersion string `json:"desiredVersion,omitempty"`

	// currentConfigHash is a SHA-256 hash of the node's actual machine config.
	// +optional
	CurrentConfigHash string `json:"currentConfigHash,omitempty"`

	// desiredConfigHash is a SHA-256 hash of the desired machine config.
	// +optional
	DesiredConfigHash string `json:"desiredConfigHash,omitempty"`

	// lastAppliedConfigHash is the hash of the node's config AFTER our last
	// successful apply. Used for drift detection: if currentHash matches this,
	// the config hasn't changed since we last applied → synced.
	// This avoids false drift from serialization differences between
	// configpatcher output and Talos internal normalization.
	// +optional
	LastAppliedConfigHash string `json:"lastAppliedConfigHash,omitempty"`

	// lastAppliedGeneration is the FleetNodeSet generation that was last
	// successfully applied to this node. If the generation advances,
	// we re-apply regardless of hash match.
	// +optional
	LastAppliedGeneration int64 `json:"lastAppliedGeneration,omitempty"`

	// message provides human-readable detail about the current phase.
	// +optional
	Message string `json:"message,omitempty"`

	// lastApplied is the timestamp of the last successful config apply or upgrade.
	// +optional
	LastApplied *metav1.Time `json:"lastApplied,omitempty"`
}

// FleetPhase represents the aggregate state of the FleetNodeSet.
// +kubebuilder:validation:Enum=Synced;Progressing;Degraded;Paused
type FleetPhase string

const (
	FleetPhaseSynced      FleetPhase = "Synced"
	FleetPhaseProgressing FleetPhase = "Progressing"
	FleetPhaseDegraded    FleetPhase = "Degraded"
	FleetPhasePaused      FleetPhase = "Paused"
)

// FleetNodeSetStatus defines the observed state of FleetNodeSet.
type FleetNodeSetStatus struct {
	// phase is the aggregate state of all managed nodes.
	// +optional
	Phase FleetPhase `json:"phase,omitempty"`

	// observedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// total is the number of nodes matching the nodeSelector.
	Total int `json:"total"`

	// synced is the number of nodes at desired version + config.
	Synced int `json:"synced"`

	// progressing is the number of nodes currently being updated.
	Progressing int `json:"progressing"`

	// pending is the number of nodes waiting to be updated.
	Pending int `json:"pending"`

	// failed is the number of nodes that failed to update.
	Failed int `json:"failed"`

	// nodes contains per-node status details.
	// +optional
	Nodes []FleetNodeStatus `json:"nodes,omitempty"`

	// conditions represent the current state of the FleetNodeSet.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Total",type="integer",JSONPath=".status.total"
// +kubebuilder:printcolumn:name="Synced",type="integer",JSONPath=".status.synced"
// +kubebuilder:printcolumn:name="Progressing",type="integer",JSONPath=".status.progressing"
// +kubebuilder:printcolumn:name="Talos",type="string",JSONPath=".spec.talos.version"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// FleetNodeSet is the Schema for the fleetnodesets API.
// It declares the desired Talos OS version and machine configuration
// for a set of nodes. The TFC controller reconciles actual state to
// match, performing sequential upgrades and config applies with
// safety guarantees (etcd quorum, drain, health checks).
type FleetNodeSet struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state.
	// +required
	Spec FleetNodeSetSpec `json:"spec"`

	// status defines the observed state.
	// +optional
	Status FleetNodeSetStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// FleetNodeSetList contains a list of FleetNodeSet.
type FleetNodeSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []FleetNodeSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FleetNodeSet{}, &FleetNodeSetList{})
}
