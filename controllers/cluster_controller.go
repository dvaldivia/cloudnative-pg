/*
This file is part of Cloud Native PostgreSQL.

Copyright (C) 2019-2020 2ndQuadrant Italia SRL. Exclusively licensed to 2ndQuadrant Limited.
*/

// Package controllers contains the controller of the CRD
package controllers

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"gitlab.2ndquadrant.com/k8s/cloud-native-postgresql/api/v1alpha1"
	"gitlab.2ndquadrant.com/k8s/cloud-native-postgresql/pkg/expectations"
	"gitlab.2ndquadrant.com/k8s/cloud-native-postgresql/pkg/postgres"
)

const (
	podOwnerKey = ".metadata.controller"
	pvcOwnerKey = ".metadata.controller"
	jobOwnerKey = ".metadata.controller"
)

var (
	apiGVString = v1alpha1.GroupVersion.String()
)

// ClusterReconciler reconciles a Cluster objects
type ClusterReconciler struct {
	client.Client
	Log             logr.Logger
	Scheme          *runtime.Scheme
	podExpectations *expectations.ControllerExpectations
	jobExpectations *expectations.ControllerExpectations
	pvcExpectations *expectations.ControllerExpectations
}

// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations,verbs=get;update;list
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingwebhookconfigurations,verbs=get;update;list
// +kubebuilder:rbac:groups=postgresql.k8s.enterprisedb.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.k8s.enterprisedb.io,resources=clusters/status,verbs=get;watch;update;patch
// +kubebuilder:rbac:groups=postgresql.k8s.enterprisedb.io,resources=clusters/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=create;list;get;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;create;watch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;delete;patch;create;watch
// +kubebuilder:rbac:groups="batch",resources=jobs,verbs=get;list;delete;patch;create;watch
// +kubebuilder:rbac:groups="",resources=pods/status,verbs=get
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=get;list;delete;patch;create;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;create;delete;update;patch;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=create;delete;get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=create;patch;update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=create;patch;update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=create;patch;update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;watch;delete;patch
// +kubebuilder:rbac:groups="",resources=configmaps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create

// Reconcile is the operator reconciler loop
func (r *ClusterReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("namespace", req.Namespace, "name", req.Name)

	var cluster v1alpha1.Cluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		// This also happens when you delete a Cluster resource in k8s. If
		// that's the case, let's just wait for the Kubernetes garbage collector
		// to remove all the Pods of the cluster.
		if apierrs.IsNotFound(err) {
			log.Info("Resource has been deleted")

			return ctrl.Result{}, nil
		}

		// This is a real error, maybe the RBAC configuration is wrong?
		return ctrl.Result{}, fmt.Errorf("cannot get the managed resource: %w", err)
	}

	var namespace corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Namespace: "", Name: req.Namespace}, &namespace); err != nil {
		return ctrl.Result{}, fmt.Errorf("cannot get the containing namespace: %w", err)
	}

	if !namespace.DeletionTimestamp.IsZero() {
		// This happens when you delete a namespace containing a Cluster resource. If that's the case,
		// let's just wait for the Kubernetes to remove all object in the namespace.
		return ctrl.Result{}, nil
	}

	// Update the status of this resource
	resources, err := r.getManagedResources(ctx, cluster)
	if err != nil {
		log.Error(err, "Cannot extract the list of managed resources")
		return ctrl.Result{}, err
	}

	if cluster.Status.CurrentPrimary != "" && cluster.Status.CurrentPrimary != cluster.Status.TargetPrimary {
		log.Info("There is a switchover or a failover "+
			"in progress, waiting for the operation to complete",
			"currentPrimary", cluster.Status.CurrentPrimary,
			"targetPrimary", cluster.Status.TargetPrimary)
		return ctrl.Result{}, err
	}

	// Update the status section of this Cluster resource
	if err = r.updateResourceStatus(ctx, &cluster, resources); err != nil {
		if apierrs.IsConflict(err) {
			// Let's wait for another reconciler loop, since the
			// status already changed
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("cannot update the resource status: %w", err)
	}

	// If we are joining a node, we should wait for the process to finish
	if resources.countRunningJobs() > 0 {
		log.V(2).Info("Waiting for jobs to finish",
			"clusterName", cluster.Name,
			"namespace", cluster.Namespace,
			"jobs", resources.jobs.Items)
		return ctrl.Result{}, nil
	}

	// Ensure we have the required global objects
	if err := r.createPostgresClusterObjects(ctx, &cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("cannot create Cluster auxiliary objects: %w", err)
	}

	// Get the replication status
	var instancesStatus postgres.PostgresqlStatusList
	if instancesStatus, err = r.getStatusFromInstances(ctx, resources.pods); err != nil {
		return ctrl.Result{}, fmt.Errorf("cannot get status from instances: %w", err)
	}

	// Update the target primary name from the Pods status.
	// This means issuing a failover or switchover when needed.
	if err = r.updateTargetPrimaryFromPods(ctx, &cluster, instancesStatus); err != nil {
		return ctrl.Result{}, fmt.Errorf("cannot update target primary from pods: %w", err)
	}

	// Update the labels for the -rw service to work correctly
	if err = r.updateLabelsOnPods(ctx, &cluster, resources.pods); err != nil {
		return ctrl.Result{}, fmt.Errorf("cannot update labels on pods: %w", err)
	}

	// Act on Pods only if there is nothing that is currently being created or deleted
	if r.SatisfiedExpectations(&cluster) {
		return r.ReconcilePods(ctx, req, &cluster, resources.pods, instancesStatus)
	}

	log.V(2).Info("A managed resource is currently being created or deleted. Waiting")
	return ctrl.Result{}, nil
}

// SatisfiedExpectations check if the expectations for a certain cluster are met
func (r *ClusterReconciler) SatisfiedExpectations(cluster *v1alpha1.Cluster) bool {
	key := expectations.KeyFunc(cluster)
	if !r.podExpectations.SatisfiedExpectations(key) {
		return false
	}
	if !r.jobExpectations.SatisfiedExpectations(key) {
		return false
	}
	if !r.pvcExpectations.SatisfiedExpectations(key) {
		return false
	}

	return true
}

// ReconcilePods decides when to create, scale up/down or wait for pods
func (r *ClusterReconciler) ReconcilePods(ctx context.Context, req ctrl.Request, cluster *v1alpha1.Cluster,
	childPods corev1.PodList, instancesStatus postgres.PostgresqlStatusList) (ctrl.Result, error) {
	log := r.Log.WithValues("namespace", req.Namespace, "name", req.Name)

	// Recreate missing Pods
	if len(cluster.Status.DanglingPVC) > 0 {
		if !cluster.IsNodeMaintenanceWindowInProgress() && cluster.Status.ReadyInstances != cluster.Status.Instances {
			// A pod is not ready, let's retry
			log.V(2).Info("Waiting for node to be ready before attaching PVCs")
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, r.handleDanglingPVC(ctx, cluster)
	}

	// We have these cases now:
	//
	// 1 - There is no existent Pod for this PostgreSQL cluster ==> we need to create the
	// first node from which we will join the others
	//
	// 2 - There is one Pod, and that one is still not ready ==> we need to wait
	// for the first node to be ready
	//
	// 3 - We have already some Pods, all they all ready ==> we can create the other
	// pods joining the node that we already have.
	if cluster.Status.Instances == 0 {
		return r.createPrimaryInstance(ctx, cluster)
	}

	// When everything is reconciled, update the status
	if cluster.Status.ReadyInstances == cluster.Status.Instances &&
		cluster.Status.Instances == cluster.Spec.Instances {
		if err := r.RegisterPhase(ctx, cluster, v1alpha1.PhaseHealthy, ""); err != nil {
			return ctrl.Result{}, err
		}

		// Check if we need to handle a rolling upgrade
		return ctrl.Result{}, r.upgradeCluster(ctx, cluster, childPods, instancesStatus)
	}

	// Find if we have Pods that are not ready, this is OK
	// only if we are in upgrade mode and we have choose to just
	// wait for the node to come up
	if !cluster.IsNodeMaintenanceWindowReusePVC() && cluster.Status.ReadyInstances < cluster.Status.Instances {
		// A Pod is not ready, let's retry
		log.V(2).Info("Waiting for Pod to be ready")
		return ctrl.Result{}, nil
	}

	// Are there missing nodes? Let's create one
	if cluster.Status.Instances < cluster.Spec.Instances &&
		cluster.Status.ReadyInstances == cluster.Status.Instances {
		newNodeSerial, err := r.generateNodeSerial(ctx, cluster)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("cannot generate node serial: %w", err)
		}
		return r.joinReplicaInstance(ctx, newNodeSerial, cluster)
	}

	// Are there nodes to be removed? Remove one of them
	if cluster.Status.Instances > cluster.Spec.Instances {
		if err := r.scaleDownCluster(ctx, cluster, childPods); err != nil {
			return ctrl.Result{}, fmt.Errorf("cannot scale down cluster: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager creates a ClusterReconciler
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Initialize expectations
	r.podExpectations = expectations.NewControllerExpectations()
	r.jobExpectations = expectations.NewControllerExpectations()
	r.pvcExpectations = expectations.NewControllerExpectations()

	// Create a new indexed field on Pods. This field will be used to easily
	// find all the Pods created by this controller
	if err := mgr.GetFieldIndexer().IndexField(
		&corev1.Pod{},
		podOwnerKey, func(rawObj runtime.Object) []string {
			pod := rawObj.(*corev1.Pod)
			owner := metav1.GetControllerOf(pod)
			if owner == nil {
				return nil
			}

			if owner.APIVersion != apiGVString || owner.Kind != v1alpha1.ClusterKind {
				return nil
			}

			return []string{owner.Name}
		}); err != nil {
		return err
	}

	// Create a new indexed field on PVCs.
	if err := mgr.GetFieldIndexer().IndexField(
		&corev1.PersistentVolumeClaim{},
		pvcOwnerKey, func(rawObj runtime.Object) []string {
			persistentVolumeClaim := rawObj.(*corev1.PersistentVolumeClaim)
			owner := metav1.GetControllerOf(persistentVolumeClaim)
			if owner == nil {
				return nil
			}

			if owner.APIVersion != apiGVString || owner.Kind != v1alpha1.ClusterKind {
				return nil
			}

			return []string{owner.Name}
		}); err != nil {
		return err
	}

	// Create a new indexed field on Jobs.
	if err := mgr.GetFieldIndexer().IndexField(
		&batchv1.Job{},
		jobOwnerKey, func(rawObj runtime.Object) []string {
			job := rawObj.(*batchv1.Job)
			owner := metav1.GetControllerOf(job)
			if owner == nil {
				return nil
			}

			if owner.APIVersion != apiGVString || owner.Kind != v1alpha1.ClusterKind {
				return nil
			}

			return []string{owner.Name}
		}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Cluster{}).
		Owns(&corev1.Pod{}).
		Owns(&batchv1.Job{}).
		WithEventFilter(&ClusterPredicate{}).
		Complete(r)
}
