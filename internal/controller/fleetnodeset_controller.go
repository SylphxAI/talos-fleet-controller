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
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"strings"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1alpha1 "github.com/SylphxAI/talos-fleet-controller/api/v1alpha1"
	"github.com/SylphxAI/talos-fleet-controller/internal/talos"
)

const (
	// requeueInterval is the default requeue interval for periodic drift checks.
	requeueInterval = 60 * time.Second

	// requeueProgressingInterval is the requeue interval when an update is in progress.
	requeueProgressingInterval = 10 * time.Second
)

// FleetNodeSetReconciler reconciles a FleetNodeSet object.
// It compares desired state (Talos version + machine config) against actual
// node state and performs sequential updates with safety guarantees.
type FleetNodeSetReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	TalosClient *talos.Client
	Recorder    record.EventRecorder
}

// +kubebuilder:rbac:groups=fleet.talos.dev,resources=fleetnodesets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=fleet.talos.dev,resources=fleetnodesets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=fleet.talos.dev,resources=fleetnodesets/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile compares desired state against actual node state and performs
// sequential updates. Each reconcile cycle:
//  1. Selects nodes matching nodeSelector
//  2. For each node: reads actual version + config, diffs against desired
//  3. If any node is drifted: picks one, cordons, drains, applies, health checks, uncordons
//  4. Updates FleetNodeSet status
func (r *FleetNodeSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the FleetNodeSet.
	var fns fleetv1alpha1.FleetNodeSet
	if err := r.Get(ctx, req.NamespacedName, &fns); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.Info("reconciling", "name", fns.Name, "generation", fns.Generation)
	reconcileStart := time.Now()
	defer func() {
		reconcileDuration.WithLabelValues(fns.Name).Observe(time.Since(reconcileStart).Seconds())
	}()

	// Emergency brake — stop all updates.
	if fns.Spec.Paused {
		log.Info("paused — skipping reconciliation")
		fns.Status.Phase = fleetv1alpha1.FleetPhasePaused
		if err := r.Status().Update(ctx, &fns); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}

	// 1. Select nodes matching nodeSelector.
	nodes, err := r.selectNodes(ctx, fns.Spec.NodeSelector)
	if err != nil {
		log.Error(err, "failed to select nodes")
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}

	if len(nodes) == 0 {
		log.Info("no nodes match selector")
		fns.Status.Phase = fleetv1alpha1.FleetPhaseSynced
		fns.Status.Total = 0
		fns.Status.Synced = 0
		if err := r.Status().Update(ctx, &fns); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}

	// 2. Assess each node: compute desired vs actual.
	nodeStates := make([]nodeAssessment, 0, len(nodes))
	for i := range nodes {
		node := &nodes[i]
		assessment := r.assessNode(ctx, node, &fns)
		nodeStates = append(nodeStates, assessment)
	}

	// 3. Update status from assessments.
	r.updateStatus(&fns, nodeStates)

	// Record metrics from status.
	recordMetrics(fns.Name, fns.Status.Total, fns.Status.Synced, fns.Status.Pending, fns.Status.Failed)

	// 4. If all synced, nothing to do.
	if fns.Status.Synced == fns.Status.Total {
		fns.Status.Phase = fleetv1alpha1.FleetPhaseSynced
		fns.Status.ObservedGeneration = fns.Generation
		if err := r.Status().Update(ctx, &fns); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("all nodes synced", "total", fns.Status.Total)
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}

	// 5. Dry-run mode — report drift but don't apply.
	if fns.Spec.DryRun {
		log.Info("dry-run mode — reporting drift without applying",
			"drifted", fns.Status.Pending+fns.Status.Progressing)
		fns.Status.Phase = fleetv1alpha1.FleetPhaseProgressing
		fns.Status.ObservedGeneration = fns.Generation
		if err := r.Status().Update(ctx, &fns); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueInterval}, nil
	}

	// 6. Check maxUnavailable — how many nodes are currently updating?
	updating := countUpdating(nodeStates)
	maxUnavailable := fns.Spec.UpdateStrategy.MaxUnavailable
	if maxUnavailable <= 0 {
		maxUnavailable = 1
	}
	if updating >= maxUnavailable {
		log.Info("maxUnavailable reached, waiting", "updating", updating, "max", maxUnavailable)
		fns.Status.Phase = fleetv1alpha1.FleetPhaseProgressing
		if err := r.Status().Update(ctx, &fns); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueProgressingInterval}, nil
	}

	// 7. Pick next node to update (prefer workers over CPs).
	next := pickNextNode(nodeStates)
	if next == nil {
		// All drifted nodes are already updating — wait.
		fns.Status.Phase = fleetv1alpha1.FleetPhaseProgressing
		if err := r.Status().Update(ctx, &fns); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueProgressingInterval}, nil
	}

	log.Info("updating node", "node", next.node.Name,
		"versionDrift", next.versionDrift, "configDrift", next.configDrift)
	r.Recorder.Eventf(&fns, corev1.EventTypeNormal, "UpdateStarted",
		"Starting update on node %s (versionDrift=%v, configDrift=%v)",
		next.node.Name, next.versionDrift, next.configDrift)

	// 8. Execute update sequence: cordon → drain → apply/upgrade → health → uncordon.
	if err := r.executeNodeUpdate(ctx, &fns, next); err != nil {
		log.Error(err, "node update failed", "node", next.node.Name)
		r.Recorder.Eventf(&fns, corev1.EventTypeWarning, "UpdateFailed",
			"Node %s update failed: %v", next.node.Name, err)
		r.setNodeStatus(&fns, next.node.Name, fleetv1alpha1.NodePhaseFailed, err.Error())
	} else {
		r.Recorder.Eventf(&fns, corev1.EventTypeNormal, "UpdateCompleted",
			"Node %s updated successfully", next.node.Name)
	}

	// 9. Update status and requeue for next node.
	fns.Status.Phase = fleetv1alpha1.FleetPhaseProgressing
	fns.Status.ObservedGeneration = fns.Generation
	if err := r.Status().Update(ctx, &fns); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: requeueProgressingInterval}, nil
}

// nodeAssessment holds the diff between desired and actual state for one node.
type nodeAssessment struct {
	node         *corev1.Node
	nodeIP       string
	isCP         bool
	versionDrift bool // actual version != desired version
	configDrift  bool // actual config hash != desired config hash
	currentVer   string
	desiredVer   string
	currentHash  string
	desiredHash  string
	isUpdating   bool // node is currently being updated (cordoned, etc.)
	err          error
}

// selectNodes returns K8s Node objects matching the label selector.
func (r *FleetNodeSetReconciler) selectNodes(ctx context.Context, sel *fleetv1alpha1.LabelSelector) ([]corev1.Node, error) {
	if sel == nil {
		return nil, fmt.Errorf("nodeSelector is required")
	}

	selector, err := convertLabelSelector(sel)
	if err != nil {
		return nil, fmt.Errorf("invalid nodeSelector: %w", err)
	}

	var nodeList corev1.NodeList
	if err := r.List(ctx, &nodeList, &client.ListOptions{LabelSelector: selector}); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	return nodeList.Items, nil
}

// assessNode compares a node's actual state against the FleetNodeSet's desired state.
func (r *FleetNodeSetReconciler) assessNode(ctx context.Context, node *corev1.Node, fns *fleetv1alpha1.FleetNodeSet) nodeAssessment {
	log := logf.FromContext(ctx)

	a := nodeAssessment{
		node:       node,
		desiredVer: fns.Spec.Talos.Version,
		isCP:       isControlPlane(node),
		isUpdating: node.Spec.Unschedulable, // cordoned = updating
	}

	// Get node internal IP for Talos API access.
	a.nodeIP = nodeInternalIP(node)
	if a.nodeIP == "" {
		a.err = fmt.Errorf("node %s has no InternalIP", node.Name)
		return a
	}

	// Get actual Talos version.
	ver, err := r.TalosClient.NodeVersion(ctx, a.nodeIP)
	if err != nil {
		log.Error(err, "failed to get version", "node", node.Name)
		a.err = err
		return a
	}
	a.currentVer = ver
	a.versionDrift = (ver != fns.Spec.Talos.Version)

	// Drift detection: compare desired PATCH hash, not node config.
	//
	// Trying to predict what a node's config will look like after apply
	// is unreliable — Talos normalizes config internally, Clone() changes
	// field ordering, and EncodeBytes() is not deterministic across paths.
	//
	// Instead, we use the Terraform pattern:
	//   desiredPatchHash = hash(patch we want to apply)
	//   lastAppliedPatchHash = hash stored in status after last successful apply
	//   If equal → already applied this exact patch → Synced
	//   If different → CRD spec changed or never applied → Apply
	//
	// This is simple, reliable, and fully deterministic — the patch bytes
	// come directly from the FleetNodeSet spec, not from config parsing.

	desiredPatch, err := r.buildDesiredPatch(fns, node)
	if err != nil {
		a.err = err
		return a
	}
	a.desiredHash = hashConfig(desiredPatch)

	// Check if we already applied this exact patch to this node.
	lastApplied := r.getNodeLastApplied(fns, node.Name)
	if lastApplied != nil && lastApplied.LastAppliedConfigHash == a.desiredHash {
		// Same patch already applied → Synced.
		a.configDrift = false
		a.currentHash = a.desiredHash
	} else {
		// Never applied, or CRD spec changed → drift.
		a.configDrift = true
		a.currentHash = ""
		if lastApplied != nil {
			a.currentHash = lastApplied.LastAppliedConfigHash
		}
	}

	return a
}

// buildDesiredPatch merges base machineConfig + applicable nodeOverride for a node.
// Returns a YAML strategic merge patch — applied to current config via configpatcher.
func (r *FleetNodeSetReconciler) buildDesiredPatch(fns *fleetv1alpha1.FleetNodeSet, node *corev1.Node) ([]byte, error) {
	var base string
	if fns.Spec.MachineConfig != nil {
		base = fns.Spec.MachineConfig.Inline
		// TODO: support secretRef
	}

	// Find matching nodeOverride and merge on top of base via configpatcher.
	for _, override := range fns.Spec.NodeOverrides {
		if matchesNode(override.NodeSelector, node) {
			merged, err := mergePatches(base, override.MachineConfig.Inline)
			if err != nil {
				return nil, fmt.Errorf("merge nodeOverride for %s: %w", node.Name, err)
			}
			return merged, nil
		}
	}

	return []byte(base), nil
}

// executeNodeUpdate performs the full update sequence for a single node.
func (r *FleetNodeSetReconciler) executeNodeUpdate(ctx context.Context, fns *fleetv1alpha1.FleetNodeSet, a *nodeAssessment) error {
	log := logf.FromContext(ctx)
	nodeName := a.node.Name

	// Pre-flight: etcd quorum check for CP nodes.
	if a.isCP {
		members, err := r.TalosClient.EtcdMembers(ctx, a.nodeIP)
		if err != nil {
			return fmt.Errorf("etcd quorum check: %w", err)
		}
		// Need at least 3 members and quorum (N/2+1) must survive with N-1.
		if members < 3 {
			return fmt.Errorf("etcd has only %d members — need at least 3 for safe CP update", members)
		}
		log.Info("etcd quorum OK", "members", members, "node", nodeName)
	}

	// Step 1: Cordon.
	r.setNodeStatus(fns, nodeName, fleetv1alpha1.NodePhaseCordoning, "Cordoning node")
	if err := r.cordonNode(ctx, a.node); err != nil {
		return fmt.Errorf("cordon %s: %w", nodeName, err)
	}

	// Step 2: Drain.
	r.setNodeStatus(fns, nodeName, fleetv1alpha1.NodePhaseDraining, "Draining workloads")
	if err := r.drainNode(ctx, a.node, fns.Spec.UpdateStrategy.DrainTimeout.Duration); err != nil {
		// Drain failure is non-fatal — log and continue (PDB may block).
		log.Error(err, "drain incomplete, proceeding", "node", nodeName)
	}

	// Step 3: Apply config if drifted.
	if a.configDrift {
		r.setNodeStatus(fns, nodeName, fleetv1alpha1.NodePhaseApplying, "Applying machine config")

		// Build desired patch from FleetNodeSet spec.
		desiredPatch, err := r.buildDesiredPatch(fns, a.node)
		if err != nil {
			_ = r.uncordonNode(ctx, a.node)
			return fmt.Errorf("build config patch for %s: %w", nodeName, err)
		}

		// Read current config as typed provider, apply patch via configpatcher.
		currentProvider, err := r.TalosClient.NodeConfigProvider(ctx, a.nodeIP)
		if err != nil {
			_ = r.uncordonNode(ctx, a.node)
			return fmt.Errorf("read current config from %s: %w", nodeName, err)
		}

		// Apply patch using Talos's own configpatcher (multi-doc, strategic merge).
		patchedProvider, err := applyPatchToConfig(currentProvider, desiredPatch)
		if err != nil {
			_ = r.uncordonNode(ctx, a.node)
			return fmt.Errorf("apply config patch for %s: %w", nodeName, err)
		}

		// Serialize patched config to YAML for ApplyConfiguration API.
		mergedConfig, err := configToBytes(patchedProvider)
		if err != nil {
			_ = r.uncordonNode(ctx, a.node)
			return fmt.Errorf("encode patched config for %s: %w", nodeName, err)
		}

		// Smart apply mode: if the config change affects kubelet (extraArgs,
		// extraMounts, etc.), use "reboot" mode instead of "auto". This avoids
		// the kubelet restart bind mount conflict (Talos #13053) where rshared
		// propagated mounts are not cleaned up between container restarts.
		// A full reboot clears all mount namespaces safely.
		mode := talos.ApplyMode(fns.Spec.UpdateStrategy.ConfigApplyMode)
		if mode == talos.ApplyModeAuto && configAffectsKubelet(desiredPatch) {
			log.Info("config affects kubelet — using reboot mode to avoid mount conflict (talos#13053)",
				"node", nodeName)
			mode = talos.ApplyModeReboot
		}
		if err := r.TalosClient.ApplyConfig(ctx, a.nodeIP, mergedConfig, mode); err != nil {
			updatesTotal.WithLabelValues(fns.Name, "config", "failure").Inc()
			_ = r.uncordonNode(ctx, a.node)
			return fmt.Errorf("apply config to %s: %w", nodeName, err)
		}
		updatesTotal.WithLabelValues(fns.Name, "config", "success").Inc()
		log.Info("config applied", "node", nodeName)
	}

	// Step 4: Upgrade if version drifted.
	if a.versionDrift {
		r.setNodeStatus(fns, nodeName, fleetv1alpha1.NodePhaseUpgrading, fmt.Sprintf("Upgrading to %s", fns.Spec.Talos.Version))
		installerImage := fmt.Sprintf("%s/installer/%s:%s",
			fns.Spec.Talos.FactoryURL, fns.Spec.Talos.Schematic, fns.Spec.Talos.Version)
		if err := r.TalosClient.Upgrade(ctx, a.nodeIP, installerImage); err != nil {
			_ = r.uncordonNode(ctx, a.node) // best-effort uncordon on failure
			return fmt.Errorf("upgrade %s: %w", nodeName, err)
		}
		log.Info("upgrade started", "node", nodeName, "image", installerImage)
	}

	// Step 5: Wait for node to become Ready.
	r.setNodeStatus(fns, nodeName, fleetv1alpha1.NodePhaseHealthChecking, "Waiting for node Ready")
	timeout := fns.Spec.UpdateStrategy.HealthCheckTimeout.Duration
	if timeout == 0 {
		timeout = 120 * time.Second
	}
	if err := r.waitNodeReady(ctx, a.node.Name, timeout); err != nil {
		_ = r.uncordonNode(ctx, a.node)
		return fmt.Errorf("health check %s: %w", nodeName, err)
	}

	// Step 6: Uncordon.
	if err := r.uncordonNode(ctx, a.node); err != nil {
		return fmt.Errorf("uncordon %s: %w", nodeName, err)
	}

	// Store the PATCH hash (not node config hash) — this is what we compare
	// on subsequent cycles to determine if the CRD spec changed.
	desiredPatch, _ := r.buildDesiredPatch(fns, a.node)
	patchHash := hashConfig(desiredPatch)

	now := time.Now()
	r.setNodeStatusSynced(fns, nodeName, fns.Spec.Talos.Version, patchHash, fns.Generation, &now)
	log.Info("node updated successfully", "node", nodeName)

	return nil
}

// cordonNode marks a node as unschedulable.
func (r *FleetNodeSetReconciler) cordonNode(ctx context.Context, node *corev1.Node) error {
	if node.Spec.Unschedulable {
		return nil // already cordoned
	}
	patch := client.MergeFrom(node.DeepCopy())
	node.Spec.Unschedulable = true
	return r.Patch(ctx, node, patch)
}

// uncordonNode marks a node as schedulable.
func (r *FleetNodeSetReconciler) uncordonNode(ctx context.Context, node *corev1.Node) error {
	// Re-fetch to avoid conflict.
	var fresh corev1.Node
	if err := r.Get(ctx, client.ObjectKeyFromObject(node), &fresh); err != nil {
		return err
	}
	if !fresh.Spec.Unschedulable {
		return nil // already uncordoned
	}
	patch := client.MergeFrom(fresh.DeepCopy())
	fresh.Spec.Unschedulable = false
	return r.Patch(ctx, &fresh, patch)
}

// drainNode evicts all non-DaemonSet, non-mirror pods from a node and waits
// for them to terminate. Respects PodDisruptionBudgets. Times out after duration.
func (r *FleetNodeSetReconciler) drainNode(ctx context.Context, node *corev1.Node, timeout time.Duration) error {
	log := logf.FromContext(ctx)

	if timeout == 0 {
		timeout = 300 * time.Second
	}
	deadline := time.Now().Add(timeout)

	// List all pods on this node using label selector (no field indexer needed).
	var podList corev1.PodList
	if err := r.List(ctx, &podList); err != nil {
		return fmt.Errorf("list pods: %w", err)
	}

	// Filter to pods on this node.
	var podsToEvict []corev1.Pod
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Spec.NodeName != node.Name {
			continue
		}
		if isDaemonSetPod(pod) || isMirrorPod(pod) {
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		podsToEvict = append(podsToEvict, *pod)
	}

	if len(podsToEvict) == 0 {
		log.Info("no pods to drain", "node", node.Name)
		return nil
	}

	log.Info("draining node", "node", node.Name, "pods", len(podsToEvict))

	// Phase 1: Send eviction requests.
	evicted := 0
	for i := range podsToEvict {
		pod := &podsToEvict[i]
		if time.Now().After(deadline) {
			return fmt.Errorf("drain timeout after evicting %d/%d pods on %s", evicted, len(podsToEvict), node.Name)
		}

		eviction := &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pod.Name,
				Namespace: pod.Namespace,
			},
		}

		err := r.SubResource("eviction").Create(ctx, pod, eviction)
		if err != nil {
			if apierrors.IsTooManyRequests(err) {
				// PDB blocking — wait and retry on next drain cycle.
				log.Info("PDB blocking eviction, will retry", "pod", pod.Name)
				time.Sleep(5 * time.Second)
				continue
			}
			if apierrors.IsNotFound(err) {
				continue // already gone
			}
			log.Error(err, "eviction failed", "pod", pod.Name, "namespace", pod.Namespace)
			continue
		}
		evicted++
	}

	// Phase 2: Wait for evicted pods to actually terminate.
	for time.Now().Before(deadline) {
		remaining := 0
		for i := range podsToEvict {
			var pod corev1.Pod
			err := r.Get(ctx, client.ObjectKey{
				Name:      podsToEvict[i].Name,
				Namespace: podsToEvict[i].Namespace,
			}, &pod)
			if apierrors.IsNotFound(err) {
				continue // pod gone
			}
			if err != nil {
				continue // can't check, assume gone
			}
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				continue // terminated
			}
			remaining++
		}

		if remaining == 0 {
			log.Info("drain complete", "node", node.Name, "evicted", evicted)
			return nil
		}

		log.Info("waiting for pod termination", "node", node.Name, "remaining", remaining)
		time.Sleep(5 * time.Second)
	}

	return fmt.Errorf("drain timeout: pods still running on %s after %s", node.Name, timeout)
}

// isDaemonSetPod returns true if the pod is owned by a DaemonSet.
func isDaemonSetPod(pod *corev1.Pod) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

// isMirrorPod returns true if the pod is a static/mirror pod.
func isMirrorPod(pod *corev1.Pod) bool {
	_, ok := pod.Annotations["kubernetes.io/config.mirror"]
	return ok
}

// waitNodeReady polls until the node's Ready condition is True or timeout.
func (r *FleetNodeSetReconciler) waitNodeReady(ctx context.Context, nodeName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var node corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				return nil
			}
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("node %s not Ready within %s", nodeName, timeout)
}

// --- Status helpers ---

func (r *FleetNodeSetReconciler) updateStatus(fns *fleetv1alpha1.FleetNodeSet, assessments []nodeAssessment) {
	fns.Status.Total = len(assessments)
	fns.Status.Synced = 0
	fns.Status.Progressing = 0
	fns.Status.Pending = 0
	fns.Status.Failed = 0
	fns.Status.Nodes = make([]fleetv1alpha1.FleetNodeStatus, 0, len(assessments))

	// Build a map of existing node statuses to preserve lastApplied fields.
	existingStatus := make(map[string]*fleetv1alpha1.FleetNodeStatus)
	for i := range fns.Status.Nodes {
		existingStatus[fns.Status.Nodes[i].Name] = &fns.Status.Nodes[i]
	}

	for _, a := range assessments {
		ns := fleetv1alpha1.FleetNodeStatus{
			Name:              a.node.Name,
			CurrentVersion:    a.currentVer,
			DesiredVersion:    a.desiredVer,
			CurrentConfigHash: a.currentHash,
			DesiredConfigHash: a.desiredHash,
		}

		// Preserve lastApplied fields from previous status.
		if existing, ok := existingStatus[a.node.Name]; ok {
			ns.LastAppliedConfigHash = existing.LastAppliedConfigHash
			ns.LastAppliedGeneration = existing.LastAppliedGeneration
			ns.LastApplied = existing.LastApplied
		}

		switch {
		case a.err != nil:
			ns.Phase = fleetv1alpha1.NodePhaseFailed
			ns.Message = a.err.Error()
			fns.Status.Failed++
		case a.isUpdating:
			ns.Phase = fleetv1alpha1.NodePhaseApplying
			ns.Message = "Update in progress"
			fns.Status.Progressing++
		case a.versionDrift || a.configDrift:
			ns.Phase = fleetv1alpha1.NodePhasePending
			if a.versionDrift {
				ns.Message = fmt.Sprintf("Version drift: %s → %s", a.currentVer, a.desiredVer)
			} else {
				ns.Message = "Config drift detected"
			}
			fns.Status.Pending++
		default:
			ns.Phase = fleetv1alpha1.NodePhaseSynced
			fns.Status.Synced++
		}

		fns.Status.Nodes = append(fns.Status.Nodes, ns)
	}
}

func (r *FleetNodeSetReconciler) setNodeStatus(fns *fleetv1alpha1.FleetNodeSet, nodeName string, phase fleetv1alpha1.NodePhase, msg string) {
	for i := range fns.Status.Nodes {
		if fns.Status.Nodes[i].Name == nodeName {
			fns.Status.Nodes[i].Phase = phase
			fns.Status.Nodes[i].Message = msg
			return
		}
	}
}

func (r *FleetNodeSetReconciler) setNodeStatusSynced(fns *fleetv1alpha1.FleetNodeSet, nodeName, version, configHash string, generation int64, t *time.Time) {
	ts := fleetv1alpha1.NewTime(*t)
	for i := range fns.Status.Nodes {
		if fns.Status.Nodes[i].Name == nodeName {
			fns.Status.Nodes[i].Phase = fleetv1alpha1.NodePhaseSynced
			fns.Status.Nodes[i].CurrentVersion = version
			fns.Status.Nodes[i].Message = ""
			fns.Status.Nodes[i].LastApplied = &ts
			fns.Status.Nodes[i].LastAppliedConfigHash = configHash
			fns.Status.Nodes[i].LastAppliedGeneration = generation
			return
		}
	}
}

// getNodeLastApplied returns the stored status for a node, or nil if not found.
func (r *FleetNodeSetReconciler) getNodeLastApplied(fns *fleetv1alpha1.FleetNodeSet, nodeName string) *fleetv1alpha1.FleetNodeStatus {
	for i := range fns.Status.Nodes {
		if fns.Status.Nodes[i].Name == nodeName {
			return &fns.Status.Nodes[i]
		}
	}
	return nil
}

// configAffectsKubelet checks if a config patch contains fields that would
// trigger a kubelet restart. When kubelet restarts with rshared extraMounts
// (common with CSI drivers like TopoLVM, Longhorn, Rook Ceph), propagated
// bind mounts are not cleaned up, causing ENOSPC on re-mount.
// See: https://github.com/siderolabs/talos/issues/13053
//
// When detected, TFC uses "reboot" mode instead of "auto" to safely clear
// all mount namespaces via full reboot.
func configAffectsKubelet(patchYAML []byte) bool {
	patchStr := string(patchYAML)
	kubeletFields := []string{
		"kubelet:",
		"extraArgs:",
		"extraMounts:",
		"extraConfig:",
		"clusterDNS:",
		"nodeIP:",
	}
	for _, field := range kubeletFields {
		if strings.Contains(patchStr, field) {
			return true
		}
	}
	return false
}

// --- Node helpers ---

func nodeInternalIP(node *corev1.Node) string {
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address
		}
	}
	return ""
}

func isControlPlane(node *corev1.Node) bool {
	_, ok := node.Labels["node-role.kubernetes.io/control-plane"]
	return ok
}

func countUpdating(assessments []nodeAssessment) int {
	count := 0
	for _, a := range assessments {
		if a.isUpdating {
			count++
		}
	}
	return count
}

// pickNextNode selects the next drifted node to update.
// Prefers workers over CPs (safer to update workers first).
func pickNextNode(assessments []nodeAssessment) *nodeAssessment {
	// First pass: pick a drifted worker.
	for i := range assessments {
		a := &assessments[i]
		if !a.isUpdating && !a.isCP && (a.versionDrift || a.configDrift) && a.err == nil {
			return a
		}
	}
	// Second pass: pick a drifted CP.
	for i := range assessments {
		a := &assessments[i]
		if !a.isUpdating && a.isCP && (a.versionDrift || a.configDrift) && a.err == nil {
			return a
		}
	}
	return nil
}

func matchesNode(sel *fleetv1alpha1.LabelSelector, node *corev1.Node) bool {
	if sel == nil {
		return true
	}
	selector, err := convertLabelSelector(sel)
	if err != nil {
		return false
	}
	return selector.Matches(labels.Set(node.Labels))
}

func convertLabelSelector(sel *fleetv1alpha1.LabelSelector) (labels.Selector, error) {
	// FleetNodeSet uses the standard metav1.LabelSelector type.
	// Convert to labels.Selector for matching.
	return fleetv1alpha1.ConvertLabelSelector(sel)
}

func hashConfig(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", h)
}

// SetupWithManager sets up the controller with the Manager.
func (r *FleetNodeSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("talos-fleet-controller") //nolint:staticcheck // GetEventRecorder not yet available in v0.23

	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1alpha1.FleetNodeSet{}).
		Named("fleetnodeset").
		Complete(r)
}
