//go:build e2e
// +build e2e

/*
Copyright 2026.

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

package e2e

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	littleredv1alpha1 "github.com/littlered-operator/littlered-operator/api/v1alpha1"
	clik8s "github.com/littlered-operator/littlered-operator/internal/cli/k8s"
)

var _ = Describe("LittleRed PodDisruptionBudget", Label("pdb"), func() {
	var k8sClient client.Client
	ctx := context.Background()

	BeforeEach(func() {
		var err error
		k8sClient, _, _, _, err = clik8s.NewClient("")
		Expect(err).NotTo(HaveOccurred())
	})

	// getPDB fetches a PDB by name, returning (pdb, found, err).
	// A not-found response is returned as (nil, false, nil).
	getPDB := func(name string) (*policyv1.PodDisruptionBudget, bool, error) {
		pdb := &policyv1.PodDisruptionBudget{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, pdb)
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		return pdb, true, nil
	}

	// waitForRunning waits for a LittleRed CR to reach PhaseRunning.
	waitForRunning := func(name string, timeout time.Duration) {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			lr := &littleredv1alpha1.LittleRed{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, lr)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(lr.Status.Phase).To(Equal(littleredv1alpha1.PhaseRunning))
		}, timeout, 5*time.Second).Should(Succeed())
	}

	// newStandaloneCR returns a minimal standalone LittleRed CR.
	newStandaloneCR := func(name string) *littleredv1alpha1.LittleRed {
		return &littleredv1alpha1.LittleRed{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec:       littleredv1alpha1.LittleRedSpec{Mode: "standalone"},
		}
	}

	// newSentinelCR returns a minimal sentinel LittleRed CR.
	newSentinelCR := func(name string) *littleredv1alpha1.LittleRed {
		return &littleredv1alpha1.LittleRed{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec: littleredv1alpha1.LittleRedSpec{
				Mode: "sentinel",
				Sentinel: &littleredv1alpha1.SentinelSpec{
					Quorum:                2,
					DownAfterMilliseconds: 5000,
					FailoverTimeout:       10000,
				},
			},
		}
	}

	// newClusterCR returns a minimal cluster LittleRed CR.
	newClusterCR := func(name string) *littleredv1alpha1.LittleRed {
		replicas := 1
		shards := 3
		return &littleredv1alpha1.LittleRed{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec: littleredv1alpha1.LittleRedSpec{
				Mode: "cluster",
				Cluster: &littleredv1alpha1.ClusterSpec{
					Shards:           shards,
					ReplicasPerShard: &replicas,
				},
			},
		}
	}

	// ============================================================================
	// PDB disabled by default
	// ============================================================================

	Context("PDB disabled by default", func() {
		type modeCase struct {
			mode     string
			pdbNames []string
			timeout  time.Duration
		}
		cases := []modeCase{
			{
				mode:     "standalone",
				pdbNames: []string{"pdb-off-standalone-redis-pdb"},
				timeout:  2 * time.Minute,
			},
			{
				mode:     "sentinel",
				pdbNames: []string{"pdb-off-sentinel-redis-pdb", "pdb-off-sentinel-sentinel-pdb"},
				timeout:  3 * time.Minute,
			},
			{
				mode:     "cluster",
				pdbNames: []string{"pdb-off-cluster-cluster-pdb"},
				timeout:  5 * time.Minute,
			},
		}

		for _, tc := range cases {
			tc := tc
			crName := fmt.Sprintf("pdb-off-%s", tc.mode)

			It(fmt.Sprintf("should not create any PDB in %s mode when create is not set", tc.mode), func() {
				AddReportEntry("cr:" + crName)

				var cr *littleredv1alpha1.LittleRed
				switch tc.mode {
				case "standalone":
					cr = newStandaloneCR(crName)
				case "sentinel":
					cr = newSentinelCR(crName)
				case "cluster":
					cr = newClusterCR(crName)
				}
				// Do not set PodDisruptionBudget — create defaults to false.
				Expect(k8sClient.Create(ctx, cr)).To(Succeed())
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })

				By("waiting for CR to be Running to ensure at least one full reconciliation")
				waitForRunning(crName, tc.timeout)

				By("verifying no PDB exists for any expected name")
				for _, pdbName := range tc.pdbNames {
					_, found, err := getPDB(pdbName)
					Expect(err).NotTo(HaveOccurred())
					Expect(found).To(BeFalse(), "PDB %q should not exist when create=false", pdbName)
				}
			})
		}
	})

	// ============================================================================
	// PDB enabled with default values
	// ============================================================================

	Context("PDB enabled with default values", func() {
		type modeCase struct {
			mode     string
			pdbNames []string
			timeout  time.Duration
		}
		cases := []modeCase{
			{
				mode:     "standalone",
				pdbNames: []string{"pdb-on-standalone-redis-pdb"},
				timeout:  2 * time.Minute,
			},
			{
				mode:     "sentinel",
				pdbNames: []string{"pdb-on-sentinel-redis-pdb", "pdb-on-sentinel-sentinel-pdb"},
				timeout:  3 * time.Minute,
			},
			{
				mode:     "cluster",
				pdbNames: []string{"pdb-on-cluster-cluster-pdb"},
				timeout:  5 * time.Minute,
			},
		}

		for _, tc := range cases {
			tc := tc
			crName := fmt.Sprintf("pdb-on-%s", tc.mode)

			It(fmt.Sprintf("should create PDB(s) with maxUnavailable=1 in %s mode", tc.mode), func() {
				AddReportEntry("cr:" + crName)

				var cr *littleredv1alpha1.LittleRed
				switch tc.mode {
				case "standalone":
					cr = newStandaloneCR(crName)
				case "sentinel":
					cr = newSentinelCR(crName)
				case "cluster":
					cr = newClusterCR(crName)
				}
				cr.Spec.PodDisruptionBudget = littleredv1alpha1.PodDisruptionBudgetSpec{
					Create: true,
				}
				Expect(k8sClient.Create(ctx, cr)).To(Succeed())
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })

				By("waiting for CR to be Running")
				waitForRunning(crName, tc.timeout)

				By("verifying each PDB exists with the correct default spec")
				for _, pdbName := range tc.pdbNames {
					Eventually(func(g Gomega) {
						pdb, found, err := getPDB(pdbName)
						g.Expect(err).NotTo(HaveOccurred())
						g.Expect(found).To(BeTrue(), "PDB %q should exist", pdbName)

						g.Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil(),
							"PDB %q should have MaxUnavailable set by default", pdbName)
						g.Expect(*pdb.Spec.MaxUnavailable).To(Equal(intstr.FromInt32(1)),
							"PDB %q MaxUnavailable should default to 1", pdbName)
						g.Expect(pdb.Spec.MinAvailable).To(BeNil(),
							"PDB %q MinAvailable should not be set by default", pdbName)
					}, 30*time.Second, 5*time.Second).Should(Succeed())
				}
			})
		}
	})

	// ============================================================================
	// Custom PDB values
	// ============================================================================

	Context("Custom PDB values", func() {
		It("should apply custom maxUnavailable", func() {
			crName := "pdb-custom-max"
			pdbName := crName + "-redis-pdb"
			maxUnavailable := intstr.FromInt32(2)

			cr := newStandaloneCR(crName)
			cr.Spec.PodDisruptionBudget = littleredv1alpha1.PodDisruptionBudgetSpec{
				Create:         true,
				MaxUnavailable: &maxUnavailable,
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })

			By("waiting for CR to be Running")
			waitForRunning(crName, 2*time.Minute)

			By("verifying PDB has maxUnavailable=2")
			Eventually(func(g Gomega) {
				pdb, found, err := getPDB(pdbName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(found).To(BeTrue())
				g.Expect(*pdb.Spec.MaxUnavailable).To(Equal(intstr.FromInt32(2)))
				g.Expect(pdb.Spec.MinAvailable).To(BeNil())
			}, 30*time.Second, 5*time.Second).Should(Succeed())
		})

		It("should apply custom minAvailable", func() {
			crName := "pdb-custom-min"
			pdbName := crName + "-redis-pdb"
			minAvailable := intstr.FromInt32(1)

			cr := newStandaloneCR(crName)
			cr.Spec.PodDisruptionBudget = littleredv1alpha1.PodDisruptionBudgetSpec{
				Create:       true,
				MinAvailable: &minAvailable,
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })

			By("waiting for CR to be Running")
			waitForRunning(crName, 2*time.Minute)

			By("verifying PDB has minAvailable=1 and no maxUnavailable")
			Eventually(func(g Gomega) {
				pdb, found, err := getPDB(pdbName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(found).To(BeTrue())
				g.Expect(*pdb.Spec.MinAvailable).To(Equal(intstr.FromInt32(1)))
				g.Expect(pdb.Spec.MaxUnavailable).To(BeNil())
			}, 30*time.Second, 5*time.Second).Should(Succeed())
		})

		It("should prefer minAvailable over maxUnavailable when both are set", func() {
			crName := "pdb-custom-both"
			pdbName := crName + "-redis-pdb"
			minAvailable := intstr.FromInt32(2)
			maxUnavailable := intstr.FromInt32(1)

			cr := newStandaloneCR(crName)
			cr.Spec.PodDisruptionBudget = littleredv1alpha1.PodDisruptionBudgetSpec{
				Create:         true,
				MinAvailable:   &minAvailable,
				MaxUnavailable: &maxUnavailable,
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })

			By("waiting for CR to be Running")
			waitForRunning(crName, 2*time.Minute)

			By("verifying PDB uses minAvailable=2 and ignores maxUnavailable")
			Eventually(func(g Gomega) {
				pdb, found, err := getPDB(pdbName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(found).To(BeTrue())
				g.Expect(*pdb.Spec.MinAvailable).To(Equal(intstr.FromInt32(2)))
				g.Expect(pdb.Spec.MaxUnavailable).To(BeNil())
			}, 30*time.Second, 5*time.Second).Should(Succeed())
		})

		It("should support percentage value for maxUnavailable", func() {
			crName := "pdb-custom-pct"
			pdbName := crName + "-redis-pdb"
			maxUnavailable := intstr.FromString("50%")

			cr := newStandaloneCR(crName)
			cr.Spec.PodDisruptionBudget = littleredv1alpha1.PodDisruptionBudgetSpec{
				Create:         true,
				MaxUnavailable: &maxUnavailable,
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })

			By("waiting for CR to be Running")
			waitForRunning(crName, 2*time.Minute)

			By("verifying PDB has maxUnavailable=50%")
			Eventually(func(g Gomega) {
				pdb, found, err := getPDB(pdbName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(found).To(BeTrue())
				g.Expect(*pdb.Spec.MaxUnavailable).To(Equal(intstr.FromString("50%")))
			}, 30*time.Second, 5*time.Second).Should(Succeed())
		})
	})

	// ============================================================================
	// PDB lifecycle
	// ============================================================================

	Context("PDB lifecycle", func() {
		It("should delete PDB when create is toggled from true to false", func() {
			crName := "pdb-toggle-off"
			pdbName := crName + "-redis-pdb"

			cr := newStandaloneCR(crName)
			cr.Spec.PodDisruptionBudget = littleredv1alpha1.PodDisruptionBudgetSpec{Create: true}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })

			By("waiting for CR to be Running")
			waitForRunning(crName, 2*time.Minute)

			By("waiting for PDB to exist")
			Eventually(func(g Gomega) {
				_, found, err := getPDB(pdbName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(found).To(BeTrue())
			}, 30*time.Second, 5*time.Second).Should(Succeed())

			By("patching CR to disable PDB creation")
			// Retry on conflict — the CR may have been updated by the controller.
			Eventually(func(g Gomega) {
				current := &littleredv1alpha1.LittleRed{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: crName, Namespace: testNamespace}, current)
				g.Expect(err).NotTo(HaveOccurred())
				current.Spec.PodDisruptionBudget.Create = false
				g.Expect(k8sClient.Update(ctx, current)).To(Succeed())
			}, 30*time.Second, 5*time.Second).Should(Succeed())

			By("verifying PDB is deleted by the operator")
			Eventually(func(g Gomega) {
				_, found, err := getPDB(pdbName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(found).To(BeFalse())
			}, 30*time.Second, 5*time.Second).Should(Succeed())
		})

		It("should recreate PDB after manual deletion", func() {
			crName := "pdb-self-heal"
			pdbName := crName + "-redis-pdb"

			cr := newStandaloneCR(crName)
			cr.Spec.PodDisruptionBudget = littleredv1alpha1.PodDisruptionBudgetSpec{Create: true}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })

			By("waiting for CR to be Running")
			waitForRunning(crName, 2*time.Minute)

			By("waiting for PDB to exist")
			Eventually(func(g Gomega) {
				_, found, err := getPDB(pdbName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(found).To(BeTrue())
			}, 30*time.Second, 5*time.Second).Should(Succeed())

			By("deleting the PDB manually")
			pdb, found, err := getPDB(pdbName)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			Expect(k8sClient.Delete(ctx, pdb)).To(Succeed())

			By("verifying the operator recreates the PDB")
			Eventually(func(g Gomega) {
				_, found, err := getPDB(pdbName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(found).To(BeTrue())
			}, 30*time.Second, 5*time.Second).Should(Succeed())
		})

		It("should garbage-collect all PDBs when the LittleRed CR is deleted in sentinel mode", func() {
			crName := "pdb-gc-sentinel"
			pdbNames := []string{
				crName + "-redis-pdb",
				crName + "-sentinel-pdb",
			}

			cr := newSentinelCR(crName)
			cr.Spec.PodDisruptionBudget = littleredv1alpha1.PodDisruptionBudgetSpec{Create: true}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())
			// DeferCleanup as a safety net in case the test fails before the explicit delete.
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, cr) })

			By("waiting for CR to be Running")
			waitForRunning(crName, 3*time.Minute)

			By("verifying both PDBs exist")
			for _, pdbName := range pdbNames {
				Eventually(func(g Gomega) {
					_, found, err := getPDB(pdbName)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(found).To(BeTrue(), "PDB %q should exist", pdbName)
				}, 30*time.Second, 5*time.Second).Should(Succeed())
			}

			By("deleting the LittleRed CR")
			Expect(k8sClient.Delete(ctx, cr)).To(Succeed())

			By("verifying both PDBs are garbage-collected")
			for _, pdbName := range pdbNames {
				Eventually(func(g Gomega) {
					_, found, err := getPDB(pdbName)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(found).To(BeFalse(), "PDB %q should be garbage-collected", pdbName)
				}, 1*time.Minute, 5*time.Second).Should(Succeed())
			}
		})
	})
})
