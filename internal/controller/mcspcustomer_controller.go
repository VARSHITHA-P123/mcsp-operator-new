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
	"os"
	"time"

	"github.com/go-logr/logr"
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
	customerFinalizerName = "mcsp.mcsp.io/finalizer"

	// Requeue intervals
	namespaceWaitInterval  = 5 * time.Second
	argoSyncPollInterval   = 15 * time.Second
	argoDeletePollInterval = 5 * time.Second
	argoDeleteMaxRetries   = 30
)

var (
	mcspPlatformNamespace = getEnvOrDefault("MCSP_PLATFORM_NAMESPACE", "mcsp-platform")
	argoCDNamespace       = getEnvOrDefault("ARGOCD_NAMESPACE", "openshift-operators")
	gitRepoURL            = getEnvOrDefault("GIT_REPO_URL", "git@github.ibm.com:Manzanita/zps-mcsp-deploy.git")
	gitBranch             = getEnvOrDefault("GIT_BRANCH", "mcsp-demo")
	gitPath               = getEnvOrDefault("GIT_PATH", "yaml")
	clusterDomain         = getEnvOrDefault("CLUSTER_DOMAIN", "zps-mcsp-cluster.cp.fyre.ibm.com")
	placementName         = getEnvOrDefault("PLACEMENT_NAME", "mcsp-hello-world-placement")
)

// getEnvOrDefault reads an environment variable and falls back to a default value
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

var argoAppGVK = schema.GroupVersionKind{
	Group:   "argoproj.io",
	Version: "v1alpha1",
	Kind:    "Application",
}

// MCSPCustomerReconciler reconciles a MCSPCustomer object
type MCSPCustomerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=mcsp.mcsp.io,resources=mcspcustomers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mcsp.mcsp.io,resources=mcspcustomers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mcsp.mcsp.io,resources=mcspcustomers/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy.open-cluster-management.io,resources=policies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy.open-cluster-management.io,resources=placementbindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=argoproj.io,resources=applications,verbs=get;list;watch;create;update;patch;delete

func (r *MCSPCustomerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// ── Step 1: Fetch the MCSPCustomer CR ────────────────────────────────────
	mcspCustomer := &mcspv1.MCSPCustomer{}
	if err := r.Get(ctx, req.NamespacedName, mcspCustomer); err != nil {
		if errors.IsNotFound(err) {
			log.Info("MCSPCustomer not found, might have been deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	customerName := mcspCustomer.Spec.CustomerName
	log.Info("Reconciling MCSPCustomer", "customerName", customerName)

	// ── Deletion handling ─────────────────────────────────────────────────────
	if !mcspCustomer.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(mcspCustomer, customerFinalizerName) {
			log.Info("MCSPCustomer is being deleted, starting cleanup", "customerName", customerName)
			if err := r.cleanupCustomerResources(ctx, customerName, log); err != nil {
				log.Error(err, "Failed to cleanup customer resources")
				return ctrl.Result{RequeueAfter: 10 * time.Second}, err
			}
			controllerutil.RemoveFinalizer(mcspCustomer, customerFinalizerName)
			if err := r.Update(ctx, mcspCustomer); err != nil {
				log.Error(err, "Failed to remove finalizer")
				return ctrl.Result{}, err
			}
			log.Info("MCSPCustomer deletion completed", "customerName", customerName)
		}
		return ctrl.Result{}, nil
	}

	// ── Step 2: Add finalizer ─────────────────────────────────────────────────
	if !controllerutil.ContainsFinalizer(mcspCustomer, customerFinalizerName) {
		log.Info("Adding finalizer to MCSPCustomer", "customerName", customerName)
		controllerutil.AddFinalizer(mcspCustomer, customerFinalizerName)
		if err := r.Update(ctx, mcspCustomer); err != nil {
			log.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// ── Step 3: Create RHACM Policy ──────────────────────────────────────────
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

	// ── Step 5: Wait for namespace ────────────────────────────────────────────
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

	// ── Step 6: Create url secret in customer namespace ───────────────────────
	if err := r.ensureURLSecret(ctx, log, customerName); err != nil {
		log.Error(err, "Failed to ensure URL secret")
		return ctrl.Result{}, err
	}

	// ── Step 7: Create ArgoCD Application ────────────────────────────────────
	argoApp := &unstructured.Unstructured{}
	argoApp.SetGroupVersionKind(argoAppGVK)
	err = r.Get(ctx, types.NamespacedName{
		Name:      customerName + "-app",
		Namespace: argoCDNamespace,
	}, argoApp)
	if err != nil && errors.IsNotFound(err) {
		log.Info("Creating ArgoCD Application", "customerName", customerName)
		if createErr := r.Create(ctx, buildArgoApp(customerName)); createErr != nil {
			log.Error(createErr, "Failed to create ArgoCD Application")
			return ctrl.Result{}, createErr
		}
		log.Info("ArgoCD Application created", "customerName", customerName)
		r.updateStatus(ctx, mcspCustomer, false, "ArgoCD syncing microservices...", "")
		return ctrl.Result{RequeueAfter: argoSyncPollInterval}, nil
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get ArgoCD Application: %w", err)
	}

	// ── Step 8: Poll ArgoCD sync + health ─────────────────────────────────────
	syncStatus, healthStatus := extractArgoStatus(argoApp)
	log.Info("ArgoCD status", "sync", syncStatus, "health", healthStatus, "customer", customerName)

	if syncStatus != "Synced" || healthStatus != "Healthy" {
		r.updateStatus(ctx, mcspCustomer, false,
			fmt.Sprintf("ArgoCD Sync: %s | Health: %s", syncStatus, healthStatus), "")
		log.Info("ArgoCD not yet Synced+Healthy, requeuing", "customerName", customerName)
		return ctrl.Result{RequeueAfter: argoSyncPollInterval}, nil
	}

	// ── Step 9: Update status → fully deployed ────────────────────────────────
	appURL := fmt.Sprintf("https://manzanita-%s.apps.%s", customerName, clusterDomain)
	r.updateStatus(ctx, mcspCustomer, true,
		fmt.Sprintf("ArgoCD Sync: %s | Health: %s", syncStatus, healthStatus),
		appURL)

	log.Info("MCSPCustomer reconciled successfully", "customerName", customerName)
	return ctrl.Result{}, nil
}

// ── ensureURLSecret ───────────────────────────────────────────────────────────
func (r *MCSPCustomerReconciler) ensureURLSecret(ctx context.Context, log logr.Logger, customerName string) error {
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      "url",
		Namespace: customerName,
	}, existing)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to get url secret: %w", err)
	}

	urlSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "url",
			Namespace: customerName,
			Labels: map[string]string{
				"tenant": customerName,
			},
		},
		StringData: map[string]string{
			"proxy-url": fmt.Sprintf("https://manzanita-%s.apps.%s", customerName, clusterDomain),
			"api-url":   fmt.Sprintf("https://manzanita-%s.apps.%s/api", customerName, clusterDomain),
			"ws-url":    fmt.Sprintf("wss://manzanita-%s.apps.%s/ws", customerName, clusterDomain),
			"redis-url": fmt.Sprintf("https://redis-%s.apps.%s", customerName, clusterDomain),
		},
	}

	if err := r.Create(ctx, urlSecret); err != nil {
		return fmt.Errorf("failed to create url secret: %w", err)
	}

	log.Info("URL secret created", "customerName", customerName)
	return nil
}

// ── updateStatus ──────────────────────────────────────────────────────────────
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

// ── extractArgoStatus ─────────────────────────────────────────────────────────
func extractArgoStatus(app *unstructured.Unstructured) (syncStatus, healthStatus string) {
	syncStatus, healthStatus = "Unknown", "Unknown"
	status, found, _ := unstructured.NestedMap(app.Object, "status")
	if !found {
		return
	}
	if s, _, err := unstructured.NestedString(status, "sync", "status"); err == nil && s != "" {
		syncStatus = s
	}
	if h, _, err := unstructured.NestedString(status, "health", "status"); err == nil && h != "" {
		healthStatus = h
	}
	return
}

// ── Cleanup ───────────────────────────────────────────────────────────────────
func (r *MCSPCustomerReconciler) cleanupCustomerResources(
	ctx context.Context, customerName string, log logr.Logger,
) error {
	log.Info("Starting cleanup for customer", "customerName", customerName)

	// Step 1: Delete ArgoCD Application
	argoApp := &unstructured.Unstructured{}
	argoApp.SetGroupVersionKind(argoAppGVK)
	if err := r.Get(ctx, types.NamespacedName{
		Name:      customerName + "-app",
		Namespace: argoCDNamespace,
	}, argoApp); err == nil {
		log.Info("Deleting ArgoCD Application", "customerName", customerName)
		if delErr := r.Delete(ctx, argoApp); delErr != nil && !errors.IsNotFound(delErr) {
			log.Error(delErr, "Failed to delete ArgoCD Application")
			return delErr
		}
		for i := 0; i < argoDeleteMaxRetries; i++ {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(argoAppGVK)
			if chkErr := r.Get(ctx, types.NamespacedName{
				Name:      customerName + "-app",
				Namespace: argoCDNamespace,
			}, check); errors.IsNotFound(chkErr) {
				log.Info("ArgoCD Application fully deleted", "customerName", customerName)
				break
			}
			if i == argoDeleteMaxRetries-1 {
				return fmt.Errorf("timed out waiting for ArgoCD Application %s-app to be deleted", customerName)
			}
			log.Info("Waiting for ArgoCD cascade delete...", "attempt", i+1)
			time.Sleep(argoDeletePollInterval)
		}
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to get ArgoCD Application: %w", err)
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

	// Step 4: Delete Namespace
	namespace := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: customerName}, namespace); err == nil {
		if delErr := r.Delete(ctx, namespace); delErr != nil && !errors.IsNotFound(delErr) {
			log.Error(delErr, "Failed to delete Namespace")
			return delErr
		}
		log.Info("Namespace deletion initiated", "customerName", customerName)
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
func buildArgoApp(customerName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "argoproj.io/v1alpha1",
			"kind":       "Application",
			"metadata": map[string]interface{}{
				"name":      customerName + "-app",
				"namespace": argoCDNamespace,
				"labels": map[string]interface{}{
					"tenant": customerName,
				},
				"finalizers": []interface{}{
					"resources-finalizer.argocd.argoproj.io",
				},
			},
			"spec": map[string]interface{}{
				"project": "default",
				"source": map[string]interface{}{
					"repoURL":        gitRepoURL,
					"targetRevision": gitBranch,
					"path":           gitPath,
					"directory": map[string]interface{}{
						"recurse": true,
					},
				},
				"destination": map[string]interface{}{
					"server":    "https://kubernetes.default.svc",
					"namespace": customerName,
				},
				"syncPolicy": map[string]interface{}{
					"automated": map[string]interface{}{
						"prune":    true,
						"selfHeal": true,
					},
					"syncOptions": []interface{}{
						"CreateNamespace=false",
						"ServerSideApply=true",
					},
				},
			},
		},
	}
}

// buildRHACMPolicy creates the RHACM Policy
// The namespace label argocd.argoproj.io/managed-by is critical
// It tells ArgoCD this namespace is managed by it so it can deploy into it
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
									map[string]interface{}{
										"complianceType": "musthave",
										"objectDefinition": map[string]interface{}{
											"apiVersion": "v1",
											"kind":       "Namespace",
											"metadata": map[string]interface{}{
												"name": customerName,
												"labels": map[string]interface{}{
													"tenant":                        customerName,
													"customer":                      "true",
													"argocd.argoproj.io/managed-by": argoCDNamespace,
												},
											},
										},
									},
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
				"name":     placementName,
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

// ── Manager setup ─────────────────────────────────────────────────────────────
func (r *MCSPCustomerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mcspv1.MCSPCustomer{}).
		Named("mcspcustomer").
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 6,
		}).
		Complete(r)
}
