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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fleetv1alpha1 "github.com/SylphxAI/talos-fleet-controller/api/v1alpha1"
)

var _ = Describe("FleetNodeSet Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}
		fleetnodeset := &fleetv1alpha1.FleetNodeSet{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind FleetNodeSet")
			err := k8sClient.Get(ctx, typeNamespacedName, fleetnodeset)
			if err != nil && errors.IsNotFound(err) {
				resource := &fleetv1alpha1.FleetNodeSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: fleetv1alpha1.FleetNodeSetSpec{
						NodeSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"test-label": "true",
							},
						},
						Talos: fleetv1alpha1.TalosSpec{
							Version:   "v1.12.4",
							Schematic: "test-schematic",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &fleetv1alpha1.FleetNodeSet{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance FleetNodeSet")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &FleetNodeSetReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				// TalosClient is nil — reconciler handles this gracefully
				// (no nodes match the test selector, so Talos API is never called)
			}

			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// No matching nodes → should requeue after standard interval.
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		})
	})
})
