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

	"dario.cat/mergo"
	"gopkg.in/yaml.v3"
)

// mergeYAML performs a deep merge of overlay on top of base.
// Both inputs are YAML strings. The result is a single merged YAML document.
// Arrays are replaced (not appended) — overlay arrays fully replace base arrays.
// Maps are recursively merged — overlay keys override base keys.
func mergeYAML(base, overlay string) ([]byte, error) {
	if base == "" && overlay == "" {
		return nil, fmt.Errorf("both base and overlay are empty")
	}
	if base == "" {
		return []byte(overlay), nil
	}
	if overlay == "" {
		return []byte(base), nil
	}

	var baseMap map[string]any
	if err := yaml.Unmarshal([]byte(base), &baseMap); err != nil {
		return nil, fmt.Errorf("parse base YAML: %w", err)
	}

	var overlayMap map[string]any
	if err := yaml.Unmarshal([]byte(overlay), &overlayMap); err != nil {
		return nil, fmt.Errorf("parse overlay YAML: %w", err)
	}

	// Deep merge: overlay values override base values.
	// mergo.WithOverride ensures overlay wins on conflicts.
	if err := mergo.Merge(&baseMap, overlayMap, mergo.WithOverride); err != nil {
		return nil, fmt.Errorf("merge YAML: %w", err)
	}

	result, err := yaml.Marshal(baseMap)
	if err != nil {
		return nil, fmt.Errorf("marshal merged YAML: %w", err)
	}

	return result, nil
}

// mergeConfigWithCurrent reads the current full config from a node,
// merges the desired patch on top, and returns the complete config
// ready for ApplyConfiguration. This preserves cluster secrets
// (CA, etcd, SA key) while applying only the desired changes.
func (r *FleetNodeSetReconciler) mergeConfigWithCurrent(
	currentConfigYAML []byte,
	desiredPatch []byte,
) ([]byte, error) {
	var currentMap map[string]any
	if err := yaml.Unmarshal(currentConfigYAML, &currentMap); err != nil {
		return nil, fmt.Errorf("parse current config: %w", err)
	}

	var patchMap map[string]any
	if err := yaml.Unmarshal(desiredPatch, &patchMap); err != nil {
		return nil, fmt.Errorf("parse desired patch: %w", err)
	}

	// Deep merge patch on top of current — secrets preserved, desired fields applied.
	if err := mergo.Merge(&currentMap, patchMap, mergo.WithOverride); err != nil {
		return nil, fmt.Errorf("merge config: %w", err)
	}

	result, err := yaml.Marshal(currentMap)
	if err != nil {
		return nil, fmt.Errorf("marshal merged config: %w", err)
	}

	return result, nil
}
