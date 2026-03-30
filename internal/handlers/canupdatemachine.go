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

	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"

	"github.com/siderolabs/talos/pkg/machinery/config/configdiff"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
)

// DoCanUpdateMachine implements the CanUpdateMachine hook.
//
// For Talos Linux, we can handle nearly all machine config changes in-place via
// `talosctl apply-config`. The only things we cannot update without
// recreating the machine are:
//   - Disk layout / partitioning changes
//   - System extension changes (these require an upgrade/reinstall)
//
// For simplicity, this extension claims it can handle ALL spec changes on the
// Machine and BootstrapConfig objects. The actual UpdateMachine handler will
// determine the correct apply mode (no-reboot vs auto) and report failure if
// a change truly requires recreation.
func (h *ExtensionHandlers) DoCanUpdateMachine(ctx context.Context, req *runtimehooksv1.CanUpdateMachineRequest, resp *runtimehooksv1.CanUpdateMachineResponse) {
	log := ctrl.LoggerFrom(ctx).WithValues("Machine", klog.KObj(&req.Desired.Machine))
	log.Info("CanUpdateMachine called")

	currentMachine := req.Current.Machine.DeepCopy()
	desiredMachine := req.Desired.Machine.DeepCopy()

	// Claim we can handle all Machine spec changes in-place.
	canUpdateTalosMachineSpec(&currentMachine.Spec, &desiredMachine.Spec)

	// Compute JSON patches showing what we claimed.
	marshalledCurrentMachine, err := json.Marshal(req.Current.Machine)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = "failed to marshal current Machine: " + err.Error()
		return
	}
	machinePatch, err := createJSONPatch(marshalledCurrentMachine, currentMachine)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = "failed to create Machine patch: " + err.Error()
		return
	}

	// For BootstrapConfig (Talos machine config): claim all changes.
	bootstrapConfigPatch, err := createBootstrapConfigPatch(
		req.Current.BootstrapConfig.Raw,
		req.Desired.BootstrapConfig.Raw,
	)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = "failed to create BootstrapConfig patch: " + err.Error()
		return
	}

	// For InfrastructureMachine: claim all changes.
	infraPatch, err := createInfraMachinePatch(
		req.Current.InfrastructureMachine.Raw,
		req.Desired.InfrastructureMachine.Raw,
	)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = "failed to create InfrastructureMachine patch: " + err.Error()
		return
	}

	resp.MachinePatch = runtimehooksv1.Patch{
		PatchType: runtimehooksv1.JSONPatchType,
		Patch:     machinePatch,
	}
	resp.BootstrapConfigPatch = runtimehooksv1.Patch{
		PatchType: runtimehooksv1.JSONPatchType,
		Patch:     bootstrapConfigPatch,
	}
	resp.InfrastructureMachinePatch = runtimehooksv1.Patch{
		PatchType: runtimehooksv1.JSONPatchType,
		Patch:     infraPatch,
	}
	resp.Status = runtimehooksv1.ResponseStatusSuccess

	// Compute and store strategic merge patches for the Talos machine config.
	// UpdateMachine (which only receives Desired, not Current) retrieves these
	// patches and applies them on top of the node's live config. This preserves
	// per-node identity (hostname, VLAN, MAC, IPv6) that CAPHR injected at Day 0.
	machineKey := klog.KObj(&req.Desired.Machine).String()
	if err := h.storeBootstrapConfigDiff(machineKey, req.Current.BootstrapConfig.Raw, req.Desired.BootstrapConfig.Raw); err != nil {
		// Log but don't fail the CanUpdateMachine response — we still claim
		// we can handle the changes. UpdateMachine will fail if the stored
		// patches are missing, which is the safe default (avoids overwriting
		// per-node identity with a full config replace).
		log.Error(err, "Failed to compute/store bootstrap config diff for patch mode")
	}

	log.Info("CanUpdateMachine completed",
		"machinePatchLen", len(machinePatch),
		"bootstrapPatchLen", len(bootstrapConfigPatch),
		"infraPatchLen", len(infraPatch),
	)
}

// storeBootstrapConfigDiff computes the strategic merge patch between the current
// and desired BootstrapConfig and stores it for later use by UpdateMachine.
func (h *ExtensionHandlers) storeBootstrapConfigDiff(machineKey string, currentRaw, desiredRaw []byte) error {
	currentYAML, err := extractBootstrapData(currentRaw)
	if err != nil {
		return err
	}

	desiredYAML, err := extractBootstrapData(desiredRaw)
	if err != nil {
		return err
	}

	currentProvider, err := configloader.NewFromBytes(currentYAML)
	if err != nil {
		return err
	}

	desiredProvider, err := configloader.NewFromBytes(desiredYAML)
	if err != nil {
		return err
	}

	patches, err := configdiff.Patch(currentProvider, desiredProvider)
	if err != nil {
		return err
	}

	h.configPatches.Store(machineKey, patches)

	return nil
}

// DoCanUpdateMachineSet implements the CanUpdateMachineSet hook.
// Same logic as CanUpdateMachine but operates on MachineSet templates.
func (h *ExtensionHandlers) DoCanUpdateMachineSet(ctx context.Context, req *runtimehooksv1.CanUpdateMachineSetRequest, resp *runtimehooksv1.CanUpdateMachineSetResponse) {
	log := ctrl.LoggerFrom(ctx).WithValues("MachineSet", klog.KObj(&req.Desired.MachineSet))
	log.Info("CanUpdateMachineSet called")

	currentMachineSet := req.Current.MachineSet.DeepCopy()
	desiredMachineSet := req.Desired.MachineSet.DeepCopy()

	// Claim we can handle all MachineSet template spec changes in-place.
	canUpdateTalosMachineSpec(&currentMachineSet.Spec.Template.Spec, &desiredMachineSet.Spec.Template.Spec)

	marshalledCurrentMachineSet, err := json.Marshal(req.Current.MachineSet)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = "failed to marshal current MachineSet: " + err.Error()
		return
	}
	machineSetPatch, err := createJSONPatch(marshalledCurrentMachineSet, currentMachineSet)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = "failed to create MachineSet patch: " + err.Error()
		return
	}

	bootstrapTemplatePatch, err := createBootstrapConfigPatch(
		req.Current.BootstrapConfigTemplate.Raw,
		req.Desired.BootstrapConfigTemplate.Raw,
	)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = "failed to create BootstrapConfigTemplate patch: " + err.Error()
		return
	}

	infraTemplatePatch, err := createInfraMachinePatch(
		req.Current.InfrastructureMachineTemplate.Raw,
		req.Desired.InfrastructureMachineTemplate.Raw,
	)
	if err != nil {
		resp.Status = runtimehooksv1.ResponseStatusFailure
		resp.Message = "failed to create InfrastructureMachineTemplate patch: " + err.Error()
		return
	}

	resp.MachineSetPatch = runtimehooksv1.Patch{
		PatchType: runtimehooksv1.JSONPatchType,
		Patch:     machineSetPatch,
	}
	resp.BootstrapConfigTemplatePatch = runtimehooksv1.Patch{
		PatchType: runtimehooksv1.JSONPatchType,
		Patch:     bootstrapTemplatePatch,
	}
	resp.InfrastructureMachineTemplatePatch = runtimehooksv1.Patch{
		PatchType: runtimehooksv1.JSONPatchType,
		Patch:     infraTemplatePatch,
	}
	resp.Status = runtimehooksv1.ResponseStatusSuccess

	log.Info("CanUpdateMachineSet completed",
		"machineSetPatchLen", len(machineSetPatch),
		"bootstrapPatchLen", len(bootstrapTemplatePatch),
		"infraPatchLen", len(infraTemplatePatch),
	)
}

// canUpdateTalosMachineSpec declares which Machine spec fields this extension
// can handle in-place. For Talos, we can handle version and failure domain
// changes. We mutate `current` to match `desired` for claimed fields so that
// the JSON patch correctly reflects what we handle.
func canUpdateTalosMachineSpec(current, desired *clusterv1.MachineSpec) {
	// Version changes: Talos can apply Kubernetes version changes via config.
	if current.Version != desired.Version {
		current.Version = desired.Version
	}
	// Failure domain changes.
	if current.FailureDomain != desired.FailureDomain {
		current.FailureDomain = desired.FailureDomain
	}
}

// createBootstrapConfigPatch takes the raw current and desired BootstrapConfig
// JSON and returns a JSON patch that transforms current into desired.
// For Talos, the BootstrapConfig is the full machine config — we claim all changes.
func createBootstrapConfigPatch(currentRaw, desiredRaw []byte) ([]byte, error) {
	if len(currentRaw) == 0 || len(desiredRaw) == 0 {
		return []byte("[]"), nil
	}

	// Unmarshal both into generic maps so we can produce a proper diff.
	var currentObj, desiredObj map[string]interface{}
	if err := json.Unmarshal(currentRaw, &currentObj); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(desiredRaw, &desiredObj); err != nil {
		return nil, err
	}

	// Copy all desired spec fields into current to claim them.
	// For Talos, we can handle all spec changes in-place via talosctl apply-config.
	if desiredSpec, ok := desiredObj["spec"]; ok {
		currentObj["spec"] = desiredSpec
	}

	// Re-marshal current with claimed changes and diff against original.
	modifiedBytes, err := json.Marshal(currentObj)
	if err != nil {
		return nil, err
	}
	return createJSONPatchFromBytes(currentRaw, modifiedBytes)
}

// createInfraMachinePatch takes the raw current and desired InfrastructureMachine
// JSON and returns a JSON patch. For Talos infra (e.g. QEMU, Metal), we claim
// all spec changes since Talos handles infra config via the machine config.
func createInfraMachinePatch(currentRaw, desiredRaw []byte) ([]byte, error) {
	if len(currentRaw) == 0 || len(desiredRaw) == 0 {
		return []byte("[]"), nil
	}

	var currentObj, desiredObj map[string]interface{}
	if err := json.Unmarshal(currentRaw, &currentObj); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(desiredRaw, &desiredObj); err != nil {
		return nil, err
	}

	// Claim all spec changes.
	if desiredSpec, ok := desiredObj["spec"]; ok {
		currentObj["spec"] = desiredSpec
	}

	modifiedBytes, err := json.Marshal(currentObj)
	if err != nil {
		return nil, err
	}
	return createJSONPatchFromBytes(currentRaw, modifiedBytes)
}
