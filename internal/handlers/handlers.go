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

// Package handlers implements CAPI RuntimeSDK in-place update extension hooks
// for Talos Linux. The extension handles CanUpdateMachine, CanUpdateMachineSet,
// and UpdateMachine hooks, applying config changes via the Talos API instead of
// deleting and recreating Machines.
package handlers

import (
	"encoding/json"
	"sync"

	"gomodules.xyz/jsonpatch/v2"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/SylphxAI/talos-fleet-controller/internal/talos"
)

// ExtensionHandlers provides shared state for all in-place update hook handlers.
type ExtensionHandlers struct {
	// TalosClient is the Talos gRPC client used by UpdateMachine to apply configs.
	TalosClient talos.Interface

	// state tracks in-flight UpdateMachine operations keyed by Machine namespace/name.
	state sync.Map
}

// NewExtensionHandlers creates a new ExtensionHandlers instance.
func NewExtensionHandlers(talosClient talos.Interface) *ExtensionHandlers {
	return &ExtensionHandlers{
		TalosClient: talosClient,
	}
}

// createJSONPatch produces an RFC 6902 JSON patch from the original marshalled
// bytes and a modified runtime.Object. The patch describes which fields the
// extension has "claimed" it can handle in-place.
func createJSONPatch(marshalledOriginal []byte, modified runtime.Object) ([]byte, error) {
	marshalledModified, err := json.Marshal(modified)
	if err != nil {
		return nil, err
	}

	patch, err := jsonpatch.CreatePatch(marshalledOriginal, marshalledModified)
	if err != nil {
		return nil, err
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return nil, err
	}

	return patchBytes, nil
}
