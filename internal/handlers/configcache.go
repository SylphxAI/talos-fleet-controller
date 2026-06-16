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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/siderolabs/talos/pkg/machinery/config/configdiff"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
)

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
