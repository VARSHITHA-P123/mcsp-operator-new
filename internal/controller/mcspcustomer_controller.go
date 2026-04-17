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
package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	mcspv1 "github.com/VARSHITHA-P123/mcsp-operator-new/api/v1"
)

const (
	// customerFinalizerName is the finalizer added to every MCSPCustomer CR
	// It blocks deletion until cleanup is complete
	customerFinalizerName = "mcsp.mcsp.io/finalizer"

	// Namespace where RHACM policies and jobs are created
	mcspPlatformNamespace = "mcsp-platform"

	// Git repo details for deployment
	gitRepoURL = "https://github.ibm.com/Manzanita/zps-mcsp-deploy.git"
	gitBranch  = "mcsp-demo"

	// Deployer image used by the Job pod
	deployerImage = "image-registry.openshift-image-registry.svc:5000/mcsp-platform/mcsp-customer-deployer:v1"
	// Secret name containing the IBM GitHub token
	gitTokenSecretName = "ibm-github-token"
	gitTokenSecretKey  = "token"

	// Requeue intervals
	namespaceWaitInterval = 5 * time.Second
)

// MCSPCustomerReconciler reconciles a MCSPCustomer object
type MCSPCustomerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=mcsp.mcsp.io,resources=mcspcustomers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mcsp.mcsp.io,resources=mcspcustomers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mcsp.mcsp.io,resources=mcspcustomers/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy.open-cluster-management.io,resources=policies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy.open-cluster-management.io,resources=placementbindings,verbs=get;list;watch;create;update;patch;delete

func (r *MCSPCustomerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// ── Step 1: Fetch the MCSPCustomer CR ────────────────────────────────────
	mcspCustomer := &mcspv1.MCSPCustomer{}
	if err := r.Get(ctx, req.NamespacedName, mcspCustomer); err != nil {
		if errors.IsNotFound(err) {
			// CR deleted before we could process it — nothing to do
			log.Info("MCSPCustomer not found, might have been deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	customerName := mcspCustomer.Spec.CustomerName
	log.Info("Reconciling MCSPCustomer", "customerName", customerName)

	// ── Deletion handling ─────────────────────────────────────────────────────
	if !mcspCustomer.DeletionTimestamp.IsZero() {
		// CR is being deleted — run cleanup
		if controllerutil.ContainsFinalizer(mcspCustomer, customerFinalizerName) {
			log.Info("MCSPCustomer is being deleted, starting cleanup", "customerName", customerName)

			if err := r.cleanupCustomerResources(ctx, customerName, log); err != nil {
				log.Error(err, "Failed to cleanup customer resources")
				return ctrl.Result{RequeueAfter: 10 * time.Second}, err
			}

			// Cleanup done — remove finalizer so Kubernetes can delete the CR
			controllerutil.RemoveFinalizer(mcspCustomer, customerFinalizerName)
			if err := r.Update(ctx, mcspCustomer); err != nil {
				log.Error(err, "Failed to remove finalizer")
				return ctrl.Result{}, err
			}

			log.Info("MCSPCustomer deletion completed", "customerName", customerName)
		}
		return ctrl.Result{}, nil
	}

	// ── Step 2: Add finalizer if not present ─────────────────────────────────
	// Finalizer blocks Kubernetes from deleting the CR until cleanup is done
	if !controllerutil.ContainsFinalizer(mcspCustomer, customerFinalizerName) {
		log.Info("Adding finalizer to MCSPCustomer", "customerName", customerName)
		controllerutil.AddFinalizer(mcspCustomer, customerFinalizerName)
		if err := r.Update(ctx, mcspCustomer); err != nil {
			log.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
		// controller-runtime will requeue automatically after Update
		return ctrl.Result{}, nil
	}

	// ── Step 3: Create RHACM Policy ──────────────────────────────────────────
	// This tells RHACM to create namespace, resourcequota, limitrange
	// and rolebinding on the managed cluster
	rhacmPolicy := &unstructured.Unstructured{}
	rhacmPolicy.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "policy.open-cluster-management.io",
		Version: "v1",
		Kind:    "Policy",
	})
	err := r.Get(ctx, types.NamespacedName{
		Name:      customerName + "-policy",
		Namespace: mcspPlatformNamespace,
	}, rhacmPolicy)
	if err != nil && errors.IsNotFound(err) {
		if createErr := r.Create(ctx, buildRHACMPolicy(customerName)); createErr != nil {
			log.Error(createErr, "Failed to create RHACM Policy")
			return ctrl.Result{}, createErr
		}
		log.Info("RHACM Policy created", "customerName", customerName)
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get RHACM Policy: %w", err)
	}

	// ── Step 4: Create PlacementBinding ──────────────────────────────────────
	// Binds the policy to the placement so RHACM knows which cluster to enforce on
	placementBinding := &unstructured.Unstructured{}
	placementBinding.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "policy.open-cluster-management.io",
		Version: "v1",
		Kind:    "PlacementBinding",
	})
	err = r.Get(ctx, types.NamespacedName{
		Name:      customerName + "-policy-binding",
		Namespace: mcspPlatformNamespace,
	}, placementBinding)
	if err != nil && errors.IsNotFound(err) {
		if createErr := r.Create(ctx, buildPlacementBinding(customerName)); createErr != nil {
			log.Error(createErr, "Failed to create PlacementBinding")
			return ctrl.Result{}, createErr
		}
		log.Info("PlacementBinding created", "customerName", customerName)
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get PlacementBinding: %w", err)
	}

	// ── Step 5: Wait for namespace to be ready ────────────────────────────────
	// RHACM takes a few seconds to create the namespace on the managed cluster
	// We poll every 5 seconds until it appears
	namespace := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: customerName}, namespace); err != nil {
		if errors.IsNotFound(err) {
			log.Info("Waiting for namespace to be created by RHACM", "customerName", customerName)
			r.updateStatus(ctx, mcspCustomer, false, "Waiting for RHACM to provision namespace", "")
			return ctrl.Result{RequeueAfter: namespaceWaitInterval}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get Namespace: %w", err)
	}
	log.Info("Namespace is ready", "namespace", customerName)

	// ── Step 6: Create Deployment Job ────────────────────────────────────────
	// Job clones the git repo and runs deploy.sh to deploy all microservices
	// into the customer namespace
	job := &batchv1.Job{}
	err = r.Get(ctx, types.NamespacedName{
		Name:      customerName + "-deploy-job",
		Namespace: mcspPlatformNamespace,
	}, job)
	if err != nil && errors.IsNotFound(err) {
		log.Info("Creating deployment job", "customerName", customerName)
		if createErr := r.Create(ctx, buildDeploymentJob(customerName)); createErr != nil {
			log.Error(createErr, "Failed to create deployment Job")
			return ctrl.Result{}, createErr
		}
		log.Info("Deployment Job created", "customerName", customerName)
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get Job: %w", err)
	}

	// ── Step 7: Update Status ─────────────────────────────────────────────────
	appURL := fmt.Sprintf("https://mcsp-app-%s.apps.zps-mcsp-cluster.cp.fyre.ibm.com", customerName)
	r.updateStatus(ctx, mcspCustomer, true,
		fmt.Sprintf("Customer %s deployment job started", customerName),
		appURL)

	log.Info("MCSPCustomer reconciled successfully", "customerName", customerName)
	return ctrl.Result{}, nil
}

// ── updateStatus ──────────────────────────────────────────────────────────────
// Updates the MCSPCustomer status subresource
// Errors are logged but not returned — status update failure should not block reconcile
func (r *MCSPCustomerReconciler) updateStatus(
	ctx context.Context,
	mcspCustomer *mcspv1.MCSPCustomer,
	deployed bool, message, url string,
) {
	mcspCustomer.Status.Deployed = deployed
	mcspCustomer.Status.Message = message
	if url != "" {
		mcspCustomer.Status.URL = url
	}
	if err := r.Status().Update(ctx, mcspCustomer); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to update MCSPCustomer status")
	}
}

// ── Cleanup ───────────────────────────────────────────────────────────────────
// Runs when MCSPCustomer CR is deleted
// Order: Job → PlacementBinding → RHACM Policy → Namespace
func (r *MCSPCustomerReconciler) cleanupCustomerResources(
	ctx context.Context, customerName string, log logr.Logger,
) error {
	log.Info("Starting cleanup for customer", "customerName", customerName)

	// Step 1: Delete Deployment Job
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      customerName + "-deploy-job",
		Namespace: mcspPlatformNamespace,
	}, job); err == nil {
		if delErr := r.Delete(ctx, job); delErr != nil && !errors.IsNotFound(delErr) {
			log.Error(delErr, "Failed to delete Job")
			return delErr
		}
		log.Info("Job deleted", "customerName", customerName)
	}

	// Step 2: Delete PlacementBinding
	placementBinding := &unstructured.Unstructured{}
	placementBinding.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "policy.open-cluster-management.io",
		Version: "v1",
		Kind:    "PlacementBinding",
	})
	if err := r.Get(ctx, types.NamespacedName{
		Name:      customerName + "-policy-binding",
		Namespace: mcspPlatformNamespace,
	}, placementBinding); err == nil {
		if delErr := r.Delete(ctx, placementBinding); delErr != nil && !errors.IsNotFound(delErr) {
			log.Error(delErr, "Failed to delete PlacementBinding")
			return delErr
		}
		log.Info("PlacementBinding deleted", "customerName", customerName)
	}

	// Step 3: Delete RHACM Policy
	// Removing the policy stops RHACM from enforcing the namespace on the managed cluster
	rhacmPolicy := &unstructured.Unstructured{}
	rhacmPolicy.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "policy.open-cluster-management.io",
		Version: "v1",
		Kind:    "Policy",
	})
	if err := r.Get(ctx, types.NamespacedName{
		Name:      customerName + "-policy",
		Namespace: mcspPlatformNamespace,
	}, rhacmPolicy); err == nil {
		if delErr := r.Delete(ctx, rhacmPolicy); delErr != nil && !errors.IsNotFound(delErr) {
			log.Error(delErr, "Failed to delete RHACM Policy")
			return delErr
		}
		log.Info("RHACM Policy deleted", "customerName", customerName)
	}

	// Step 4: Delete Namespace and wait for it to be fully gone
	namespace := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: customerName}, namespace); err == nil {
		if delErr := r.Delete(ctx, namespace); delErr != nil && !errors.IsNotFound(delErr) {
			log.Error(delErr, "Failed to delete Namespace")
			return delErr
		}
		log.Info("Namespace deletion initiated", "customerName", customerName)

		// Check if namespace is gone — if not, return error to trigger requeue
		// This avoids time.Sleep blocking the worker goroutine
		if checkErr := r.Get(ctx, types.NamespacedName{Name: customerName}, namespace); checkErr != nil {
			if !errors.IsNotFound(checkErr) {
				return checkErr
			}
		} else {
			return fmt.Errorf("namespace %s still terminating, will requeue", customerName)
		}
		log.Info("Namespace fully deleted", "customerName", customerName)
	}

	log.Info("Cleanup completed successfully", "customerName", customerName)
	return nil
}

// ── Builder helpers ───────────────────────────────────────────────────────────

// buildRHACMPolicy creates the RHACM Policy object that enforces:
// Namespace, ResourceQuota, LimitRange, RoleBinding on the managed cluster
func buildRHACMPolicy(customerName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "policy.open-cluster-management.io/v1",
			"kind":       "Policy",
			"metadata": map[string]interface{}{
				"name":      customerName + "-policy",
				"namespace": mcspPlatformNamespace,
			},
			"spec": map[string]interface{}{
				"remediationAction": "enforce",
				"disabled":          false,
				"policy-templates": []interface{}{
					map[string]interface{}{
						"objectDefinition": map[string]interface{}{
							"apiVersion": "policy.open-cluster-management.io/v1",
							"kind":       "ConfigurationPolicy",
							"metadata": map[string]interface{}{
								"name": customerName + "-config",
							},
							"spec": map[string]interface{}{
								"remediationAction": "enforce",
								"severity":          "low",
								"object-templates": []interface{}{
									// Namespace
									map[string]interface{}{
										"complianceType": "musthave",
										"objectDefinition": map[string]interface{}{
											"apiVersion": "v1",
											"kind":       "Namespace",
											"metadata": map[string]interface{}{
												"name": customerName,
												"labels": map[string]interface{}{
													"tenant":   customerName,
													"customer": "true",
												},
											},
										},
									},
									// ResourceQuota
									map[string]interface{}{
										"complianceType": "musthave",
										"objectDefinition": map[string]interface{}{
											"apiVersion": "v1",
											"kind":       "ResourceQuota",
											"metadata": map[string]interface{}{
												"name":      customerName + "-quota",
												"namespace": customerName,
											},
											"spec": map[string]interface{}{
												"hard": map[string]interface{}{
													"requests.cpu":    "4",
													"requests.memory": "8Gi",
													"limits.cpu":      "8",
													"limits.memory":   "16Gi",
													"pods":            "50",
												},
											},
										},
									},
									// LimitRange
									map[string]interface{}{
										"complianceType": "musthave",
										"objectDefinition": map[string]interface{}{
											"apiVersion": "v1",
											"kind":       "LimitRange",
											"metadata": map[string]interface{}{
												"name":      customerName + "-limits",
												"namespace": customerName,
											},
											"spec": map[string]interface{}{
												"limits": []interface{}{
													map[string]interface{}{
														"type": "Container",
														"default": map[string]interface{}{
															"cpu":    "500m",
															"memory": "512Mi",
														},
														"defaultRequest": map[string]interface{}{
															"cpu":    "100m",
															"memory": "128Mi",
														},
													},
												},
											},
										},
									},
									// RoleBinding — image pull access
									map[string]interface{}{
										"complianceType": "musthave",
										"objectDefinition": map[string]interface{}{
											"apiVersion": "rbac.authorization.k8s.io/v1",
											"kind":       "RoleBinding",
											"metadata": map[string]interface{}{
												"name":      customerName + "-image-puller",
												"namespace": mcspPlatformNamespace,
											},
											"roleRef": map[string]interface{}{
												"apiGroup": "rbac.authorization.k8s.io",
												"kind":     "ClusterRole",
												"name":     "system:image-puller",
											},
											"subjects": []interface{}{
												map[string]interface{}{
													"kind":      "ServiceAccount",
													"name":      "default",
													"namespace": customerName,
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// buildPlacementBinding binds the RHACM Policy to the placement
// so RHACM knows which cluster to enforce the policy on
func buildPlacementBinding(customerName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "policy.open-cluster-management.io/v1",
			"kind":       "PlacementBinding",
			"metadata": map[string]interface{}{
				"name":      customerName + "-policy-binding",
				"namespace": mcspPlatformNamespace,
			},
			"placementRef": map[string]interface{}{
				"name":     "mcsp-hello-world-placement",
				"apiGroup": "cluster.open-cluster-management.io",
				"kind":     "Placement",
			},
			"subjects": []interface{}{
				map[string]interface{}{
					"name":     customerName + "-policy",
					"apiGroup": "policy.open-cluster-management.io",
					"kind":     "Policy",
				},
			},
		},
	}
}

// buildDeploymentJob creates the Kubernetes Job that deploys
// all microservices into the customer namespace
func buildDeploymentJob(customerName string) *batchv1.Job {
	ttl := int32(3600)       // auto-delete job after 1 hour
	backoffLimit := int32(3) // retry up to 3 times on failure

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      customerName + "-deploy-job",
			Namespace: mcspPlatformNamespace,
			Labels: map[string]string{
				"app":    "mcsp-deployer",
				"tenant": customerName,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			BackoffLimit:            &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":    "mcsp-deployer",
						"tenant": customerName,
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "mcsp-deployer",
					RestartPolicy:      corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						{
							Name:    "deployer",
							Image:   deployerImage,
							Command: []string{"/bin/bash", "-c"},
							Args: []string{
								fmt.Sprintf(`
									git clone https://$(GIT_TOKEN)@%s -b %s /tmp/deploy &&
									cd /tmp/deploy &&
									sh deploy.sh %s
								`, gitRepoURL[8:], gitBranch, customerName),
							},
							Env: []corev1.EnvVar{
								{
									Name: "GIT_TOKEN",
									ValueFrom: &corev1.EnvVarSource{
										SecretKeyRef: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: gitTokenSecretName,
											},
											Key: gitTokenSecretKey,
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// ── Manager setup ─────────────────────────────────────────────────────────────

// SetupWithManager sets up the controller with the Manager
// MaxConcurrentReconciles: 6 allows 6 customers to onboard/offboard simultaneously
func (r *MCSPCustomerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mcspv1.MCSPCustomer{}).
		Named("mcspcustomer").
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 6,
		}).
		Complete(r)
}