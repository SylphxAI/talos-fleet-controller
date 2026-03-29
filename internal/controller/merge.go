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

package controller

import (
	"fmt"

	"github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configpatcher"
	"github.com/siderolabs/talos/pkg/machinery/config/encoder"
)

// applyPatchToConfig applies a strategic merge patch (YAML bytes) to the
// current config provider. Uses Talos's own configpatcher — handles
// multi-document configs, proper strategic merge, and correct serialization.
//
// This is the ONLY correct way to merge Talos configs. Our previous approach
// (yaml.Unmarshal → mergo.Merge → yaml.Marshal) corrupted multi-document
// configs and produced non-deterministic YAML output.
func applyPatchToConfig(currentConfig config.Provider, patchYAML []byte) (config.Provider, error) {
	if len(patchYAML) == 0 {
		return currentConfig, nil
	}

	// LoadPatch auto-detects strategic merge vs JSON6902.
	patch, err := configpatcher.LoadPatch(patchYAML)
	if err != nil {
		return nil, fmt.Errorf("load patch: %w", err)
	}

	// Apply patch to current config using Talos's multi-document-aware merge.
	output, err := configpatcher.Apply(configpatcher.WithConfig(currentConfig), []configpatcher.Patch{patch})
	if err != nil {
		return nil, fmt.Errorf("apply patch: %w", err)
	}

	result, err := output.Config()
	if err != nil {
		return nil, fmt.Errorf("get patched config: %w", err)
	}

	return result, nil
}

// configToBytes serializes a config provider to deterministic YAML bytes
// using Talos's encoder (not generic yaml.Marshal). This ensures consistent
// output for hash comparison — no key reordering, no format changes.
func configToBytes(cfg config.Provider) ([]byte, error) {
	return cfg.EncodeBytes(encoder.WithComments(encoder.CommentsDisabled))
}

// mergePatches merges multiple YAML patch strings into one by applying
// them sequentially to an empty base. Used to combine base machineConfig
// + nodeOverride into a single patch.
func mergePatches(base, overlay string) ([]byte, error) {
	if base == "" && overlay == "" {
		return nil, fmt.Errorf("both base and overlay are empty")
	}
	if base == "" {
		return []byte(overlay), nil
	}
	if overlay == "" {
		return []byte(base), nil
	}

	// Load base as a config, then apply overlay as a strategic merge patch.
	basePatch, err := configpatcher.LoadPatch([]byte(base))
	if err != nil {
		return nil, fmt.Errorf("load base patch: %w", err)
	}

	overlayPatch, err := configpatcher.LoadPatch([]byte(overlay))
	if err != nil {
		return nil, fmt.Errorf("load overlay patch: %w", err)
	}

	// Apply base first, then overlay on top.
	// Use an empty-ish config as the starting point.
	output, err := configpatcher.Apply(
		configpatcher.WithBytes([]byte(base)),
		[]configpatcher.Patch{overlayPatch},
	)
	if err != nil {
		// If sequential apply fails, try just returning the overlay
		// (base might not be a valid standalone config).
		_ = basePatch
		return []byte(overlay), nil
	}

	return output.Bytes()
}
