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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

const workerName = "worker-1"

// --- nodeInternalIP ---

func TestNodeInternalIP_Found(t *testing.T) {
	node := &corev1.Node{
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeHostName, Address: workerName},
				{Type: corev1.NodeInternalIP, Address: "10.0.0.5"},
				{Type: corev1.NodeExternalIP, Address: "203.0.113.5"},
			},
		},
	}
	ip := nodeInternalIP(node)
	if ip != "10.0.0.5" {
		t.Fatalf("expected 10.0.0.5, got %q", ip)
	}
}

func TestNodeInternalIP_NotFound(t *testing.T) {
	node := &corev1.Node{
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeHostName, Address: workerName},
				{Type: corev1.NodeExternalIP, Address: "203.0.113.5"},
			},
		},
	}
	ip := nodeInternalIP(node)
	if ip != "" {
		t.Fatalf("expected empty string, got %q", ip)
	}
}

func TestNodeInternalIP_EmptyAddresses(t *testing.T) {
	node := &corev1.Node{}
	ip := nodeInternalIP(node)
	if ip != "" {
		t.Fatalf("expected empty string, got %q", ip)
	}
}

func TestNodeInternalIP_FirstInternalIPReturned(t *testing.T) {
	// If a node has multiple InternalIPs (dual-stack), the first one wins.
	node := &corev1.Node{
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: corev1.NodeInternalIP, Address: "fd00::1"},
			},
		},
	}
	ip := nodeInternalIP(node)
	if ip != "10.0.0.1" {
		t.Fatalf("expected first InternalIP 10.0.0.1, got %q", ip)
	}
}

// --- isControlPlane ---

func TestIsControlPlane_WithLabel(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"node-role.kubernetes.io/control-plane": "",
				"kubernetes.io/hostname":                "cp-1",
			},
		},
	}
	if !isControlPlane(node) {
		t.Fatal("expected node with control-plane label to be identified as CP")
	}
}

func TestIsControlPlane_WithoutLabel(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"kubernetes.io/hostname": workerName,
			},
		},
	}
	if isControlPlane(node) {
		t.Fatal("expected node without control-plane label to NOT be identified as CP")
	}
}

func TestIsControlPlane_NilLabels(t *testing.T) {
	node := &corev1.Node{}
	if isControlPlane(node) {
		t.Fatal("expected node with nil labels to NOT be identified as CP")
	}
}

func TestIsControlPlane_LabelValueDoesNotMatter(t *testing.T) {
	// The label check is presence-based, not value-based.
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"node-role.kubernetes.io/control-plane": "true",
			},
		},
	}
	if !isControlPlane(node) {
		t.Fatal("expected node with control-plane label (value='true') to be identified as CP")
	}
}

// --- countUpdating ---

func TestCountUpdating_Empty(t *testing.T) {
	count := countUpdating(nil)
	if count != 0 {
		t.Fatalf("expected 0, got %d", count)
	}
}

func TestCountUpdating_NoneUpdating(t *testing.T) {
	assessments := []nodeAssessment{
		{isUpdating: false},
		{isUpdating: false},
		{isUpdating: false},
	}
	count := countUpdating(assessments)
	if count != 0 {
		t.Fatalf("expected 0, got %d", count)
	}
}

func TestCountUpdating_AllUpdating(t *testing.T) {
	assessments := []nodeAssessment{
		{isUpdating: true},
		{isUpdating: true},
	}
	count := countUpdating(assessments)
	if count != 2 {
		t.Fatalf("expected 2, got %d", count)
	}
}

func TestCountUpdating_Mixed(t *testing.T) {
	assessments := []nodeAssessment{
		{isUpdating: true},
		{isUpdating: false},
		{isUpdating: true},
		{isUpdating: false},
		{isUpdating: true},
	}
	count := countUpdating(assessments)
	if count != 3 {
		t.Fatalf("expected 3, got %d", count)
	}
}

// --- pickNextNode ---

func TestPickNextNode_PrefersWorkersOverCPs(t *testing.T) {
	cpNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp-1"}}
	workerNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: workerName}}

	assessments := []nodeAssessment{
		{node: cpNode, isCP: true, versionDrift: true, isUpdating: false},
		{node: workerNode, isCP: false, versionDrift: true, isUpdating: false},
	}

	picked := pickNextNode(assessments)
	if picked == nil {
		t.Fatal("expected a node to be picked")
	}
	if picked.node.Name != workerName {
		t.Fatalf("expected worker-1 to be preferred over cp-1, got %s", picked.node.Name)
	}
}

func TestPickNextNode_PicksCPWhenNoWorkersNeedUpdate(t *testing.T) {
	cpNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp-1"}}
	workerNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: workerName}}

	assessments := []nodeAssessment{
		{node: cpNode, isCP: true, configDrift: true, isUpdating: false},
		// Worker is synced (no drift).
		{node: workerNode, isCP: false, versionDrift: false, configDrift: false, isUpdating: false},
	}

	picked := pickNextNode(assessments)
	if picked == nil {
		t.Fatal("expected a node to be picked")
	}
	if picked.node.Name != "cp-1" {
		t.Fatalf("expected cp-1, got %s", picked.node.Name)
	}
}

func TestPickNextNode_NilWhenAllSynced(t *testing.T) {
	assessments := []nodeAssessment{
		{node: &corev1.Node{}, isCP: false, versionDrift: false, configDrift: false, isUpdating: false},
		{node: &corev1.Node{}, isCP: true, versionDrift: false, configDrift: false, isUpdating: false},
	}

	picked := pickNextNode(assessments)
	if picked != nil {
		t.Fatalf("expected nil when all nodes are synced, got %v", picked.node.Name)
	}
}

func TestPickNextNode_SkipsAlreadyUpdating(t *testing.T) {
	worker1 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: workerName}}
	worker2 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-2"}}

	assessments := []nodeAssessment{
		// worker-1 is drifted but already updating (cordoned).
		{node: worker1, isCP: false, versionDrift: true, isUpdating: true},
		// worker-2 is drifted and not yet updating.
		{node: worker2, isCP: false, versionDrift: true, isUpdating: false},
	}

	picked := pickNextNode(assessments)
	if picked == nil {
		t.Fatal("expected a node to be picked")
	}
	if picked.node.Name != "worker-2" {
		t.Fatalf("expected worker-2 (not already updating), got %s", picked.node.Name)
	}
}

func TestPickNextNode_SkipsNodesWithErrors(t *testing.T) {
	errNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "err-node"}}
	okNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "ok-node"}}

	assessments := []nodeAssessment{
		{node: errNode, isCP: false, versionDrift: true, isUpdating: false, err: errForTest("connection refused")},
		{node: okNode, isCP: false, configDrift: true, isUpdating: false},
	}

	picked := pickNextNode(assessments)
	if picked == nil {
		t.Fatal("expected a node to be picked")
	}
	if picked.node.Name != "ok-node" {
		t.Fatalf("expected ok-node (no error), got %s", picked.node.Name)
	}
}

func TestPickNextNode_NilWhenAllUpdating(t *testing.T) {
	assessments := []nodeAssessment{
		{node: &corev1.Node{}, isCP: false, versionDrift: true, isUpdating: true},
		{node: &corev1.Node{}, isCP: true, configDrift: true, isUpdating: true},
	}

	picked := pickNextNode(assessments)
	if picked != nil {
		t.Fatal("expected nil when all drifted nodes are already updating")
	}
}

func TestPickNextNode_EmptyAssessments(t *testing.T) {
	picked := pickNextNode(nil)
	if picked != nil {
		t.Fatal("expected nil for empty assessments")
	}
}

func TestPickNextNode_ConfigDriftAlsoCounts(t *testing.T) {
	worker := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: workerName}}

	assessments := []nodeAssessment{
		// Config drift but no version drift — should still be picked.
		{node: worker, isCP: false, versionDrift: false, configDrift: true, isUpdating: false},
	}

	picked := pickNextNode(assessments)
	if picked == nil {
		t.Fatal("expected node with config drift to be picked")
	}
	if picked.node.Name != workerName {
		t.Fatalf("expected worker-1, got %s", picked.node.Name)
	}
}

// --- hashConfig ---

func TestHashConfig_Deterministic(t *testing.T) {
	data := []byte("machine:\n  network:\n    hostname: worker-1\n")

	h1 := hashConfig(data)
	h2 := hashConfig(data)

	if h1 != h2 {
		t.Fatalf("hashConfig is not deterministic: %q != %q", h1, h2)
	}
}

func TestHashConfig_DifferentInputsDifferentHashes(t *testing.T) {
	d1 := []byte("machine:\n  network:\n    hostname: worker-1\n")
	d2 := []byte("machine:\n  network:\n    hostname: worker-2\n")

	h1 := hashConfig(d1)
	h2 := hashConfig(d2)

	if h1 == h2 {
		t.Fatal("different inputs should produce different hashes")
	}
}

func TestHashConfig_HasSHA256Prefix(t *testing.T) {
	h := hashConfig([]byte("test"))
	if len(h) < 7 || h[:7] != "sha256:" {
		t.Fatalf("expected sha256: prefix, got %q", h)
	}
}

func TestHashConfig_EmptyInput(t *testing.T) {
	// Empty input should still produce a valid, deterministic hash.
	h := hashConfig([]byte{})
	if len(h) == 0 {
		t.Fatal("expected non-empty hash for empty input")
	}
	if h[:7] != "sha256:" {
		t.Fatalf("expected sha256: prefix, got %q", h)
	}
	// SHA-256 of empty input is well-known.
	expected := "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if h != expected {
		t.Fatalf("expected %s, got %s", expected, h)
	}
}

func TestHashConfig_CorrectLength(t *testing.T) {
	h := hashConfig([]byte("data"))
	// "sha256:" (7) + 64 hex chars = 71 total.
	if len(h) != 71 {
		t.Fatalf("expected hash length 71, got %d (%q)", len(h), h)
	}
}

// --- matchesNode ---

func TestMatchesNode_NilSelector(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"some-label": "value"},
		},
	}
	if !matchesNode(nil, node) {
		t.Fatal("nil selector should match all nodes")
	}
}

func TestMatchesNode_MatchLabels_Match(t *testing.T) {
	sel := &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"kubernetes.io/hostname": workerName,
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"kubernetes.io/hostname":                workerName,
				"node-role.kubernetes.io/control-plane": "",
			},
		},
	}
	if !matchesNode(sel, node) {
		t.Fatal("expected selector to match node with matching label")
	}
}

func TestMatchesNode_MatchLabels_NoMatch(t *testing.T) {
	sel := &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"kubernetes.io/hostname": workerName,
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"kubernetes.io/hostname": "worker-2",
			},
		},
	}
	if matchesNode(sel, node) {
		t.Fatal("expected selector to NOT match node with different label value")
	}
}

func TestMatchesNode_MatchLabels_MultipleLabels(t *testing.T) {
	sel := &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"zone":   "us-east-1a",
			"tenant": "acme",
		},
	}
	// Node has both labels.
	nodeMatching := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"zone":   "us-east-1a",
				"tenant": "acme",
				"extra":  "ignored",
			},
		},
	}
	if !matchesNode(sel, nodeMatching) {
		t.Fatal("expected selector to match node with all required labels")
	}

	// Node missing one label.
	nodePartial := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"zone": "us-east-1a",
			},
		},
	}
	if matchesNode(sel, nodePartial) {
		t.Fatal("expected selector to NOT match node missing required label")
	}
}

func TestMatchesNode_EmptySelector(t *testing.T) {
	// Empty LabelSelector (no matchLabels, no matchExpressions) matches everything.
	sel := &metav1.LabelSelector{}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"anything": "goes"},
		},
	}
	if !matchesNode(sel, node) {
		t.Fatal("empty selector should match all nodes")
	}
}

func TestMatchesNode_MatchExpressions_In(t *testing.T) {
	sel := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      "zone",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{"us-east-1a", "us-east-1b"},
			},
		},
	}

	nodeMatch := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"zone": "us-east-1a"},
		},
	}
	if !matchesNode(sel, nodeMatch) {
		t.Fatal("expected In expression to match")
	}

	nodeNoMatch := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"zone": "eu-west-1a"},
		},
	}
	if matchesNode(sel, nodeNoMatch) {
		t.Fatal("expected In expression to NOT match different zone")
	}
}

func TestMatchesNode_MatchExpressions_NotIn(t *testing.T) {
	sel := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      "tier",
				Operator: metav1.LabelSelectorOpNotIn,
				Values:   []string{"test"},
			},
		},
	}

	prodNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"tier": "production"},
		},
	}
	if !matchesNode(sel, prodNode) {
		t.Fatal("expected NotIn to match node with different value")
	}

	testNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{"tier": "test"},
		},
	}
	if matchesNode(sel, testNode) {
		t.Fatal("expected NotIn to NOT match excluded value")
	}
}

func TestMatchesNode_MatchExpressions_Exists(t *testing.T) {
	sel := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      "node-role.kubernetes.io/control-plane",
				Operator: metav1.LabelSelectorOpExists,
			},
		},
	}

	cpNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"node-role.kubernetes.io/control-plane": "",
			},
		},
	}
	if !matchesNode(sel, cpNode) {
		t.Fatal("expected Exists to match node with label present")
	}

	workerNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"kubernetes.io/hostname": workerName,
			},
		},
	}
	if matchesNode(sel, workerNode) {
		t.Fatal("expected Exists to NOT match node without label")
	}
}

func TestMatchesNode_MatchExpressions_DoesNotExist(t *testing.T) {
	sel := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      "node-role.kubernetes.io/control-plane",
				Operator: metav1.LabelSelectorOpDoesNotExist,
			},
		},
	}

	workerNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"kubernetes.io/hostname": workerName,
			},
		},
	}
	if !matchesNode(sel, workerNode) {
		t.Fatal("expected DoesNotExist to match node without the label")
	}

	cpNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"node-role.kubernetes.io/control-plane": "",
			},
		},
	}
	if matchesNode(sel, cpNode) {
		t.Fatal("expected DoesNotExist to NOT match node with the label")
	}
}

func TestMatchesNode_CombinedMatchLabelsAndExpressions(t *testing.T) {
	sel := &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"zone": "us-east-1a",
		},
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      "node-role.kubernetes.io/control-plane",
				Operator: metav1.LabelSelectorOpDoesNotExist,
			},
		},
	}

	// Matches: right zone, no CP label.
	workerInZone := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"zone":                   "us-east-1a",
				"kubernetes.io/hostname": workerName,
			},
		},
	}
	if !matchesNode(sel, workerInZone) {
		t.Fatal("expected combined selector to match worker in correct zone")
	}

	// Doesn't match: right zone, but has CP label.
	cpInZone := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"zone":                                  "us-east-1a",
				"node-role.kubernetes.io/control-plane": "",
			},
		},
	}
	if matchesNode(sel, cpInZone) {
		t.Fatal("expected combined selector to NOT match CP node")
	}

	// Doesn't match: wrong zone.
	workerWrongZone := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"zone": "eu-west-1a",
			},
		},
	}
	if matchesNode(sel, workerWrongZone) {
		t.Fatal("expected combined selector to NOT match wrong zone")
	}
}

// --- ConvertLabelSelector (api/v1alpha1 helper, tested via the controller wrapper) ---

func TestConvertLabelSelector_MatchLabels(t *testing.T) {
	sel := &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"app": "nginx",
		},
	}
	selector, err := convertLabelSelector(sel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the selector string is parseable and correct.
	str := selector.String()
	if str != "app=nginx" {
		t.Fatalf("expected selector string 'app=nginx', got %q", str)
	}
}

func TestConvertLabelSelector_MatchExpressions(t *testing.T) {
	sel := &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      "tier",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{"frontend", "backend"},
			},
		},
	}
	selector, err := convertLabelSelector(sel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify it matches expected labels.
	if !selector.Matches(labels.Set{"tier": "frontend"}) {
		t.Fatal("expected selector to match tier=frontend")
	}
	if !selector.Matches(labels.Set{"tier": "backend"}) {
		t.Fatal("expected selector to match tier=backend")
	}
	if selector.Matches(labels.Set{"tier": "database"}) {
		t.Fatal("expected selector to NOT match tier=database")
	}
}

func TestConvertLabelSelector_EmptySelector(t *testing.T) {
	sel := &metav1.LabelSelector{}
	selector, err := convertLabelSelector(sel)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Empty selector matches everything.
	if !selector.Matches(labels.Set{"any": "label"}) {
		t.Fatal("empty selector should match any labels")
	}
}

func TestConvertLabelSelector_NilSelector(t *testing.T) {
	selector, err := convertLabelSelector(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// metav1.LabelSelectorAsSelector(nil) returns labels.Nothing() — matches nothing.
	// This is the Kubernetes convention: nil *LabelSelector = select nothing,
	// empty LabelSelector{} = select everything.
	if selector.Matches(labels.Set{"any": "label"}) {
		t.Fatal("nil selector should match nothing (Kubernetes convention)")
	}
}

// --- isDaemonSetPod / isMirrorPod (bonus: testing exported helpers) ---

func TestIsDaemonSetPod_True(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "DaemonSet", Name: "kube-proxy"},
			},
		},
	}
	if !isDaemonSetPod(pod) {
		t.Fatal("expected pod owned by DaemonSet to return true")
	}
}

func TestIsDaemonSetPod_False(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "nginx-abc123"},
			},
		},
	}
	if isDaemonSetPod(pod) {
		t.Fatal("expected pod owned by ReplicaSet to return false")
	}
}

func TestIsDaemonSetPod_NoOwners(t *testing.T) {
	pod := &corev1.Pod{}
	if isDaemonSetPod(pod) {
		t.Fatal("expected pod with no owners to return false")
	}
}

func TestIsMirrorPod_True(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"kubernetes.io/config.mirror": "abc123",
			},
		},
	}
	if !isMirrorPod(pod) {
		t.Fatal("expected pod with mirror annotation to return true")
	}
}

func TestIsMirrorPod_False(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"other-annotation": "value",
			},
		},
	}
	if isMirrorPod(pod) {
		t.Fatal("expected pod without mirror annotation to return false")
	}
}

func TestIsMirrorPod_NoAnnotations(t *testing.T) {
	pod := &corev1.Pod{}
	if isMirrorPod(pod) {
		t.Fatal("expected pod with no annotations to return false")
	}
}

// errForTest is a simple error implementation for test fixtures.
type errForTest string

func (e errForTest) Error() string { return string(e) }
