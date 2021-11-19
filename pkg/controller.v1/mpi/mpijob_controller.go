// Copyright 2021 The Kubeflow Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mpi

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	commonv1 "github.com/kubeflow/common/pkg/apis/common/v1"
	"github.com/kubeflow/common/pkg/controller.v1/common"
	"github.com/kubeflow/common/pkg/controller.v1/control"
	"github.com/kubeflow/common/pkg/controller.v1/expectation"
	commonutil "github.com/kubeflow/common/pkg/util"
	"github.com/kubeflow/training-operator/pkg/common/util"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	kubeclientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"
	volcanoclient "volcano.sh/apis/pkg/client/clientset/versioned"

	mpiv1 "github.com/kubeflow/training-operator/pkg/apis/mpi/v1"
	"github.com/kubeflow/training-operator/pkg/apis/mpi/validation"
	trainingoperatorcommon "github.com/kubeflow/training-operator/pkg/common"
)

const (
	FailedDeleteJobReason     = "FailedDeleteJob"
	SuccessfulDeleteJobReason = "SuccessfulDeleteJob"
	controllerName            = "mpijob-controller"
)

func NewReconciler(mgr manager.Manager, enableGangScheduling bool) *MPIJobReconciler {
	r := &MPIJobReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		recorder: mgr.GetEventRecorderFor(controllerName),
		Log:      log.Log,
	}

	cfg := mgr.GetConfig()
	kubeClientSet := kubeclientset.NewForConfigOrDie(cfg)
	volcanoClientSet := volcanoclient.NewForConfigOrDie(cfg)
	sharedInformers := informers.NewSharedInformerFactory(kubeClientSet, 0)
	priorityClassInformer := sharedInformers.Scheduling().V1beta1().PriorityClasses()

	r.JobController = common.JobController{
		Controller:                  r,
		Expectations:                expectation.NewControllerExpectations(),
		Config:                      common.JobControllerConfiguration{EnableGangScheduling: enableGangScheduling},
		WorkQueue:                   &util.FakeWorkQueue{},
		Recorder:                    r.recorder,
		KubeClientSet:               kubeClientSet,
		VolcanoClientSet:            volcanoClientSet,
		PriorityClassLister:         priorityClassInformer.Lister(),
		PriorityClassInformerSynced: priorityClassInformer.Informer().HasSynced,
		PodControl:                  control.RealPodControl{KubeClient: kubeClientSet, Recorder: r.recorder},
		ServiceControl:              control.RealServiceControl{KubeClient: kubeClientSet, Recorder: r.recorder},
	}
	return r
}

// MPIJobReconciler reconciles a MPIJob object
type MPIJobReconciler struct {
	common.JobController
	client.Client
	Scheme   *runtime.Scheme
	recorder record.EventRecorder
	Log      logr.Logger
}

//+kubebuilder:rbac:groups=kubeflow.org,resources=mpijobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kubeflow.org,resources=mpijobs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kubeflow.org,resources=mpijobs/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=serviceaccount,verbs=get;list;watch;create;delete
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=roles,verbs=get;list;watch;create;delete;update
//+kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=rolebindings,verbs=get;list;watch;create;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *MPIJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)
	logger := r.Log.WithValues(mpiv1.Singular, req.NamespacedName)

	mpijob := &mpiv1.MPIJob{}
	err := r.Get(ctx, req.NamespacedName, mpijob)
	if err != nil {
		logger.Info(err.Error(), "unable to fetch MPIJob", req.NamespacedName.String())
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err = validation.ValidateV1MpiJobSpec(&mpijob.Spec); err != nil {
		logger.Info(err.Error(), "MPIJob failed validation", req.NamespacedName.String())
	}

	// Check if reconciliation is needed
	jobKey, err := common.KeyFunc(mpijob)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get jobKey for job object %#v: %v", mpijob, err))
	}
	replicaTypes := util.GetReplicaTypes(mpijob.Spec.MPIReplicaSpecs)
	needReconcile := util.SatisfiedExpectations(r.Expectations, jobKey, replicaTypes)

	if !needReconcile || mpijob.GetDeletionTimestamp() != nil {
		return ctrl.Result{}, nil
	}

	// Set default priorities to mpijob
	r.Scheme.Default(mpijob)

	// Use common to reconcile the job related pod and service
	//mpijob not need service
	err = r.ReconcileJobs(mpijob, mpijob.Spec.MPIReplicaSpecs, mpijob.Status, &mpijob.Spec.RunPolicy)
	if err != nil {
		logrus.Warnf("Reconcile MPIJob error %v", err)
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *MPIJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c, err := controller.New(r.ControllerName(), mgr, controller.Options{
		Reconciler: r,
	})

	if err != nil {
		return err
	}

	// using onOwnerCreateFunc is easier to set defaults
	if err = c.Watch(&source.Kind{Type: &mpiv1.MPIJob{}}, &handler.EnqueueRequestForObject{},
		predicate.Funcs{CreateFunc: r.onOwnerCreateFunc()},
	); err != nil {
		return err
	}

	// inject watching for job related pod
	if err = c.Watch(&source.Kind{Type: &corev1.Pod{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &mpiv1.MPIJob{},
	}, predicate.Funcs{
		CreateFunc: util.OnDependentCreateFunc(r.Expectations),
		UpdateFunc: util.OnDependentUpdateFunc(&r.JobController),
		DeleteFunc: util.OnDependentDeleteFunc(r.Expectations),
	}); err != nil {
		return err
	}

	// inject watching for job related service
	if err = c.Watch(&source.Kind{Type: &corev1.Service{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &mpiv1.MPIJob{},
	}, predicate.Funcs{
		CreateFunc: util.OnDependentCreateFunc(r.Expectations),
		UpdateFunc: util.OnDependentUpdateFunc(&r.JobController),
		DeleteFunc: util.OnDependentDeleteFunc(r.Expectations),
	}); err != nil {
		return err
	}

	return nil
}

//mpijob not need delete services
func (r *MPIJobReconciler) DeletePodsAndServices(runPolicy *commonv1.RunPolicy, job interface{}, pods []*corev1.Pod) error {
	if len(pods) == 0 {
		return nil
	}

	// Delete nothing when the cleanPodPolicy is None.
	if *runPolicy.CleanPodPolicy == commonv1.CleanPodPolicyNone {
		return nil
	}

	for _, pod := range pods {
		// Note that pending pod will turn into running once schedulable,
		// not cleaning it may leave orphan running pod in the future,
		// we should treat it equivalent to running phase here.
		if *runPolicy.CleanPodPolicy == commonv1.CleanPodPolicyRunning && pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodPending {
			continue
		}
		if err := r.PodControl.DeletePod(pod.Namespace, pod.Name, job.(runtime.Object)); err != nil {
			return err
		}
	}
	return nil
}

// reconcileServices checks and updates services for each given ReplicaSpec.
// It will requeue the job in case of an error while creating/deleting services.
// mpijob not need services
func (jc *MPIJobReconciler) ReconcileServices(
	job metav1.Object,
	services []*corev1.Service,
	rtype commonv1.ReplicaType,
	spec *commonv1.ReplicaSpec) error {
	return nil
}

func (r *MPIJobReconciler) ControllerName() string {
	return controllerName
}

func (r *MPIJobReconciler) GetAPIGroupVersionKind() schema.GroupVersionKind {
	return mpiv1.GroupVersion.WithKind(mpiv1.Kind)
}

func (r *MPIJobReconciler) GetAPIGroupVersion() schema.GroupVersion {
	return mpiv1.GroupVersion
}

func (r *MPIJobReconciler) GetGroupNameLabelValue() string {
	return mpiv1.GroupVersion.Group
}

// Same as Func (tc *TFController) SetClusterSpec(...) in pod.go
func (r *MPIJobReconciler) SetClusterSpec(job interface{}, podTemplate *corev1.PodTemplateSpec, rtype, index string) error {
	return nil
}

func (r *MPIJobReconciler) GetDefaultContainerName() string {
	return mpiv1.DefaultContainerName
}

func (r *MPIJobReconciler) GetDefaultContainerPortName() string {
	return mpiv1.DefaultPortName
}

func (r *MPIJobReconciler) IsMasterRole(replicas map[commonv1.ReplicaType]*commonv1.ReplicaSpec,
	rtype commonv1.ReplicaType, index int) bool {
	return false
}

func (r *MPIJobReconciler) GetJobFromInformerCache(namespace, name string) (metav1.Object, error) {
	mpijob := &mpiv1.MPIJob{}
	err := r.Get(context.Background(), types.NamespacedName{
		Namespace: namespace, Name: name,
	}, mpijob)
	return mpijob, err
}

// onOwnerCreateFunc modify creation condition.
func (r *MPIJobReconciler) onOwnerCreateFunc() func(event.CreateEvent) bool {
	return func(e event.CreateEvent) bool {
		mpiJob, ok := e.Object.(*mpiv1.MPIJob)
		if !ok {
			return true
		}

		r.Scheme.Default(mpiJob)
		msg := fmt.Sprintf("MPIJob %s/%s is created.", mpiJob.Namespace, e.Object.GetName())
		logrus.Info(msg)
		trainingoperatorcommon.CreatedJobsCounterInc(mpiJob.Namespace, mpiv1.FrameworkName)
		if err := commonutil.UpdateJobConditions(&mpiJob.Status, commonv1.JobCreated, mpiJobCreatedReason, msg); err != nil {
			log.Log.Error(err, "append job condition error")
			return false
		}
		return true
	}
}

func (r *MPIJobReconciler) ReconcilePods(
	job interface{},
	jobStatus *commonv1.JobStatus,
	pods []*corev1.Pod,
	rtype commonv1.ReplicaType,
	spec *commonv1.ReplicaSpec,
	replicas map[commonv1.ReplicaType]*commonv1.ReplicaSpec,
) error {

	mpiJob, ok := job.(*mpiv1.MPIJob)
	if !ok {
		return fmt.Errorf("%v is not a type of MPIJob", mpiJob)
	}

	// first set StartTime.
	if mpiJob.Status.StartTime == nil {
		now := metav1.Now()
		mpiJob.Status.StartTime = &now
	}

	initializeReplicaStatuses(jobStatus, rtype)

	// Get the launcher Job for this MPIJob.
	launcher, err := r.getLauncherJob(mpiJob)
	if err != nil {
		return err
	}

	var worker []*corev1.Pod
	// We're done if the launcher either succeeded or failed.
	done := launcher != nil && isPodFinished(launcher)

	if !done {
		workerSpec := mpiJob.Spec.MPIReplicaSpecs[mpiv1.MPIReplicaTypeWorker]
		workerReplicas := int32(0)
		if workerSpec != nil && workerSpec.Replicas != nil {
			workerReplicas = *workerSpec.Replicas
		}
		isGPULauncher := isGPULauncher(mpiJob)

		// Get the launcher ServiceAccount for this MPIJob.
		if sa, err := r.getOrCreateLauncherServiceAccount(mpiJob); sa == nil || err != nil {
			return err
		}

		// Get the ConfigMap for this MPIJob.
		if config, err := r.getOrCreateConfigMap(mpiJob, workerReplicas, isGPULauncher); config == nil || err != nil {
			return err
		}

		// Get the launcher Role for this MPIJob.
		if r, err := r.getOrCreateLauncherRole(mpiJob, workerReplicas); r == nil || err != nil {
			return err
		}

		// Get the launcher RoleBinding for this MPIJob.
		if rb, err := r.getLauncherRoleBinding(mpiJob); rb == nil || err != nil {
			return err
		}

		// Get the PodGroup for this MPIJob
		// if c.gangSchedulerName != "" {
		// 	if podgroup, err := c.getOrCreatePodGroups(mpiJob, workerReplicas+1); podgroup == nil || err != nil {
		// 		return err
		// 	}
		// }

		worker, err = r.getOrCreateWorker(mpiJob)
		if err != nil {
			return err
		}

		if launcher == nil {
			launcher, err = r.KubeClientSet.CoreV1().Pods(mpiJob.Namespace).Create(context.Background(), r.newLauncher(mpiJob, kubectlDeliveryImage, isGPULauncher), metav1.CreateOptions{})
			if err != nil {
				r.Recorder.Eventf(mpiJob, corev1.EventTypeWarning, mpiJobFailedReason, "launcher pod created failed: %v", err)
				return err
			} else {
				r.Recorder.Eventf(mpiJob, corev1.EventTypeNormal, mpiJobRunningReason, "launcher pod created success: %v", launcher.Name)
			}
		}
	}

	// Finally, we update the status block of the MPIJob resource to reflect the
	// current state of the world.
	err = r.updateMPIJobStatus(mpiJob, launcher, worker)
	if err != nil {
		return err
	}
	return nil
}

func (r *MPIJobReconciler) updateMPIJobStatus(mpiJob *mpiv1.MPIJob, launcher *corev1.Pod, worker []*corev1.Pod) error {
	//oldStatus := mpiJob.Status.DeepCopy()
	if launcher != nil {
		initializeMPIJobStatuses(mpiJob, mpiv1.MPIReplicaTypeLauncher)
		if isPodSucceeded(launcher) {
			mpiJob.Status.ReplicaStatuses[commonv1.ReplicaType(mpiv1.MPIReplicaTypeLauncher)].Succeeded = 1
			msg := fmt.Sprintf("MPIJob %s/%s successfully completed.", mpiJob.Namespace, mpiJob.Name)
			r.Recorder.Event(mpiJob, corev1.EventTypeNormal, mpiJobSucceededReason, msg)
			if mpiJob.Status.CompletionTime == nil {
				now := metav1.Now()
				mpiJob.Status.CompletionTime = &now
			}
			err := updateMPIJobConditions(mpiJob, commonv1.JobSucceeded, mpiJobSucceededReason, msg)
			if err != nil {
				return err
			}
		} else if isPodFailed(launcher) {
			mpiJob.Status.ReplicaStatuses[commonv1.ReplicaType(mpiv1.MPIReplicaTypeLauncher)].Failed = 1
			msg := fmt.Sprintf("MPIJob %s/%s has failed", mpiJob.Namespace, mpiJob.Name)
			reason := launcher.Status.Reason
			if reason == "" {
				reason = mpiJobFailedReason
			}
			r.Recorder.Event(mpiJob, corev1.EventTypeWarning, reason, msg)
			if reason == "Evicted" {
				reason = mpiJobEvict
			} else if !isEvicted(mpiJob.Status) && mpiJob.Status.CompletionTime == nil {
				now := metav1.Now()
				mpiJob.Status.CompletionTime = &now
			}
			err := updateMPIJobConditions(mpiJob, commonv1.JobFailed, reason, msg)
			if err != nil {
				klog.Errorf("Append mpiJob(%s/%s) condition error: %v", mpiJob.Namespace, mpiJob.Name, err)
				return err
			}

		} else if isPodRunning(launcher) {
			mpiJob.Status.ReplicaStatuses[commonv1.ReplicaType(mpiv1.MPIReplicaTypeLauncher)].Active = 1
		}
	}

	var (
		running = 0
		evict   = 0
	)

	initializeMPIJobStatuses(mpiJob, mpiv1.MPIReplicaTypeWorker)
	//spec := mpiJob.Spec.MPIReplicaSpecs[kubeflow.MPIReplicaTypeWorker]
	for i := 0; i < len(worker); i++ {
		switch worker[i].Status.Phase {
		case corev1.PodFailed:
			mpiJob.Status.ReplicaStatuses[commonv1.ReplicaType(mpiv1.MPIReplicaTypeWorker)].Failed += 1
			if worker[i].Status.Reason == "Evicted" {
				evict += 1
			}
		case corev1.PodSucceeded:
			mpiJob.Status.ReplicaStatuses[commonv1.ReplicaType(mpiv1.MPIReplicaTypeWorker)].Succeeded += 1
		case corev1.PodRunning:
			running += 1
			mpiJob.Status.ReplicaStatuses[commonv1.ReplicaType(mpiv1.MPIReplicaTypeWorker)].Active += 1
		}
	}
	if evict > 0 {
		msg := fmt.Sprintf("%d/%d workers are evicted", evict, len(worker))
		if err := updateMPIJobConditions(mpiJob, commonv1.JobFailed, mpiJobEvict, msg); err != nil {
			return err
		}
		r.Recorder.Event(mpiJob, corev1.EventTypeWarning, mpiJobEvict, msg)
	}

	if launcher != nil && launcher.Status.Phase == corev1.PodRunning && running == len(worker) {
		msg := fmt.Sprintf("MPIJob %s/%s is running.", mpiJob.Namespace, mpiJob.Name)
		err := updateMPIJobConditions(mpiJob, commonv1.JobRunning, mpiJobRunningReason, msg)
		if err != nil {
			return err
		}
		r.Recorder.Eventf(mpiJob, corev1.EventTypeNormal, "MPIJobRunning", "MPIJob %s/%s is running", mpiJob.Namespace, mpiJob.Name)
	}

	// no need to update the mpijob if the status hasn't changed since last time.
	// if !reflect.DeepEqual(*oldStatus, mpiJob.Status) {
	// 	return r.UpdateJobStatusInApiServer(mpiJob, mpiJob.Status)
	// }
	return nil
}

func (r *MPIJobReconciler) GetJobFromAPIClient(namespace, name string) (metav1.Object, error) {
	job := &mpiv1.MPIJob{}

	clientReader, err := util.GetDelegatingClientFromClient(r.Client)
	if err != nil {
		return nil, err
	}
	err = clientReader.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, job)
	if err != nil {
		if errors.IsNotFound(err) {
			logrus.Error(err, "tensorflow job not found", "namespace", namespace, "name", name)
		} else {
			logrus.Error(err, "failed to get job from api-server", "namespace", namespace, "name", name)
		}
		return nil, err
	}
	return job, nil
}

// GetPodsForJob returns the set of pods that this job should manage.
// It also reconciles ControllerRef by adopting/orphaning.
// Note that the returned Pods are pointers into the cache.
func (r *MPIJobReconciler) GetPodsForJob(jobObject interface{}) ([]*corev1.Pod, error) {
	job, ok := jobObject.(metav1.Object)
	if !ok {
		return nil, fmt.Errorf("job is not of type metav1.Object")
	}

	// Create selector.
	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: r.GenLabels(job.GetName()),
	})

	if err != nil {
		return nil, fmt.Errorf("couldn't convert Job selector: %v", err)
	}
	// List all pods to include those that don't match the selector anymore
	// but have a ControllerRef pointing to this controller.
	podlist := &corev1.PodList{}
	err = r.List(context.Background(), podlist,
		client.MatchingLabelsSelector{Selector: selector}, client.InNamespace(job.GetNamespace()))
	if err != nil {
		return nil, err
	}

	pods := util.ConvertPodList(podlist.Items)

	// If any adoptions are attempted, we should first recheck for deletion
	// with an uncached quorum read sometime after listing Pods (see #42639).
	canAdoptFunc := common.RecheckDeletionTimestamp(func() (metav1.Object, error) {
		fresh, err := r.Controller.GetJobFromAPIClient(job.GetNamespace(), job.GetName())
		if err != nil {
			return nil, err
		}
		if fresh.GetUID() != job.GetUID() {
			return nil, fmt.Errorf("original Job %v/%v is gone: got uid %v, wanted %v", job.GetNamespace(), job.GetName(), fresh.GetUID(), job.GetUID())
		}
		return fresh, nil
	})
	cm := control.NewPodControllerRefManager(r.PodControl, job, selector, r.Controller.GetAPIGroupVersionKind(), canAdoptFunc)
	return cm.ClaimPods(pods)
}

func (r *MPIJobReconciler) DeleteJob(job interface{}) error {
	mpiJob, ok := job.(*mpiv1.MPIJob)
	if !ok {
		return fmt.Errorf("%v is not a type of TFJob", mpiJob)
	}

	log := commonutil.LoggerForJob(mpiJob)
	if err := r.Delete(context.Background(), mpiJob); err != nil {
		r.Recorder.Eventf(mpiJob, corev1.EventTypeWarning, FailedDeleteJobReason, "Error deleting: %v", err)
		log.Errorf("failed to delete job %s/%s, %v", mpiJob.Namespace, mpiJob.Name, err)
		return err
	}

	r.Recorder.Eventf(mpiJob, corev1.EventTypeNormal, SuccessfulDeleteJobReason, "Deleted job: %v", mpiJob.Name)
	log.Infof("job %s/%s has been deleted", mpiJob.Namespace, mpiJob.Name)
	trainingoperatorcommon.DeletedJobsCounterInc(mpiJob.Namespace, mpiv1.FrameworkName)
	return nil
}

// GetServicesForJob returns the set of services that this job should manage.
// It also reconciles ControllerRef by adopting/orphaning.
// Note that the returned services are pointers into the cache.
func (r *MPIJobReconciler) GetServicesForJob(jobObject interface{}) ([]*corev1.Service, error) {
	return nil, nil
}

func (r *MPIJobReconciler) UpdateJobStatus(job interface{}, replicas map[commonv1.ReplicaType]*commonv1.ReplicaSpec, jobStatus *commonv1.JobStatus) error {
	mpiJob, ok := job.(*mpiv1.MPIJob)
	if !ok {
		return fmt.Errorf("%+v is not a type of MPIJob", job)
	}

	for rtype, spec := range replicas {
		status := jobStatus.ReplicaStatuses[rtype]

		succeeded := status.Succeeded
		expected := *(spec.Replicas) - succeeded
		running := status.Active
		failed := status.Failed

		logrus.Infof("MPIJob=%s, ReplicaType=%s expected=%d, running=%d, succeeded=%d , failed=%d",
			mpiJob.Name, rtype, expected, running, succeeded, failed)

		if rtype == commonv1.ReplicaType(mpiv1.MPIReplicaTypeLauncher) {
			if running > 0 {
				msg := fmt.Sprintf("MPIJob %s is running.", mpiJob.Name)
				err := commonutil.UpdateJobConditions(jobStatus, commonv1.JobRunning, commonutil.JobRunningReason, msg)
				if err != nil {
					commonutil.LoggerForJob(mpiJob).Infof("Append job condition error: %v", err)
					return err
				}
			}
			// when master is succeed, the job is finished.
			if expected == 0 {
				msg := fmt.Sprintf("MPIJob %s is successfully completed.", mpiJob.Name)
				logrus.Info(msg)
				r.Recorder.Event(mpiJob, corev1.EventTypeNormal, commonutil.JobSucceededReason, msg)
				if jobStatus.CompletionTime == nil {
					now := metav1.Now()
					jobStatus.CompletionTime = &now
				}
				err := commonutil.UpdateJobConditions(jobStatus, commonv1.JobSucceeded, commonutil.JobSucceededReason, msg)
				if err != nil {
					commonutil.LoggerForJob(mpiJob).Infof("Append job condition error: %v", err)
					return err
				}
				trainingoperatorcommon.SuccessfulJobsCounterInc(mpiJob.Namespace, mpiv1.FrameworkName)
				return nil
			}
		}
		if failed > 0 {
			if spec.RestartPolicy == commonv1.RestartPolicyExitCode {
				msg := fmt.Sprintf("MPIJob %s is restarting because %d %s replica(s) failed.", mpiJob.Name, failed, rtype)
				r.Recorder.Event(mpiJob, corev1.EventTypeWarning, commonutil.JobRestartingReason, msg)
				err := commonutil.UpdateJobConditions(jobStatus, commonv1.JobRestarting, commonutil.JobRestartingReason, msg)
				if err != nil {
					commonutil.LoggerForJob(mpiJob).Infof("Append job condition error: %v", err)
					return err
				}
				trainingoperatorcommon.RestartedJobsCounterInc(mpiJob.Namespace, mpiv1.FrameworkName)
			} else {
				msg := fmt.Sprintf("MPIJob %s is failed because %d %s replica(s) failed.", mpiJob.Name, failed, rtype)
				r.Recorder.Event(mpiJob, corev1.EventTypeNormal, commonutil.JobFailedReason, msg)
				if mpiJob.Status.CompletionTime == nil {
					now := metav1.Now()
					mpiJob.Status.CompletionTime = &now
				}
				err := commonutil.UpdateJobConditions(jobStatus, commonv1.JobFailed, commonutil.JobFailedReason, msg)
				if err != nil {
					commonutil.LoggerForJob(mpiJob).Infof("Append job condition error: %v", err)
					return err
				}
				trainingoperatorcommon.FailedJobsCounterInc(mpiJob.Namespace, mpiv1.FrameworkName)
			}
		}
	}

	// Some workers are still running, leave a running condition.
	msg := fmt.Sprintf("MPIJob %s is running.", mpiJob.Name)
	commonutil.LoggerForJob(mpiJob).Infof(msg)

	if err := commonutil.UpdateJobConditions(jobStatus, commonv1.JobRunning, commonutil.JobRunningReason, msg); err != nil {
		commonutil.LoggerForJob(mpiJob).Error(err, "failed to update MPIJob conditions")
		return err
	}

	return nil
}

func (r *MPIJobReconciler) UpdateJobStatusInApiServer(job interface{}, jobStatus *commonv1.JobStatus) error {
	mpiJob, ok := job.(*mpiv1.MPIJob)
	if !ok {
		return fmt.Errorf("%v is not a type of MpiJob", mpiJob)
	}

	startTime := time.Now()
	logger := commonutil.LoggerForJob(mpiJob)
	defer func() {
		logger.Infof("Finished updating MpiJobs Status %q (%v)",
			mpiJob.Name, time.Since(startTime))
	}()

	mpiJob = mpiJob.DeepCopy()
	mpiJob.Status = *jobStatus.DeepCopy()

	result := r.Status().Update(context.Background(), mpiJob)

	if result != nil {
		r.Log.WithValues("mpijob", types.NamespacedName{
			Namespace: mpiJob.GetNamespace(),
			Name:      mpiJob.GetName(),
		})
		return result
	}

	return nil
}

// getLauncherJob gets the launcher Job controlled by this MPIJob.
func (r *MPIJobReconciler) getLauncherJob(mpiJob *mpiv1.MPIJob) (*corev1.Pod, error) {
	launcher := &corev1.Pod{}
	NamespacedName := types.NamespacedName{Namespace: mpiJob.Namespace, Name: mpiJob.Name + launcherSuffix}
	err := r.Get(context.Background(), NamespacedName, launcher)
	if errors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		// If an error occurs during Get, we'll requeue the item so we can
		// attempt processing again later. This could have been caused by a
		// temporary network failure, or any other transient reason.
		return nil, err
	}

	// If the launcher is not controlled by this MPIJob resource, we should log
	// a warning to the event recorder and return.
	if !metav1.IsControlledBy(launcher, mpiJob) {
		msg := fmt.Sprintf(MessageResourceExists, launcher.Name, launcher.Kind)
		r.Recorder.Event(mpiJob, corev1.EventTypeWarning, ErrResourceExists, msg)
		return launcher, fmt.Errorf(msg)
	}
	return launcher, nil
}

// getOrCreateConfigMap gets the ConfigMap controlled by this MPIJob, or creates
// one if it doesn't exist.
func (r *MPIJobReconciler) getOrCreateConfigMap(mpiJob *mpiv1.MPIJob, workerReplicas int32, isGPULauncher bool) (*corev1.ConfigMap, error) {
	newCM := newConfigMap(mpiJob, workerReplicas, isGPULauncher)
	podList, err := r.getRunningWorkerPods(mpiJob)
	if err != nil {
		return nil, err
	}
	updateDiscoverHostsInConfigMap(newCM, mpiJob, podList, isGPULauncher)

	//cm, err := r.configMapLister.ConfigMaps(mpiJob.Namespace).Get(mpiJob.Name + configSuffix)

	cm := &corev1.ConfigMap{}
	NamespacedName := types.NamespacedName{Namespace: mpiJob.Namespace, Name: mpiJob.Name + configSuffix}
	err = r.Get(context.Background(), NamespacedName, cm)

	// If the ConfigMap doesn't exist, we'll create it.
	if errors.IsNotFound(err) {
		cm, err = r.KubeClientSet.CoreV1().ConfigMaps(mpiJob.Namespace).Create(context.Background(), newCM, metav1.CreateOptions{})
	}
	// If an error occurs during Get/Create, we'll requeue the item so we
	// can attempt processing again later. This could have been caused by a
	// temporary network failure, or any other transient reason.
	if err != nil {
		return nil, err
	}

	// If the ConfigMap is not controlled by this MPIJob resource, we
	// should log a warning to the event recorder and return.
	if !metav1.IsControlledBy(cm, mpiJob) {
		msg := fmt.Sprintf(MessageResourceExists, cm.Name, cm.Kind)
		r.Recorder.Event(mpiJob, corev1.EventTypeWarning, ErrResourceExists, msg)
		return nil, fmt.Errorf(msg)
	}

	// If the ConfigMap is changed, update it
	if !reflect.DeepEqual(cm.Data, newCM.Data) {
		cm, err = r.KubeClientSet.CoreV1().ConfigMaps(mpiJob.Namespace).Update(context.Background(), newCM, metav1.UpdateOptions{})
		if err != nil {
			return nil, err
		}
	}

	return cm, nil
}

// getOrCreateLauncherServiceAccount gets the launcher ServiceAccount controlled
// by this MPIJob, or creates one if it doesn't exist.
func (r *MPIJobReconciler) getOrCreateLauncherServiceAccount(mpiJob *mpiv1.MPIJob) (*corev1.ServiceAccount, error) {

	sa := &corev1.ServiceAccount{}
	NamespacedName := types.NamespacedName{Namespace: mpiJob.Namespace, Name: mpiJob.Name + launcherSuffix}
	err := r.Get(context.Background(), NamespacedName, sa)

	if err == nil {
		r.Recorder.Eventf(mpiJob, corev1.EventTypeNormal, "ServiceAccount is exist", "ServiceAccount: %v", sa.Name)
	}

	if errors.IsNotFound(err) {
		sa, err = r.KubeClientSet.CoreV1().ServiceAccounts(mpiJob.Namespace).Create(context.Background(), newLauncherServiceAccount(mpiJob), metav1.CreateOptions{})
	}
	// If an error occurs during Get/Create, we'll requeue the item so we
	// can attempt processing again later. This could have been caused by a
	// temporary network failure, or any other transient reason.
	if err != nil {
		return nil, err
	}
	// If the launcher ServiceAccount is not controlled by this MPIJob resource, we
	// should log a warning to the event recorder and return.
	if !metav1.IsControlledBy(sa, mpiJob) {
		msg := fmt.Sprintf(MessageResourceExists, sa.Name, sa.Kind)
		r.Recorder.Event(mpiJob, corev1.EventTypeWarning, ErrResourceExists, msg)
		return nil, fmt.Errorf(msg)
	}

	return sa, nil
}

// getOrCreateLauncherRole gets the launcher Role controlled by this MPIJob.
func (r *MPIJobReconciler) getOrCreateLauncherRole(mpiJob *mpiv1.MPIJob, workerReplicas int32) (*rbacv1.Role, error) {
	role := &rbacv1.Role{}
	NamespacedName := types.NamespacedName{Namespace: mpiJob.Namespace, Name: mpiJob.Name + launcherSuffix}
	err := r.Get(context.Background(), NamespacedName, role)

	if err == nil {
		r.Recorder.Eventf(mpiJob, corev1.EventTypeNormal, "LauncherRole is exist", "LauncherRole: %v", role.Name)
	}

	launcherRole := newLauncherRole(mpiJob, workerReplicas)
	// If the Role doesn't exist, we'll create it.
	if errors.IsNotFound(err) {
		role, err = r.KubeClientSet.RbacV1().Roles(mpiJob.Namespace).Create(context.Background(), launcherRole, metav1.CreateOptions{})
	}
	// If an error occurs during Get/Create, we'll requeue the item so we
	// can attempt processing again later. This could have been caused by a
	// temporary network failure, or any other transient reason.
	if err != nil {
		return nil, err
	}
	// If the launcher Role is not controlled by this MPIJob resource, we
	// should log a warning to the event recorder and return.
	if !metav1.IsControlledBy(role, mpiJob) {
		msg := fmt.Sprintf(MessageResourceExists, role.Name, role.Kind)
		r.Recorder.Event(mpiJob, corev1.EventTypeWarning, ErrResourceExists, msg)
		return nil, fmt.Errorf(msg)
	}

	if !reflect.DeepEqual(role.Rules, launcherRole.Rules) {
		role, err = r.KubeClientSet.RbacV1().Roles(mpiJob.Namespace).Update(context.Background(), launcherRole, metav1.UpdateOptions{})
		if err != nil {
			return nil, err
		}
	}

	return role, nil
}

// getLauncherRoleBinding gets the launcher RoleBinding controlled by this
// MPIJob, or creates one if it doesn't exist.
func (r *MPIJobReconciler) getLauncherRoleBinding(mpiJob *mpiv1.MPIJob) (*rbacv1.RoleBinding, error) {
	rb := &rbacv1.RoleBinding{}
	NamespacedName := types.NamespacedName{Namespace: mpiJob.Namespace, Name: mpiJob.Name + launcherSuffix}
	err := r.Get(context.Background(), NamespacedName, rb)
	// If the RoleBinding doesn't exist, we'll create it.

	if err == nil {
		r.Recorder.Eventf(mpiJob, corev1.EventTypeNormal, "RoleBinding is exist", "RoleBinding: %v", rb.Name)
	}

	if errors.IsNotFound(err) {
		rb, err = r.KubeClientSet.RbacV1().RoleBindings(mpiJob.Namespace).Create(context.Background(), newLauncherRoleBinding(mpiJob), metav1.CreateOptions{})
	}
	// If an error occurs during Get/Create, we'll requeue the item so we
	// can attempt processing again later. This could have been caused by a
	// temporary network failure, or any other transient reason.
	if err != nil {
		return nil, err
	}
	// If the launcher RoleBinding is not controlled by this MPIJob resource, we
	// should log a warning to the event recorder and return.
	if !metav1.IsControlledBy(rb, mpiJob) {
		msg := fmt.Sprintf(MessageResourceExists, rb.Name, rb.Kind)
		r.Recorder.Event(mpiJob, corev1.EventTypeWarning, ErrResourceExists, msg)
		return nil, fmt.Errorf(msg)
	}

	return rb, nil
}

// getOrCreateWorker gets the worker Pod controlled by this
// MPIJob, or creates one if it doesn't exist.
func (r *MPIJobReconciler) getOrCreateWorker(mpiJob *mpiv1.MPIJob) ([]*corev1.Pod, error) {
	var (
		workerPrefix   string        = mpiJob.Name + workerSuffix
		workerPods     []*corev1.Pod = []*corev1.Pod{}
		i              int32         = 0
		workerReplicas *int32
	)
	if worker, ok := mpiJob.Spec.MPIReplicaSpecs[mpiv1.MPIReplicaTypeWorker]; ok && worker != nil {
		workerReplicas = worker.Replicas
	} else {
		return workerPods, nil
	}

	// Remove Pods when replicas are scaled down
	selector, err := workerSelector(mpiJob.Name)
	if err != nil {
		return nil, err
	}

	podlist := &corev1.PodList{}
	err = r.List(context.Background(), podlist, client.MatchingLabelsSelector{Selector: selector}, client.InNamespace(mpiJob.GetNamespace()))

	if err != nil {
		return nil, err
	}
	if len(podlist.Items) > int(*workerReplicas) {
		for _, pod := range podlist.Items {
			indexStr, ok := pod.Labels[commonv1.ReplicaIndexLabel]
			if !ok {
				return nil, err
			}
			index, err := strconv.Atoi(indexStr)
			if err == nil {
				if index >= int(*workerReplicas) {
					err = r.KubeClientSet.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
					if err != nil {
						return nil, err
					}
				}
			}
		}
	}

	for ; i < *workerReplicas; i++ {
		name := fmt.Sprintf("%s-%d", workerPrefix, i)

		pod := &corev1.Pod{}
		NamespacedName := types.NamespacedName{Namespace: mpiJob.Namespace, Name: name}
		err := r.Get(context.Background(), NamespacedName, pod)

		// If the worker Pod doesn't exist, we'll create it.
		if errors.IsNotFound(err) {
			worker := r.newWorker(mpiJob, name)
			if worker == nil {
				msg := fmt.Sprintf(MessageResourceDoesNotExist, "Worker")
				r.Recorder.Event(mpiJob, corev1.EventTypeWarning, ErrResourceDoesNotExist, msg)
				err = fmt.Errorf(msg)
				return nil, err
			}
			// Insert ReplicaIndexLabel
			worker.Labels[commonv1.ReplicaIndexLabel] = strconv.Itoa(int(i))
			pod, err = r.KubeClientSet.CoreV1().Pods(mpiJob.Namespace).Create(context.Background(), worker, metav1.CreateOptions{})
			if err == nil {
				r.Recorder.Eventf(mpiJob, corev1.EventTypeNormal, "SuccessfulCreatePod", "Created worker pod: %v", pod.Name)
			} else {
				r.Recorder.Eventf(mpiJob, corev1.EventTypeWarning, "FailedCreatePod", "Created worker pod: %v", pod.Name)
			}
		}

		// If an error occurs during Get/Create, we'll requeue the item so we
		// can attempt processing again later. This could have been caused by a
		// temporary network failure, or any other transient reason.
		if err != nil && !errors.IsNotFound(err) {
			r.Recorder.Eventf(mpiJob, corev1.EventTypeWarning, mpiJobFailedReason, "worker pod created failed: %v", err)
			return nil, err
		}
		// If the worker is not controlled by this MPIJob resource, we should log
		// a warning to the event recorder and return.
		if pod != nil && !metav1.IsControlledBy(pod, mpiJob) {
			msg := fmt.Sprintf(MessageResourceExists, pod.Name, pod.Kind)
			r.Recorder.Event(mpiJob, corev1.EventTypeWarning, ErrResourceExists, msg)
			return nil, fmt.Errorf(msg)
		}
		workerPods = append(workerPods, pod)
	}

	return workerPods, nil
}

// newWorker creates a new worker Pod for an MPIJob resource. It also
// sets the appropriate OwnerReferences on the resource so handleObject can
// discover the MPIJob resource that 'owns' it.
func (r *MPIJobReconciler) newWorker(mpiJob *mpiv1.MPIJob, name string) *corev1.Pod {
	labels := defaultWorkerLabels(mpiJob.Name)

	podSpec := mpiJob.Spec.MPIReplicaSpecs[mpiv1.MPIReplicaTypeWorker].Template.DeepCopy()

	// keep the labels which are set in PodTemplate
	if len(podSpec.Labels) == 0 {
		podSpec.Labels = make(map[string]string)
	}

	for key, value := range labels {
		podSpec.Labels[key] = value
	}
	setRestartPolicy(podSpec, mpiJob.Spec.MPIReplicaSpecs[mpiv1.MPIReplicaTypeWorker])
	logger := commonutil.LoggerForReplica(mpiJob, strings.ToLower(string(mpiv1.MPIReplicaTypeLauncher)))
	if len(podSpec.Spec.Containers) == 0 {
		klog.Errorln("Worker pod does not have any containers in its spec")
		return nil
	}
	container := podSpec.Spec.Containers[0]
	if len(container.Command) == 0 {
		container.Command = []string{"sleep"}
		container.Args = []string{"365d"}
	}

	// We need the kubexec.sh script here because Open MPI checks for the path
	// in every rank.
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
		Name:      configVolumeName,
		MountPath: configMountPath,
	})
	podSpec.Spec.Containers[0] = container

	scriptMode := int32(0555)
	podSpec.Spec.Volumes = append(podSpec.Spec.Volumes, corev1.Volume{
		Name: configVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: mpiJob.Name + configSuffix,
				},
				Items: []corev1.KeyToPath{
					{
						Key:  kubexecScriptName,
						Path: kubexecScriptName,
						Mode: &scriptMode,
					},
				},
			},
		},
	})

	// if gang-scheduling is enabled:
	// 1. if user has specified other scheduler, we report a warning without overriding any fields.
	// 2. if no SchedulerName is set for pods, then we set the SchedulerName to "volcano".
	if r.Config.EnableGangScheduling {
		if !util.IsGangSchedulerSet(mpiJob.Spec.MPIReplicaSpecs, gangSchedulerName) {
			errMsg := "Another scheduler is specified when gang-scheduling is enabled and it will not be overwritten"
			logger.Warning(errMsg)
			r.Recorder.Event(mpiJob, corev1.EventTypeWarning, podTemplateSchedulerNameReason, errMsg)
		} else {
			podSpec.Spec.SchedulerName = gangSchedulerName
		}

		if podSpec.Annotations == nil {
			podSpec.Annotations = map[string]string{}
		}
		podSpec.Annotations[gangSchedulingPodGroupAnnotation] = mpiJob.GetName()
		podSpec.Annotations[volcanoTaskSpecKey] = strings.ToLower(string(mpiv1.MPIReplicaTypeWorker))
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   mpiJob.Namespace,
			Labels:      podSpec.Labels,
			Annotations: podSpec.Annotations,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(mpiJob, mpiv1.SchemeGroupVersionKind),
			},
		},
		Spec: podSpec.Spec,
	}
}

// newLauncher creates a new launcher Job for an MPIJob resource. It also sets
// the appropriate OwnerReferences on the resource so handleObject can discover
// the MPIJob resource that 'owns' it.
func (r *MPIJobReconciler) newLauncher(mpiJob *mpiv1.MPIJob, kubectlDeliveryImage string, isGPULauncher bool) *corev1.Pod {
	launcherName := mpiJob.Name + launcherSuffix
	labels := map[string]string{
		labelGroupName:   "kubeflow.org",
		labelMPIJobName:  mpiJob.Name,
		labelMPIRoleType: launcher,
	}

	podSpec := mpiJob.Spec.MPIReplicaSpecs[mpiv1.MPIReplicaTypeLauncher].Template.DeepCopy()
	// copy the labels and annotations to pod from PodTemplate
	if len(podSpec.Labels) == 0 {
		podSpec.Labels = make(map[string]string)
	}
	for key, value := range labels {
		podSpec.Labels[key] = value
	}

	logger := commonutil.LoggerForReplica(mpiJob, strings.ToLower(string(mpiv1.MPIReplicaTypeLauncher)))
	// add SchedulerName to podSpec
	if r.Config.EnableGangScheduling {
		if !util.IsGangSchedulerSet(mpiJob.Spec.MPIReplicaSpecs, gangSchedulerName) {
			errMsg := "Another scheduler is specified when gang-scheduling is enabled and it will not be overwritten"
			logger.Warning(errMsg)
			r.Recorder.Event(mpiJob, corev1.EventTypeWarning, podTemplateSchedulerNameReason, errMsg)
		} else {
			podSpec.Spec.SchedulerName = gangSchedulerName
		}

		if podSpec.Annotations == nil {
			podSpec.Annotations = map[string]string{}
		}
		podSpec.Annotations[gangSchedulingPodGroupAnnotation] = mpiJob.GetName()
		podSpec.Annotations[volcanoTaskSpecKey] = strings.ToLower(string(mpiv1.MPIReplicaTypeLauncher))
	}

	podSpec.Spec.ServiceAccountName = launcherName
	podSpec.Spec.InitContainers = append(podSpec.Spec.InitContainers, corev1.Container{
		Name:            kubectlDeliveryName,
		Image:           kubectlDeliveryImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Env: []corev1.EnvVar{
			{
				Name:  kubectlTargetDirEnv,
				Value: kubectlMountPath,
			},
			{
				Name:  "NAMESPACE",
				Value: mpiJob.Namespace,
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      kubectlVolumeName,
				MountPath: kubectlMountPath,
			},
			{
				Name:      configVolumeName,
				MountPath: configMountPath,
			},
		},
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse(initContainerCpu),
				corev1.ResourceMemory:           resource.MustParse(initContainerMem),
				corev1.ResourceEphemeralStorage: resource.MustParse(initContainerEphStorage),
			},
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:              resource.MustParse(initContainerCpu),
				corev1.ResourceMemory:           resource.MustParse(initContainerMem),
				corev1.ResourceEphemeralStorage: resource.MustParse(initContainerEphStorage),
			},
		},
	})
	if len(podSpec.Spec.Containers) == 0 {
		klog.Errorln("Launcher pod does not have any containers in its spec")
		msg := fmt.Sprintf(MessageResourceDoesNotExist, "Launcher")
		r.Recorder.Event(mpiJob, corev1.EventTypeWarning, ErrResourceDoesNotExist, msg)
		return nil
	}
	container := podSpec.Spec.Containers[0]
	container.Env = append(container.Env,
		corev1.EnvVar{
			Name:  "OMPI_MCA_plm_rsh_agent",
			Value: fmt.Sprintf("%s/%s", configMountPath, kubexecScriptName),
		},
		corev1.EnvVar{
			Name:  "OMPI_MCA_orte_default_hostfile",
			Value: fmt.Sprintf("%s/%s", configMountPath, hostfileName),
		},
	)

	if !isGPULauncher {
		container.Env = append(container.Env,
			// We overwrite these environment variables so that users will not
			// be mistakenly using GPU resources for launcher due to potential
			// issues with scheduler/container technologies.
			corev1.EnvVar{
				Name:  "NVIDIA_VISIBLE_DEVICES",
				Value: "",
			},
			corev1.EnvVar{
				Name:  "NVIDIA_DRIVER_CAPABILITIES",
				Value: "",
			})
	}

	container.VolumeMounts = append(container.VolumeMounts,
		corev1.VolumeMount{
			Name:      kubectlVolumeName,
			MountPath: kubectlMountPath,
		},
		corev1.VolumeMount{
			Name:      configVolumeName,
			MountPath: configMountPath,
		})
	podSpec.Spec.Containers[0] = container

	// Submit a warning event if the user specifies restart policy for
	// the pod template. We recommend to set it from the replica level.
	if podSpec.Spec.RestartPolicy != corev1.RestartPolicy("") {
		errMsg := "Restart policy in pod template will be overwritten by restart policy in replica spec"
		klog.Warning(errMsg)
		r.Recorder.Event(mpiJob, corev1.EventTypeWarning, podTemplateRestartPolicyReason, errMsg)
	}
	setRestartPolicy(podSpec, mpiJob.Spec.MPIReplicaSpecs[mpiv1.MPIReplicaTypeLauncher])

	scriptsMode := int32(0555)
	hostfileMode := int32(0444)
	podSpec.Spec.Volumes = append(podSpec.Spec.Volumes,
		corev1.Volume{
			Name: kubectlVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		corev1.Volume{
			Name: configVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: mpiJob.Name + configSuffix,
					},
					Items: []corev1.KeyToPath{
						{
							Key:  kubexecScriptName,
							Path: kubexecScriptName,
							Mode: &scriptsMode,
						},
						{
							Key:  hostfileName,
							Path: hostfileName,
							Mode: &hostfileMode,
						},
						{
							Key:  discoverHostsScriptName,
							Path: discoverHostsScriptName,
							Mode: &scriptsMode,
						},
					},
				},
			},
		})
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        launcherName,
			Namespace:   mpiJob.Namespace,
			Labels:      podSpec.Labels,
			Annotations: podSpec.Annotations,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(mpiJob, mpiv1.SchemeGroupVersionKind),
			},
		},
		Spec: podSpec.Spec,
	}
}

// getRunningWorkerPods get all worker Pods with Running phase controlled by this MPIJob.
func (r *MPIJobReconciler) getRunningWorkerPods(mpiJob *mpiv1.MPIJob) ([]*corev1.Pod, error) {
	selector, err := workerSelector(mpiJob.Name)
	if err != nil {
		return nil, err
	}

	podFullList := &corev1.PodList{}
	err = r.List(context.Background(), podFullList, client.MatchingLabelsSelector{Selector: selector}, client.InNamespace(mpiJob.GetNamespace()))
	//podFullList, err := r.PodLister.List(selector)
	if err != nil {
		return nil, err
	}
	// Only running Pods should be included within the `discover_hosts.sh` script.
	var podList []corev1.Pod
	for idx, pod := range podFullList.Items {
		if pod.Status.Phase == corev1.PodRunning {
			podList = append(podList, podFullList.Items[idx])
		}
	}

	return util.ConvertPodList(podList), nil
}

// newConfigMap creates a new ConfigMap containing configurations for an MPIJob
// resource. It also sets the appropriate OwnerReferences on the resource so
// handleObject can discover the MPIJob resource that 'owns' it.
func newConfigMap(mpiJob *mpiv1.MPIJob, workerReplicas int32, isGPULauncher bool) *corev1.ConfigMap {
	kubexec := fmt.Sprintf(`#!/bin/sh
set -x
POD_NAME=$1
shift
%s/kubectl exec ${POD_NAME}`, kubectlMountPath)
	if len(mpiJob.Spec.MainContainer) > 0 {
		kubexec = fmt.Sprintf("%s --container %s", kubexec, mpiJob.Spec.MainContainer)
	}
	kubexec = fmt.Sprintf("%s -- /bin/sh -c \"$*\"", kubexec)

	// If no processing unit is specified, default to 1 slot.
	slots := 1
	if mpiJob.Spec.SlotsPerWorker != nil {
		slots = int(*mpiJob.Spec.SlotsPerWorker)
	}
	var buffer bytes.Buffer
	if isGPULauncher {
		buffer.WriteString(fmt.Sprintf("%s%s slots=%d\n", mpiJob.Name, launcherSuffix, slots))
	}
	for i := 0; i < int(workerReplicas); i++ {
		buffer.WriteString(fmt.Sprintf("%s%s-%d slots=%d\n", mpiJob.Name, workerSuffix, i, slots))
	}

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mpiJob.Name + configSuffix,
			Namespace: mpiJob.Namespace,
			Labels: map[string]string{
				"app": mpiJob.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(mpiJob, mpiv1.SchemeGroupVersionKind),
			},
		},
		Data: map[string]string{
			hostfileName:      buffer.String(),
			kubexecScriptName: kubexec,
		},
	}
}

// updateDiscoverHostsInConfigMap updates the ConfigMap if the content of `discover_hosts.sh` changes.
func updateDiscoverHostsInConfigMap(configMap *corev1.ConfigMap, mpiJob *mpiv1.MPIJob, runningPods []*corev1.Pod, isGPULauncher bool) {
	slots := 1
	if mpiJob.Spec.SlotsPerWorker != nil {
		slots = int(*mpiJob.Spec.SlotsPerWorker)
	}

	// Sort the slice of Pods to make sure the order of entries in `discover_hosts.sh` is maintained.
	sort.Slice(runningPods, func(i, j int) bool {
		return runningPods[i].Name < runningPods[j].Name
	})

	discoverHosts := "#!/bin/sh"
	if isGPULauncher {
		discoverHosts = fmt.Sprintf("%s\necho %s%s:%d\n", discoverHosts, mpiJob.Name, launcherSuffix, slots)
	}
	for _, p := range runningPods {
		discoverHosts = fmt.Sprintf("%s\necho %s:%d", discoverHosts, p.Name, slots)
	}

	oldDiscoverHosts, exist := configMap.Data[discoverHostsScriptName]
	if exist {
		if oldDiscoverHosts == discoverHosts {
			return
		}
	}
	configMap.Data[discoverHostsScriptName] = discoverHosts
}

// newLauncherServiceAccount creates a new launcher ServiceAccount for an MPIJob
// resource. It also sets the appropriate OwnerReferences on the resource so
// handleObject can discover the MPIJob resource that 'owns' it.
func newLauncherServiceAccount(mpiJob *mpiv1.MPIJob) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mpiJob.Name + launcherSuffix,
			Namespace: mpiJob.Namespace,
			Labels: map[string]string{
				"app": mpiJob.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(mpiJob, mpiv1.SchemeGroupVersionKind),
			},
		},
	}
}

// newLauncherRole creates a new launcher Role for an MPIJob resource. It also
// sets the appropriate OwnerReferences on the resource so handleObject can
// discover the MPIJob resource that 'owns' it.
func newLauncherRole(mpiJob *mpiv1.MPIJob, workerReplicas int32) *rbacv1.Role {
	var podNames []string
	for i := 0; i < int(workerReplicas); i++ {
		podNames = append(podNames, fmt.Sprintf("%s%s-%d", mpiJob.Name, workerSuffix, i))
	}
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mpiJob.Name + launcherSuffix,
			Namespace: mpiJob.Namespace,
			Labels: map[string]string{
				"app": mpiJob.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(mpiJob, mpiv1.SchemeGroupVersionKind),
			},
		},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:     []string{"get", "list", "watch"},
				APIGroups: []string{""},
				Resources: []string{"pods"},
			},
			{
				Verbs:         []string{"create"},
				APIGroups:     []string{""},
				Resources:     []string{"pods/exec"},
				ResourceNames: podNames,
			},
		},
	}
}

// newLauncherRoleBinding creates a new launcher RoleBinding for an MPIJob
// resource. It also sets the appropriate OwnerReferences on the resource so
// handleObject can discover the MPIJob resource that 'owns' it.
func newLauncherRoleBinding(mpiJob *mpiv1.MPIJob) *rbacv1.RoleBinding {
	launcherName := mpiJob.Name + launcherSuffix
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      launcherName,
			Namespace: mpiJob.Namespace,
			Labels: map[string]string{
				"app": mpiJob.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(mpiJob, mpiv1.SchemeGroupVersionKind),
			},
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      launcherName,
				Namespace: mpiJob.Namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     launcherName,
		},
	}
}

func setRestartPolicy(podTemplateSpec *corev1.PodTemplateSpec, spec *commonv1.ReplicaSpec) {
	if spec.RestartPolicy == commonv1.RestartPolicyExitCode {
		podTemplateSpec.Spec.RestartPolicy = corev1.RestartPolicyNever
	} else {
		podTemplateSpec.Spec.RestartPolicy = corev1.RestartPolicy(spec.RestartPolicy)
	}
}