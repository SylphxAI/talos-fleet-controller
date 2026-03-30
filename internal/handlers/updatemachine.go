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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	yaml "sigs.k8s.io/yaml"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"

	"github.com/siderolabs/talos/pkg/machinery/config/configdiff"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
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

// extractStrategicPatches extracts the strategic merge patches from a CABPT
// TalosConfig BootstrapConfig JSON. The TalosConfig has spec.strategicPatches
// which is an array of YAML strings (Talos strategic merge patches).
func extractStrategicPatches(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("unmarshal BootstrapConfig: %w", err)
	}
	spec, ok := obj["spec"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("no spec in BootstrapConfig")
	}
	patchesRaw, ok := spec["strategicPatches"]
	if !ok {
		return nil, nil // No strategic patches.
	}
	patchesArr, ok := patchesRaw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("strategicPatches is not an array")
	}
	var patches []string
	for _, p := range patchesArr {
		s, ok := p.(string)
		if !ok {
			continue
		}
		patches = append(patches, s)
	}
	return patches, nil
}

// mergeDesiredWithPerNodeIdentity takes the desired template config YAML and the
// live config YAML from the node, and produces a merged config that has the new
// template values PLUS per-node identity preserved from the live config.
//
// Per-node identity fields (injected by CAPHR at Day 0):
//   - machine.network.hostname
//   - machine.network.interfaces (MAC, VLAN, addresses — entire array)
//   - machine.kubelet.extraArgs (node-ip, provider-id)
//   - machine.kubelet.nodeIP (validSubnets for dual-stack)
//
// Strategy: Parse both configs as YAML maps. Start with the desired config,
// then overlay per-node fields from the live config. This preserves template
// changes while keeping per-node identity intact.
func mergeDesiredWithPerNodeIdentity(desiredYAML, liveYAML []byte) ([]byte, error) {
	var desired, live map[string]interface{}
	if err := yaml.Unmarshal(desiredYAML, &desired); err != nil {
		return nil, fmt.Errorf("parse desired config: %w", err)
	}
	if err := yaml.Unmarshal(liveYAML, &live); err != nil {
		return nil, fmt.Errorf("parse live config: %w", err)
	}

	liveMachine := getMap(live, "machine")
	desiredMachine := getMap(desired, "machine")
	if liveMachine == nil || desiredMachine == nil {
		// Can't merge — return desired as-is.
		return desiredYAML, nil
	}

	liveNetwork := getMap(liveMachine, "network")
	desiredNetwork := getMap(desiredMachine, "network")

	// Preserve hostname from live config.
	if liveNetwork != nil && desiredNetwork != nil {
		if hostname, ok := liveNetwork["hostname"]; ok {
			desiredNetwork["hostname"] = hostname
		}
	}

	// Preserve entire interfaces array from live config.
	// Interfaces contain per-node MAC addresses, VLAN IPs, device selectors —
	// all injected by CAPHR at Day 0 and unique per node.
	if liveNetwork != nil && desiredNetwork != nil {
		if ifaces, ok := liveNetwork["interfaces"]; ok {
			desiredNetwork["interfaces"] = ifaces
		}
	}

	// Preserve kubelet per-node settings.
	liveKubelet := getMap(liveMachine, "kubelet")
	desiredKubelet := getMap(desiredMachine, "kubelet")
	if desiredKubelet == nil && liveKubelet != nil {
		desiredKubelet = map[string]interface{}{}
		desiredMachine["kubelet"] = desiredKubelet
	}
	if liveKubelet != nil && desiredKubelet != nil {
		// Merge extraArgs from live into desired (live takes precedence for per-node keys).
		if liveArgs, ok := liveKubelet["extraArgs"].(map[string]interface{}); ok {
			desiredArgs, _ := desiredKubelet["extraArgs"].(map[string]interface{})
			if desiredArgs == nil {
				desiredArgs = map[string]interface{}{}
			}
			for k, v := range liveArgs {
				desiredArgs[k] = v
			}
			desiredKubelet["extraArgs"] = desiredArgs
		}
		// Preserve nodeIP (validSubnets for dual-stack).
		if nodeIP, ok := liveKubelet["nodeIP"]; ok {
			desiredKubelet["nodeIP"] = nodeIP
		}
	}

	return yaml.Marshal(desired)
}

// getMap safely extracts a nested map from a parent map.
func getMap(parent map[string]interface{}, key string) map[string]interface{} {
	if parent == nil {
		return nil
	}
	v, ok := parent[key]
	if !ok {
		return nil
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}
	return m
}

// cleanupConfigCache removes the machine's entry from the config cache ConfigMap.
func (h *ExtensionHandlers) cleanupConfigCache(ctx context.Context, machineKey string) {
	if h.K8sClient == nil {
		return
	}
	log := ctrl.LoggerFrom(ctx)
	var cm corev1.ConfigMap
	if err := h.K8sClient.Get(ctx, client.ObjectKey{
		Namespace: extensionNamespace,
		Name:      configCacheConfigMapName,
	}, &cm); err != nil {
		return // ConfigMap doesn't exist or error — nothing to clean up.
	}
	dataKey := sanitizeConfigMapKey(machineKey)
	if _, ok := cm.Data[dataKey]; !ok {
		return // Entry doesn't exist.
	}
	delete(cm.Data, dataKey)
	if err := h.K8sClient.Update(ctx, &cm); err != nil {
		log.Error(err, "Failed to clean up config cache ConfigMap entry", "machineKey", machineKey)
	}
}

// recoverPatchesFromConfigMap re-derives config patches after a pod restart.
// CanUpdateMachine persists the current BootstrapConfig raw bytes in a ConfigMap.
// This method reads those bytes, combines them with the desired BootstrapConfig
// from the UpdateMachineRequest, and re-computes the strategic merge patches.
func (h *ExtensionHandlers) recoverPatchesFromConfigMap(ctx context.Context, machineKey string, desiredBootstrapRaw []byte) (interface{}, error) {
	if h.K8sClient == nil {
		return nil, fmt.Errorf("K8s client is nil")
	}

	// Read the ConfigMap with cached current BootstrapConfig bytes.
	var cm corev1.ConfigMap
	if err := h.K8sClient.Get(ctx, client.ObjectKey{
		Namespace: extensionNamespace,
		Name:      configCacheConfigMapName,
	}, &cm); err != nil {
		return nil, fmt.Errorf("failed to get config cache ConfigMap: %w", err)
	}

	// ConfigMap data key uses the machine key with slashes replaced by dots.
	dataKey := sanitizeConfigMapKey(machineKey)
	currentRawStr, ok := cm.Data[dataKey]
	if !ok {
		return nil, fmt.Errorf("no cached current config for machine %s in ConfigMap", machineKey)
	}

	currentRaw := []byte(currentRawStr)

	// Re-derive patches using the same logic as CanUpdateMachine.
	currentYAML, err := extractBootstrapData(currentRaw)
	if err != nil {
		return nil, fmt.Errorf("failed to extract current bootstrap data: %w", err)
	}
	desiredYAML, err := extractBootstrapData(desiredBootstrapRaw)
	if err != nil {
		return nil, fmt.Errorf("failed to extract desired bootstrap data: %w", err)
	}

	currentProvider, err := configloader.NewFromBytes(currentYAML)
	if err != nil {
		return nil, fmt.Errorf("failed to parse current config: %w", err)
	}
	desiredProvider, err := configloader.NewFromBytes(desiredYAML)
	if err != nil {
		return nil, fmt.Errorf("failed to parse desired config: %w", err)
	}

	patches, err := configdiff.Patch(currentProvider, desiredProvider)
	if err != nil {
		return nil, fmt.Errorf("failed to compute config diff: %w", err)
	}

	// Re-populate in-memory cache for subsequent retries.
	h.configPatches.Store(machineKey, patches)

	return patches, nil
}

// sanitizeConfigMapKey converts a Machine key (e.g. "caphr/machine-name")
// into a valid ConfigMap data key by replacing slashes with dots.
func sanitizeConfigMapKey(key string) string {
	return strings.ReplaceAll(key, "/", ".")
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
