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
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMergeYAML_BothEmpty(t *testing.T) {
	_, err := mergeYAML("", "")
	if err == nil {
		t.Fatal("expected error when both base and overlay are empty")
	}
	if !strings.Contains(err.Error(), "both base and overlay are empty") {
		t.Fatalf("unexpected error message: %s", err)
	}
}

func TestMergeYAML_EmptyBase_NonEmptyOverlay(t *testing.T) {
	overlay := "machine:\n  network:\n    hostname: worker-1\n"
	result, err := mergeYAML("", overlay)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result) != overlay {
		t.Fatalf("expected overlay returned verbatim\ngot:  %q\nwant: %q", string(result), overlay)
	}
}

func TestMergeYAML_NonEmptyBase_EmptyOverlay(t *testing.T) {
	base := "machine:\n  network:\n    hostname: worker-1\n"
	result, err := mergeYAML(base, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result) != base {
		t.Fatalf("expected base returned verbatim\ngot:  %q\nwant: %q", string(result), base)
	}
}

func TestMergeYAML_DeepMergeMaps(t *testing.T) {
	base := `
machine:
  network:
    hostname: worker-1
    interfaces:
      - interface: eth0
        dhcp: true
  install:
    disk: /dev/sda
`
	overlay := `
machine:
  network:
    nameservers:
      - 8.8.8.8
  certSANs:
    - 10.0.0.1
`
	result, err := mergeYAML(base, overlay)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var merged map[string]any
	if err := yaml.Unmarshal(result, &merged); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	machine, ok := merged["machine"].(map[string]any)
	if !ok {
		t.Fatal("expected 'machine' to be a map")
	}

	// Base key preserved: install.disk
	install, ok := machine["install"].(map[string]any)
	if !ok {
		t.Fatal("expected 'machine.install' to be preserved from base")
	}
	if install["disk"] != "/dev/sda" {
		t.Fatalf("expected machine.install.disk = /dev/sda, got %v", install["disk"])
	}

	// Base key preserved: network.hostname
	network, ok := machine["network"].(map[string]any)
	if !ok {
		t.Fatal("expected 'machine.network' to be a map")
	}
	if network["hostname"] != "worker-1" {
		t.Fatalf("expected machine.network.hostname = worker-1, got %v", network["hostname"])
	}

	// Overlay key added: network.nameservers
	nameservers, ok := network["nameservers"].([]any)
	if !ok {
		t.Fatal("expected 'machine.network.nameservers' to be a list")
	}
	if len(nameservers) != 1 || nameservers[0] != "8.8.8.8" {
		t.Fatalf("expected nameservers = [8.8.8.8], got %v", nameservers)
	}

	// Overlay key added at machine level: certSANs
	certSANs, ok := machine["certSANs"].([]any)
	if !ok {
		t.Fatal("expected 'machine.certSANs' to be a list")
	}
	if len(certSANs) != 1 || certSANs[0] != "10.0.0.1" {
		t.Fatalf("expected certSANs = [10.0.0.1], got %v", certSANs)
	}
}

func TestMergeYAML_ArrayReplacement(t *testing.T) {
	// mergo.WithOverride replaces arrays — overlay array wins entirely.
	base := `
machine:
  network:
    nameservers:
      - 1.1.1.1
      - 8.8.8.8
`
	overlay := `
machine:
  network:
    nameservers:
      - 9.9.9.9
`
	result, err := mergeYAML(base, overlay)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var merged map[string]any
	if err := yaml.Unmarshal(result, &merged); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	machine := merged["machine"].(map[string]any)
	network := machine["network"].(map[string]any)
	nameservers := network["nameservers"].([]any)

	// Overlay array replaces base array — should be [9.9.9.9], NOT [1.1.1.1, 8.8.8.8, 9.9.9.9].
	if len(nameservers) != 1 {
		t.Fatalf("expected 1 nameserver (array replaced), got %d: %v", len(nameservers), nameservers)
	}
	if nameservers[0] != "9.9.9.9" {
		t.Fatalf("expected nameserver = 9.9.9.9, got %v", nameservers[0])
	}
}

func TestMergeYAML_OverrideConflictingKeys(t *testing.T) {
	base := `
machine:
  network:
    hostname: old-name
  install:
    disk: /dev/sda
`
	overlay := `
machine:
  network:
    hostname: new-name
  install:
    disk: /dev/nvme0n1
`
	result, err := mergeYAML(base, overlay)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var merged map[string]any
	if err := yaml.Unmarshal(result, &merged); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	machine := merged["machine"].(map[string]any)
	network := machine["network"].(map[string]any)
	install := machine["install"].(map[string]any)

	if network["hostname"] != "new-name" {
		t.Fatalf("expected hostname overridden to new-name, got %v", network["hostname"])
	}
	if install["disk"] != "/dev/nvme0n1" {
		t.Fatalf("expected disk overridden to /dev/nvme0n1, got %v", install["disk"])
	}
}

func TestMergeYAML_NilKeysPreservedFromBase(t *testing.T) {
	// Overlay has keys that base doesn't, and base has keys that overlay doesn't.
	// Both should be preserved.
	base := `
cluster:
  controlPlane:
    endpoint: https://10.0.0.1:6443
  network:
    podSubnets:
      - 10.244.0.0/16
`
	overlay := `
cluster:
  proxy:
    disabled: true
`
	result, err := mergeYAML(base, overlay)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var merged map[string]any
	if err := yaml.Unmarshal(result, &merged); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	cluster := merged["cluster"].(map[string]any)

	// Base keys preserved.
	cp := cluster["controlPlane"].(map[string]any)
	if cp["endpoint"] != "https://10.0.0.1:6443" {
		t.Fatalf("expected controlPlane.endpoint preserved, got %v", cp["endpoint"])
	}

	network := cluster["network"].(map[string]any)
	podSubnets := network["podSubnets"].([]any)
	if len(podSubnets) != 1 || podSubnets[0] != "10.244.0.0/16" {
		t.Fatalf("expected podSubnets preserved, got %v", podSubnets)
	}

	// Overlay key added.
	proxy := cluster["proxy"].(map[string]any)
	if proxy["disabled"] != true {
		t.Fatalf("expected proxy.disabled = true, got %v", proxy["disabled"])
	}
}

func TestMergeYAML_InvalidBaseYAML(t *testing.T) {
	_, err := mergeYAML("{{invalid yaml", "key: value")
	if err == nil {
		t.Fatal("expected error for invalid base YAML")
	}
	if !strings.Contains(err.Error(), "parse base YAML") {
		t.Fatalf("expected 'parse base YAML' in error, got: %s", err)
	}
}

func TestMergeYAML_InvalidOverlayYAML(t *testing.T) {
	_, err := mergeYAML("key: value", "{{invalid yaml")
	if err == nil {
		t.Fatal("expected error for invalid overlay YAML")
	}
	if !strings.Contains(err.Error(), "parse overlay YAML") {
		t.Fatalf("expected 'parse overlay YAML' in error, got: %s", err)
	}
}

func TestMergeYAML_TopLevelScalarOverride(t *testing.T) {
	base := "version: v1alpha1\nkind: worker\n"
	overlay := "kind: controlplane\n"

	result, err := mergeYAML(base, overlay)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var merged map[string]any
	if err := yaml.Unmarshal(result, &merged); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if merged["version"] != "v1alpha1" {
		t.Fatalf("expected version preserved, got %v", merged["version"])
	}
	if merged["kind"] != "controlplane" {
		t.Fatalf("expected kind overridden to controlplane, got %v", merged["kind"])
	}
}

func TestMergeYAML_DeeplyNestedMerge(t *testing.T) {
	base := `
a:
  b:
    c:
      d: base-value
      e: keep-me
`
	overlay := `
a:
  b:
    c:
      d: overlay-value
      f: new-key
`
	result, err := mergeYAML(base, overlay)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var merged map[string]any
	if err := yaml.Unmarshal(result, &merged); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	a := merged["a"].(map[string]any)
	b := a["b"].(map[string]any)
	c := b["c"].(map[string]any)

	if c["d"] != "overlay-value" {
		t.Fatalf("expected d overridden, got %v", c["d"])
	}
	if c["e"] != "keep-me" {
		t.Fatalf("expected e preserved from base, got %v", c["e"])
	}
	if c["f"] != "new-key" {
		t.Fatalf("expected f added from overlay, got %v", c["f"])
	}
}
