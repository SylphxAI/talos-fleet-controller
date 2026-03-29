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
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// nodesTotal tracks the total number of nodes per FleetNodeSet.
	nodesTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "tfc_nodes_total",
			Help: "Total number of nodes managed by a FleetNodeSet.",
		},
		[]string{"fleetnodeset"},
	)

	// nodesSynced tracks the number of synced nodes per FleetNodeSet.
	nodesSynced = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "tfc_nodes_synced",
			Help: "Number of nodes at desired state in a FleetNodeSet.",
		},
		[]string{"fleetnodeset"},
	)

	// nodesDrifted tracks the number of drifted nodes per FleetNodeSet.
	nodesDrifted = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "tfc_nodes_drifted",
			Help: "Number of nodes with config or version drift in a FleetNodeSet.",
		},
		[]string{"fleetnodeset"},
	)

	// nodesFailed tracks the number of failed nodes per FleetNodeSet.
	nodesFailed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "tfc_nodes_failed",
			Help: "Number of nodes that failed to update in a FleetNodeSet.",
		},
		[]string{"fleetnodeset"},
	)

	// reconcileDuration tracks reconcile loop duration.
	reconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "tfc_reconcile_duration_seconds",
			Help:    "Duration of FleetNodeSet reconcile loop in seconds.",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 10), // 0.1s to ~100s
		},
		[]string{"fleetnodeset"},
	)

	// updatesTotal tracks total update attempts.
	updatesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "tfc_updates_total",
			Help: "Total number of node update attempts.",
		},
		[]string{"fleetnodeset", "type", "result"}, // type=config|upgrade, result=success|failure
	)
)

func init() {
	metrics.Registry.MustRegister(
		nodesTotal,
		nodesSynced,
		nodesDrifted,
		nodesFailed,
		reconcileDuration,
		updatesTotal,
	)
}

// recordMetrics updates Prometheus metrics from FleetNodeSet status.
func recordMetrics(name string, total, synced, pending, failed int) {
	nodesTotal.WithLabelValues(name).Set(float64(total))
	nodesSynced.WithLabelValues(name).Set(float64(synced))
	nodesDrifted.WithLabelValues(name).Set(float64(pending))
	nodesFailed.WithLabelValues(name).Set(float64(failed))
}
