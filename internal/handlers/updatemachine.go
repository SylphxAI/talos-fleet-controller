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

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"

	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"

	"github.com/SylphxAI/talos-fleet-controller/internal/talos"
)

const (
	// retryIntervalSeconds is how long CAPI should wait before re-calling
	// UpdateMachine while we are still applying the config.
	retryIntervalSeconds = 15

	// updateTimeoutDuration is the maximum time we wait for a node to
	// finish applying config before declaring failure.
	updateTimeoutDuration = 5 * time.Minute

	// controlPlaneLabel is the CAPI Machine label for control plane nodes.
	// Note: this is the CAPI label (cluster.x-k8s.io/control-plane), NOT the
	// K8s node role label (node-role.kubernetes.io/control-plane).
	// UpdateMachine receives CAPI Machine objects, not K8s Nodes.
	controlPlaneLabel = "cluster.x-k8s.io/control-plane"
)

// updateState tracks in-progress UpdateMachine operations.
type updateState struct {
	startedAt time.Time
	applied   bool
}

// DoUpdateMachine implements the UpdateMachine hook.
//
// Uses PATCH mode to preserve per-node identity (hostname, VLAN, MAC, IPv6)
// that CAPHR injected at Day 0. Instead of sending the full desired config
// (which would overwrite per-node fields), we:
//
//  1. Retrieve the strategic merge patches computed by CanUpdateMachine
//     (diff between Current and Desired BootstrapConfig templates)
//  2. Read the node's live config (which has per-node identity fields)
//  3. Apply the template-level diff on top of the live config
//  4. Choose the apply mode:
//     - Control plane nodes (has control-plane label): mode=no-reboot
//     - Worker nodes: mode=auto
//  5. Send the merged result via Talos ApplyConfiguration API
//  6. Return Retry while waiting for the config to take effect
//  7. Return Success once the node has settled
//
// This hook is idempotent — CAPI may call it multiple times for the same Machine.
func (h *ExtensionHandlers) DoUpdateMachine(ctx context.Context, req *runtimehooksv1.UpdateMachineRequest, resp *runtimehooksv1.UpdateMachineResponse) {
	log := ctrl.LoggerFrom(ctx).WithValues("Machine", klog.KObj(&req.Desired.Machine))
	log.Info("UpdateMachine called")
	defer func() {
		log.Info("UpdateMachine response",
			"status", resp.Status,
			"message", resp.Message,
			"retryAfterSeconds", resp.RetryAfterSeconds,
		)
	}()

	key := klog.KObj(&req.Desired.Machine).String()
	machine := &req.Desired.Machine

	// Find the node's IP. Try Machine addresses first, then InfrastructureMachine.
	// Prefer InternalIP (VLAN), fallback to ExternalIP (public).
	nodeIP := findIP(machine.Status.Addresses)
	if nodeIP == "" {
		// Machine.Status.Addresses may be empty in UpdateMachineRequest.
		// Try extracting from InfrastructureMachine status.
		var infraStatus struct {
			Addresses []clusterv1.MachineAddress `json:"addresses"`
		}
		if req.Desired.InfrastructureMachine.Raw != nil {
			var infraObj map[string]json.RawMessage
			if err := json.Unmarshal(req.Desired.InfrastructureMachine.Raw, &infraObj); err == nil {
				if statusRaw, ok := infraObj["status"]; ok {
					_ = json.Unmarshal(statusRaw, &infraStatus)
					nodeIP = findIP(infraStatus.Addresses)
				}
			}
		}
	}
	if nodeIP == "" {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = "Machine has no IP address — cannot connect via Talos API"
		return
	}

	// Check if we have already started working on this Machine.
	stateVal, loaded := h.state.Load(key)
	if loaded {
		state := stateVal.(*updateState)

		// Check timeout.
		if time.Since(state.startedAt) > updateTimeoutDuration {
			h.state.Delete(key)
			h.configPatches.Delete(key)
			resp.Status = runtimehooksv1.ResponseStatusFailure
			resp.Message = fmt.Sprintf("update timed out after %s", updateTimeoutDuration)
			return
		}

		if state.applied {
			// We already applied the config. Give the node some time to
			// settle, then report success. In a production extension,
			// we would poll node readiness here.
			if time.Since(state.startedAt) > 30*time.Second {
				h.state.Delete(key)
				h.configPatches.Delete(key)
				resp.Status = runtimehooksv1.ResponseStatusSuccess
				resp.Message = "Config applied successfully; update complete"
				resp.RetryAfterSeconds = 0
				return
			}
			// Still settling.
			resp.Status = runtimehooksv1.ResponseStatusSuccess
			resp.Message = "Config applied; waiting for node to settle"
			resp.RetryAfterSeconds = retryIntervalSeconds
			return
		}
	}

	// --- PATCH MODE ---
	// Instead of extracting the full desired config and replacing the node's config
	// (which would overwrite per-node identity injected by CAPHR at Day 0),
	// we retrieve the strategic merge patches computed by CanUpdateMachine
	// (diff between Current and Desired BootstrapConfig templates) and apply
	// them on top of the node's live config.
	//
	// Flow:
	//   1. Retrieve stored patches from CanUpdateMachine (template-level diff)
	//   2. Read the node's live config (which has per-node identity fields)
	//   3. Apply the patches on top of the live config
	//   4. Send the merged result via ApplyConfiguration
	//
	// This preserves per-node identity (hostname, VLAN, MAC, IPv6) because
	// those fields are not in the template diff — they only exist on the live node.

	// Step 1: Retrieve the stored patches from CanUpdateMachine.
	// Use Load (not LoadAndDelete) because CAPI may retry UpdateMachine
	// multiple times if ApplyConfig fails transiently. We clean up after
	// successful apply.
	patchesVal, hasPatch := h.configPatches.Load(key)
	if !hasPatch {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = "no config patch found — CanUpdateMachine must run before UpdateMachine"
		return
	}
	patches := patchesVal.([]configpatcher.Patch)

	// Step 2: Read the node's live config.
	liveProvider, err := h.TalosClient.NodeConfigProvider(ctx, nodeIP)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = "failed to read live config from node: " + err.Error()
		return
	}

	// Step 3: Apply the template diff patches on top of the live config.
	patched, err := configpatcher.Apply(configpatcher.WithConfig(liveProvider), patches)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = "failed to apply config patch: " + err.Error()
		return
	}

	patchedBytes, err := patched.Bytes()
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = "failed to encode patched config: " + err.Error()
		return
	}

	log.Info("Config patch applied",
		"patchCount", len(patches),
		"liveConfigLen", len(mustEncodeProvider(liveProvider)),
		"patchedConfigLen", len(patchedBytes),
	)

	// Determine apply mode based on node role.
	mode := talos.ApplyModeAuto
	if isControlPlane(machine.Labels) {
		mode = talos.ApplyModeNoReboot
		log.Info("Control plane node detected — using no-reboot mode", "nodeIP", nodeIP)
	} else {
		log.Info("Worker node detected — using auto mode", "nodeIP", nodeIP)
	}

	// Apply the patched config via Talos API.
	log.Info("Applying patched config via Talos API", "nodeIP", nodeIP, "mode", mode, "configLen", len(patchedBytes))
	if err := h.TalosClient.ApplyConfig(ctx, nodeIP, patchedBytes, mode); err != nil {
		// If this is the first attempt, record the state and retry.
		// Transient errors (node rebooting, etc.) should resolve on retry.
		if !loaded {
			h.state.Store(key, &updateState{startedAt: time.Now(), applied: false})
		}
		resp.Status = runtimehooksv1.ResponseStatusSuccess
		resp.Message = "Talos API apply-config failed (will retry): " + err.Error()
		resp.RetryAfterSeconds = retryIntervalSeconds
		return
	}

	// Config applied successfully. Clean up stored patches and record state.
	h.configPatches.Delete(key)
	h.state.Store(key, &updateState{startedAt: time.Now(), applied: true})
	resp.Status = runtimehooksv1.ResponseStatusSuccess
	resp.Message = "Config applied via Talos API; waiting for node to settle"
	resp.RetryAfterSeconds = retryIntervalSeconds
}

// isControlPlane returns true if the Machine labels indicate a control plane node.
func isControlPlane(labels map[string]string) bool {
	if labels == nil {
		return false
	}
	_, ok := labels[controlPlaneLabel]
	return ok
}

// extractBootstrapData extracts the machine config YAML from the raw
// BootstrapConfig JSON. The bootstrap data is typically stored in
// spec.data or spec.machineConfig depending on the bootstrap provider.
// For Talos bootstrap providers, the config is the full machine config YAML.
func extractBootstrapData(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("bootstrap config is empty")
	}

	// Parse the raw bootstrap config to find the machine config data.
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("failed to unmarshal bootstrap config: %w", err)
	}

	// Try spec.data first (common for bootstrap providers that embed config).
	if spec, ok := obj["spec"].(map[string]interface{}); ok {
		// Try spec.talosConfig (Talos bootstrap provider pattern).
		if talosConfig, ok := spec["talosConfig"].(string); ok && talosConfig != "" {
			return []byte(talosConfig), nil
		}

		// Try spec.data (generic pattern).
		if data, ok := spec["data"].(string); ok && data != "" {
			return []byte(data), nil
		}

		// Try spec.machineConfig (alternative pattern).
		if mc, ok := spec["machineConfig"].(string); ok && mc != "" {
			return []byte(mc), nil
		}

		// If the spec itself contains Talos machine config fields (machine, cluster),
		// marshal the entire spec as YAML — this handles inline configs.
		if _, hasMachine := spec["machine"]; hasMachine {
			specBytes, err := json.Marshal(spec)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal spec as machine config: %w", err)
			}
			return specBytes, nil
		}
	}

	// No recognized bootstrap data format found.
	return nil, fmt.Errorf("could not extract Talos machine config from bootstrap data: no recognized field (tried data, value, spec.data, spec.talosConfig)")
}

// mustEncodeProvider encodes a config.Provider to YAML bytes for logging.
// Returns nil on error (used only for log messages, not critical path).
func mustEncodeProvider(p interface{ EncodeBytes(...encoder.Option) ([]byte, error) }) []byte {
	b, _ := p.EncodeBytes(encoder.WithComments(encoder.CommentsDisabled))
	return b
}

// findIP extracts the best IP from a list of MachineAddresses.
// Prefers InternalIP, falls back to ExternalIP.
func findIP(addrs []clusterv1.MachineAddress) string {
	for _, a := range addrs {
		if a.Type == clusterv1.MachineInternalIP {
			return a.Address
		}
	}
	for _, a := range addrs {
		if a.Type == clusterv1.MachineExternalIP {
			return a.Address
		}
	}
	return ""
}
