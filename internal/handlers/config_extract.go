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
	"encoding/json"
	"fmt"

	yaml "sigs.k8s.io/yaml"

	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
)

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
func mustEncodeProvider(p interface {
	EncodeBytes(...encoder.Option) ([]byte, error)
}) []byte {
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
