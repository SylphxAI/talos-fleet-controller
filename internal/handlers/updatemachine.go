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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"

	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"

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
	// Last resort: query actual Machine + Node from K8s API.
	// UpdateMachineRequest.Desired may not include runtime status (addresses, nodeRef).
	if nodeIP == "" && h.K8sClient != nil {
		log.Info("IP lookup: querying actual Machine from K8s API",
			"namespace", machine.Namespace, "name", machine.Name)
		var actualMachine clusterv1.Machine
		if err := h.K8sClient.Get(ctx, client.ObjectKey{
			Namespace: machine.Namespace,
			Name:      machine.Name,
		}, &actualMachine); err != nil {
			log.Error(err, "IP lookup: failed to get Machine from K8s API",
				"namespace", machine.Namespace, "name", machine.Name)
		} else {
			log.Info("IP lookup: got Machine from K8s API",
				"addressCount", len(actualMachine.Status.Addresses),
				"nodeRef", actualMachine.Status.NodeRef.Name)
			nodeIP = findIP(actualMachine.Status.Addresses)
			if nodeIP == "" && actualMachine.Status.NodeRef.Name != "" {
				log.Info("IP lookup: Machine has no usable address, trying Node",
					"nodeRef", actualMachine.Status.NodeRef.Name)
				var node corev1.Node
				if err := h.K8sClient.Get(ctx, client.ObjectKey{Name: actualMachine.Status.NodeRef.Name}, &node); err != nil {
					log.Error(err, "IP lookup: failed to get Node from K8s API",
						"nodeName", actualMachine.Status.NodeRef.Name)
				} else {
					log.Info("IP lookup: got Node from K8s API",
						"addressCount", len(node.Status.Addresses))
					for _, addr := range node.Status.Addresses {
						if addr.Type == corev1.NodeInternalIP {
							nodeIP = addr.Address
							break
						}
					}
					if nodeIP == "" {
						for _, addr := range node.Status.Addresses {
							if addr.Type == corev1.NodeExternalIP {
								nodeIP = addr.Address
								break
							}
						}
					}
				}
			}
		}
	} else if nodeIP == "" {
		log.Info("IP lookup: K8sReader is nil, cannot query K8s API")
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
	//
	// If patches are not in memory (pod restarted), re-derive them from the
	// ConfigMap cache that CanUpdateMachine persisted. This makes UpdateMachine
	// resilient to pod restarts.
	// Step 1: Read the node's live config (needed for all code paths).
	liveProvider, err := h.TalosClient.NodeConfigProvider(ctx, nodeIP)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = "failed to read live config from node: " + err.Error()
		return
	}
	liveBytes := mustEncodeProvider(liveProvider)

	// Step 2: Compute the merged config to apply.
	// Three paths, in order of preference:
	//   A. In-memory patches from CanUpdateMachine (fast path)
	//   B. Re-derived patches from ConfigMap cache (pod restart recovery)
	//   C. Direct merge: desired template + per-node identity from live (self-sufficient)
	//
	// Path C is the fallback when CanUpdateMachine was never called
	// (e.g., CACPPT RuntimeClient is nil — the common case for Talos).
	var patchedBytes []byte

	patchesVal, hasPatch := h.configPatches.Load(key)
	if !hasPatch {
		log.Info("No in-memory patches — trying ConfigMap recovery")
		var recoverErr error
		patchesVal, recoverErr = h.recoverPatchesFromConfigMap(ctx, key, req.Desired.BootstrapConfig.Raw)
		if recoverErr != nil {
			log.Info("ConfigMap recovery failed — using direct merge mode", "error", recoverErr.Error())
		} else {
			hasPatch = true
			log.Info("Recovered patches from ConfigMap cache")
		}
	}

	if hasPatch {
		// Path A/B: Apply cached/recovered patches on live config.
		patches := patchesVal.([]configpatcher.Patch)
		patched, err := configpatcher.Apply(configpatcher.WithConfig(liveProvider), patches)
		if err != nil {
			resp.Status = runtimehooksv1.ResponseStatusFailure
			resp.Message = "failed to apply config patch: " + err.Error()
			return
		}
		patchedBytes, err = patched.Bytes()
		if err != nil {
			resp.Status = runtimehooksv1.ResponseStatusFailure
			resp.Message = "failed to encode patched config: " + err.Error()
			return
		}
		log.Info("Config merged via patch mode",
			"patchCount", len(patches),
			"liveConfigLen", len(liveBytes),
			"patchedConfigLen", len(patchedBytes),
		)
	} else {
		// Path C: Apply strategic patches from TalosConfig directly on the live config.
		// This is the primary path when CACPPT RuntimeClient is nil (Talos default).
		//
		// The BootstrapConfig is a CABPT TalosConfig with spec.strategicPatches —
		// these are template-level patches (NOT a full config). Applying them on
		// the live config preserves per-node identity naturally because the patches
		// only touch template-level fields.
		strategicPatches, err := extractStrategicPatches(req.Desired.BootstrapConfig.Raw)
		if err != nil {
			resp.Status = runtimehooksv1.ResponseStatusFailure
			resp.Message = "failed to extract strategic patches from BootstrapConfig: " + err.Error()
			return
		}

		if len(strategicPatches) == 0 {
			// No strategic patches — nothing to change.
			log.Info("No strategic patches in desired BootstrapConfig — nothing to apply")
			h.state.Store(key, &updateState{startedAt: time.Now(), applied: true})
			resp.Status = runtimehooksv1.ResponseStatusSuccess
			resp.Message = "No config changes to apply"
			resp.RetryAfterSeconds = retryIntervalSeconds
			return
		}

		// Load each strategic patch and apply on top of the live config.
		var allPatches []configpatcher.Patch
		for i, patchStr := range strategicPatches {
			patch, err := configpatcher.LoadPatch([]byte(patchStr))
			if err != nil {
				resp.Status = runtimehooksv1.ResponseStatusFailure
				resp.Message = fmt.Sprintf("failed to load strategic patch %d: %s", i, err.Error())
				return
			}
			allPatches = append(allPatches, patch)
		}

		patched, err := configpatcher.Apply(configpatcher.WithConfig(liveProvider), allPatches)
		if err != nil {
			resp.Status = runtimehooksv1.ResponseStatusFailure
			resp.Message = "failed to apply strategic patches on live config: " + err.Error()
			return
		}
		patchedBytes, err = patched.Bytes()
		if err != nil {
			resp.Status = runtimehooksv1.ResponseStatusFailure
			resp.Message = "failed to encode patched config: " + err.Error()
			return
		}
		log.Info("Config merged via strategic patch mode (CABPT patches on live config)",
			"patchCount", len(allPatches),
			"liveConfigLen", len(liveBytes),
			"patchedConfigLen", len(patchedBytes),
		)
	}

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
	h.cleanupConfigCache(ctx, key)
	h.state.Store(key, &updateState{startedAt: time.Now(), applied: true})
	resp.Status = runtimehooksv1.ResponseStatusSuccess
	resp.Message = "Config applied via Talos API; waiting for node to settle"
	resp.RetryAfterSeconds = retryIntervalSeconds
}
