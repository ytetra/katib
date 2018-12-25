/*

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

package studyjob

import (
	"context"
	"log"
	"strconv"
	"sync"

	"github.com/kubeflow/katib/pkg"
	katibapi "github.com/kubeflow/katib/pkg/api"
	katibv1alpha1 "github.com/kubeflow/katib/pkg/api/operators/apis/studyjob/v1alpha1"
	pytorchjobv1beta1 "github.com/kubeflow/pytorch-operator/pkg/apis/pytorch/v1beta1"
	commonv1beta1 "github.com/kubeflow/tf-operator/pkg/apis/common/v1beta1"
	tfjobv1beta1 "github.com/kubeflow/tf-operator/pkg/apis/tensorflow/v1beta1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	batchv1 "k8s.io/api/batch/v1"
	batchv1beta "k8s.io/api/batch/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const maxMsgSize = 1<<31 - 1

/**
* USER ACTION REQUIRED: This is a scaffold file intended for the user to modify with their own Controller
* business logic.  Delete these comments after modifying this file.*
 */

// Add creates a new StudyJobController Controller and adds it to the Manager with default RBAC. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
// USER ACTION REQUIRED: update cmd/manager/main.go to call this studyjob.Add(mgr) to install this Controller
func Add(mgr manager.Manager) error {
	r, err := newReconciler(mgr)
	if err != nil {
		return err
	}
	return add(mgr, r)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) (reconcile.Reconciler, error) {
	pc, err := NewPodControl()
	if err != nil {
		return nil, err
	}
	return &ReconcileStudyJobController{Client: mgr.GetClient(), scheme: mgr.GetScheme(), muxMap: sync.Map{}, podControl: pc}, nil
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("studyjob-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to StudyJobController
	err = c.Watch(&source.Kind{Type: &katibv1alpha1.StudyJob{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	err = c.Watch(
		&source.Kind{Type: &batchv1.Job{}},
		&handler.EnqueueRequestForOwner{
			IsController: true,
			OwnerType:    &katibv1alpha1.StudyJob{},
		})
	if err != nil {
		return err
	}

	err = c.Watch(
		&source.Kind{Type: &batchv1beta.CronJob{}},
		&handler.EnqueueRequestForOwner{
			IsController: true,
			OwnerType:    &katibv1alpha1.StudyJob{},
		})
	if err != nil {
		return err
	}

	err = c.Watch(
		&source.Kind{Type: &tfjobv1beta1.TFJob{}},
		&handler.EnqueueRequestForOwner{
			IsController: true,
			OwnerType:    &katibv1alpha1.StudyJob{},
		})
	if err != nil {
		return err
	}

	err = c.Watch(
		&source.Kind{Type: &pytorchjobv1beta1.PyTorchJob{}},
		&handler.EnqueueRequestForOwner{
			IsController: true,
			OwnerType:    &katibv1alpha1.StudyJob{},
		})
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileStudyJobController{}

// ReconcileStudyJobController reconciles a StudyJob object
type ReconcileStudyJobController struct {
	client.Client
	scheme     *runtime.Scheme
	muxMap     sync.Map
	podControl *PodControl
}

type WorkerStatus struct {
	// +optional
	CompletionTime *metav1.Time
	WorkerState    katibapi.State
}

// Reconcile reads that state of the cluster for a StudyJob object and makes changes based on the state read
// and what is in the StudyJob.Spec
// a Deployment as an example
// Automatically generate RBAC rules to allow the Controller to read and write Deployments
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=studyjob.kubeflow.org,resources=studyjob,verbs=get;list;watch;create;update;patch;delete
func (r *ReconcileStudyJobController) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	// Fetch the StudyJob instance
	instance := &katibv1alpha1.StudyJob{}
	mux := new(sync.Mutex)
	if m, loaded := r.muxMap.LoadOrStore(request.NamespacedName.String(), mux); loaded {
		mux, _ = m.(*sync.Mutex)
	}
	mux.Lock()
	defer mux.Unlock()

	var firstTime bool = true
	for {
		err := r.Get(context.TODO(), request.NamespacedName, instance)
		if err != nil {
			if errors.IsNotFound(err) {
				if _, ok := r.muxMap.Load(request.NamespacedName.String()); ok {
					log.Printf("Study %s was deleted. Resouces will be released.", request.NamespacedName.String())
					r.muxMap.Delete(request.NamespacedName.String())
				}
				// Object not found, return.  Created objects are automatically garbage collected.
				// For additional cleanup logic use finalizers.
				return reconcile.Result{}, nil
			}
			log.Printf("Fail to read Object %v", err)
			// Error reading the object - requeue the request.
			return reconcile.Result{}, err
		}

		var update bool = false
		switch instance.Status.Condition {
		case katibv1alpha1.ConditionCompleted,
		     katibv1alpha1.ConditionFailed,
		     katibv1alpha1.ConditionRunning:
			update, err = r.checkStatus(instance, request.Namespace, firstTime)
		default:
			now := metav1.Now()
			instance.Status.StartTime = &now
			err = initializeStudy(instance, request.Namespace)
			if err != nil {
				r.Update(context.TODO(), instance)
				log.Printf("Fail to initialize %v", err)
				return reconcile.Result{}, err
			}
			update = true
		}
		now := metav1.Now()
		instance.Status.LastReconcileTime = &now
		if err != nil {
			r.Update(context.TODO(), instance)
			log.Printf("Fail to check status %v", err)
			return reconcile.Result{}, err
		}
		if update {
			err = r.Update(context.TODO(), instance)
			if err != nil {
				log.Printf("Fail to Update StudyJob %v : %v", instance.Status.StudyID, err)
				firstTime = false
			} else {
				break
			}
		} else {
			break
		}
	}
	return reconcile.Result{}, nil
}

func (r *ReconcileStudyJobController) checkGoal(instance *katibv1alpha1.StudyJob, c katibapi.ManagerClient, wids []string) (bool, error) {
	if instance.Spec.OptimizationGoal == nil {
		return false, nil
	}
	getMetricsRequest := &katibapi.GetMetricsRequest{
		StudyId:   instance.Status.StudyID,
		WorkerIds: wids,
	}
	mr, err := c.GetMetrics(context.Background(), getMetricsRequest)
	if err != nil {
		errStatus, _ := status.FromError(err)
		if errStatus.Code() == codes.ResourceExhausted {
			//GRPC Message is probably too long, get metrics one by one
			var mls []*katibapi.MetricsLogSet
			for _, wid := range wids {
				getMetricsRequest := &katibapi.GetMetricsRequest{
					StudyId:   instance.Status.StudyID,
					WorkerIds: []string{wid},
				}
				cmr, err := c.GetMetrics(context.Background(), getMetricsRequest)
				if err != nil {
					return false, err
				}
				mls = append(mls, cmr.MetricsLogSets...)
			}
			mr.MetricsLogSets = mls
		} else {
			//Unknown error
			return false, err
		}
	}
	goal := false
	for _, mls := range mr.MetricsLogSets {
		for _, ml := range mls.MetricsLogs {
			if ml.Name == instance.Spec.ObjectiveValueName {
				if len(ml.Values) > 0 {
					curValue, _ := strconv.ParseFloat(ml.Values[len(ml.Values)-1].Value, 32)
					if instance.Spec.OptimizationType == katibv1alpha1.OptimizationTypeMinimize {
						if curValue < *instance.Spec.OptimizationGoal {
							goal = true
						}
						if instance.Status.BestObjectiveValue != nil {
							if *instance.Status.BestObjectiveValue > curValue {
								instance.Status.BestObjectiveValue = &curValue
							}
						} else {
							instance.Status.BestObjectiveValue = &curValue
						}
						for i := range instance.Status.Trials {
							for j := range instance.Status.Trials[i].WorkerList {
								if instance.Status.Trials[i].WorkerList[j].WorkerID == mls.WorkerId {
									instance.Status.Trials[i].WorkerList[j].ObjectiveValue = &curValue
								}
							}
						}
					} else if instance.Spec.OptimizationType == katibv1alpha1.OptimizationTypeMaximize {
						if curValue > *instance.Spec.OptimizationGoal {
							goal = true
						}
						if instance.Status.BestObjectiveValue != nil {
							if *instance.Status.BestObjectiveValue < curValue {
								instance.Status.BestObjectiveValue = &curValue
							}
						} else {
							instance.Status.BestObjectiveValue = &curValue
						}
						for i := range instance.Status.Trials {
							for j := range instance.Status.Trials[i].WorkerList {
								if instance.Status.Trials[i].WorkerList[j].WorkerID == mls.WorkerId {
									instance.Status.Trials[i].WorkerList[j].ObjectiveValue = &curValue
								}
							}
						}
					}
				}
				break
			}
		}
	}
	return goal, nil
}

func (r *ReconcileStudyJobController) deleteWorkerResources(instance *katibv1alpha1.StudyJob, ns string, w *katibv1alpha1.WorkerCondition) error {
	wid := w.WorkerID
	obj := createWorkerJobObj(w.Kind)
	nname := types.NamespacedName{Namespace: ns, Name: wid}
	var wretain, mcretain bool = false, false
	if instance.Spec.WorkerSpec != nil {
		wretain = instance.Spec.WorkerSpec.Retain
	}
	if !wretain {
		joberr := r.Client.Get(context.TODO(), nname, obj)
		if joberr == nil {
			if err := r.Delete(context.TODO(), obj); err != nil {
				return err
			}
			// In order to integrate with tf-operator and pytorch-operator, we need to
			// downgrade the k8s dependency for katib from 1.11.2 to 1.10.1, and
			// controller-runtime from 0.1.3 to 0.1.1. This means that we cannot use
			// DeletePropagationForeground to clean up pods, and must do this manually.
			if err := r.podControl.DeletePodsForWorker(ns, wid); err != nil {
				return err
			}
		}
	}
	if instance.Spec.MetricsCollectorSpec != nil {
		mcretain = instance.Spec.MetricsCollectorSpec.Retain
	}
	if !mcretain {
		cjob := &batchv1beta.CronJob{}
		cjoberr := r.Client.Get(context.TODO(), nname, cjob)
		if cjoberr == nil {
			if err := r.Delete(context.TODO(), cjob); err != nil {
				return err
			}
			// Depending on the successfulJobsHistoryLimit setting, cronjob controller
			// will delete the metrics collector pods accordingly, so we do not need
			// to manually delete them here.
		}
	}
	return nil
}

func (r *ReconcileStudyJobController) updateWorker(c katibapi.ManagerClient, instance *katibv1alpha1.StudyJob, status WorkerStatus, ns string, cwids []string, i int, j int) (bool, error) {
	var update bool = false
	wid := instance.Status.Trials[i].WorkerList[j].WorkerID
	nname := types.NamespacedName{Namespace: ns, Name: wid}
	cjob := &batchv1beta.CronJob{}
	cjoberr := r.Client.Get(context.TODO(), nname, cjob)
	switch status.WorkerState {
	case katibapi.State_COMPLETED:
		ctime := status.CompletionTime
		if cjoberr == nil {
			if ctime != nil && cjob.Status.LastScheduleTime != nil {
				if ctime.Before(cjob.Status.LastScheduleTime) && len(cjob.Status.Active) == 0 {
					saveModel(c, instance.Status.StudyID, instance.Status.Trials[i].TrialID, wid)
					update = true
					susp := true
					cjob.Spec.Suspend = &susp
					if err := r.Update(context.TODO(), cjob); err != nil {
						return false, err
					}
				}
			}
		} else {
		// for some reason, metricsCollector for this worker cannot be found (deleted by anyone accidentally or even failed to be created)
			update = true
			instance.Status.Condition = katibv1alpha1.ConditionFailed
		}
		if update {
			instance.Status.Trials[i].WorkerList[j].Condition = katibv1alpha1.ConditionCompleted
			instance.Status.Trials[i].WorkerList[j].CompletionTime = metav1.Now()
			cwids = append(cwids, wid)
		}
	case katibapi.State_RUNNING:
		if instance.Status.Trials[i].WorkerList[j].Condition != katibv1alpha1.ConditionRunning {
			instance.Status.Trials[i].WorkerList[j].Condition = katibv1alpha1.ConditionRunning
			update = true
		}
		if errors.IsNotFound(cjoberr) {
			spawnErr := r.spawnMetricsCollector(instance, c, instance.Status.StudyID, instance.Status.Trials[i].TrialID, wid, ns, instance.Spec.MetricsCollectorSpec)
			if spawnErr != nil {
				instance.Status.Condition = katibv1alpha1.ConditionFailed
			}
		}
	case katibapi.State_ERROR:
		if instance.Status.Trials[i].WorkerList[j].Condition != katibv1alpha1.ConditionFailed {
			instance.Status.Trials[i].WorkerList[j].Condition = katibv1alpha1.ConditionFailed
			update = true
		}
	}
	if update {
		_, err := c.UpdateWorkerState(
			context.Background(),
			&katibapi.UpdateWorkerStateRequest{
				WorkerId: instance.Status.Trials[i].WorkerList[j].WorkerID,
				Status: status.WorkerState,
			})
		if err != nil {
			log.Printf("Fail to update worker info. ID %s", instance.Status.Trials[i].WorkerList[j].WorkerID)
			return false, err
		}
	}
	return update, nil
}

func(r *ReconcileStudyJobController) getJobWorkerStatus(w *katibv1alpha1.WorkerCondition, ns string) WorkerStatus {
	runtimejob := createWorkerJobObj(w.Kind)
	nname := types.NamespacedName{Namespace: ns, Name: w.WorkerID}
	joberr := r.Client.Get(context.TODO(), nname, runtimejob)
	if joberr != nil {
		return WorkerStatus{}
	}
	var state katibapi.State = katibapi.State_RUNNING
	var cpTime *metav1.Time
	switch w.Kind {
	case DefaultJobWorker:
		job := runtimejob.(* batchv1.Job)
		if job.Status.Active == 0 && job.Status.Succeeded > 0 {
			state = katibapi.State_COMPLETED
		} else if job.Status.Failed > 0 {
			state = katibapi.State_ERROR
		}
		cpTime = job.Status.CompletionTime
	case TFJobWorker:
		job := runtimejob.(* tfjobv1beta1.TFJob)
		if len(job.Status.Conditions) > 0 {
			lc := job.Status.Conditions[len(job.Status.Conditions)-1]
			if lc.Type == commonv1beta1.JobSucceeded {
				state = katibapi.State_COMPLETED
			} else if lc.Type == commonv1beta1.JobFailed {
				state = katibapi.State_ERROR
			}
		}
		cpTime = job.Status.CompletionTime
	case PyTorchJobWorker:
		job := runtimejob.(* pytorchjobv1beta1.PyTorchJob)
		if len(job.Status.Conditions) > 0 {
			lc := job.Status.Conditions[len(job.Status.Conditions)-1]
			if lc.Type == commonv1beta1.JobSucceeded {
				state = katibapi.State_COMPLETED
			} else if lc.Type == commonv1beta1.JobFailed {
				state = katibapi.State_ERROR
			}
		}
		cpTime = job.Status.CompletionTime
	}
	return WorkerStatus{
			CompletionTime: cpTime,
			WorkerState:    state,
	}
}

func (r *ReconcileStudyJobController) checkStatus(instance *katibv1alpha1.StudyJob, ns string, firstTime bool) (bool, error) {
	nextSuggestionSchedule := true
	var cwids []string
	var update bool = false
	if instance.Status.Condition == katibv1alpha1.ConditionCompleted || instance.Status.Condition == katibv1alpha1.ConditionFailed {
		nextSuggestionSchedule = false
	}
	opts := []grpc.DialOption{
		grpc.WithInsecure(),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMsgSize)),
	}
	conn, err := grpc.Dial(pkg.ManagerAddr, opts...)
	if err != nil {
		log.Printf("Connect katib manager error %v", err)
		instance.Status.Condition = katibv1alpha1.ConditionFailed
		return true, nil
	}
	defer conn.Close()
	c := katibapi.NewManagerClient(conn)
	for i, t := range instance.Status.Trials {
		for j, w := range t.WorkerList {
			if w.Condition == katibv1alpha1.ConditionCompleted || w.Condition == katibv1alpha1.ConditionFailed {
				if w.ObjectiveValue == nil && w.Condition == katibv1alpha1.ConditionCompleted {
					cwids = append(cwids, w.WorkerID)
				}
				if firstTime {
					if err := r.deleteWorkerResources(instance, ns, &w); err != nil {
						return false, err
					}
				}
				continue
			}
			nextSuggestionSchedule = false
			js := r.getJobWorkerStatus(&w, ns)
			update, err = r.updateWorker(c, instance, js, ns, cwids[0:], i, j)
		}
	}
	if len(cwids) > 0 {
		goal, err := r.checkGoal(instance, c, cwids)
		if goal {
			log.Printf("Study %s reached to the goal. It is completed", instance.Status.StudyID)
			instance.Status.Condition = katibv1alpha1.ConditionCompleted
			now := metav1.Now()
			instance.Status.CompletionTime = &now
			update = true
			nextSuggestionSchedule = false
		}
		if err != nil {
			log.Printf("Check Goal failed %v", err)
		}
	}
	if nextSuggestionSchedule {
		if instance.Spec.RequestCount > 0 && instance.Status.SuggestionCount >= instance.Spec.RequestCount {
			log.Printf("Study %s reached the request count. It is completed", instance.Status.StudyID)
			instance.Status.Condition = katibv1alpha1.ConditionCompleted
			now := metav1.Now()
			instance.Status.CompletionTime = &now
			return true, nil
		}
		if firstTime {
			return r.getAndRunSuggestion(instance, c, ns)
		}
	} else {
		return update, nil
	}
}

func (r *ReconcileStudyJobController) getAndRunSuggestion(instance *katibv1alpha1.StudyJob, c katibapi.ManagerClient, ns string) (bool, error) {
	//Check Suggestion Count
	sps, err := getSuggestionParam(c, instance.Status.SuggestionParameterID)
	if err != nil {
		return false, err
	}
	for i := range sps {
		if sps[i].Name == "SuggestionCount" {
			count, _ := strconv.Atoi(sps[i].Value)
			if count >= instance.Status.SuggestionCount+1 {
				//Suggestion count mismatched. May be duplicate suggestion request
				return false, nil
			}
			sps[i].Value = strconv.Itoa(instance.Status.SuggestionCount + 1)
		}
	}
	//GetSuggestion
	getSuggestReply, err := getSuggestion(
		c,
		instance.Status.StudyID,
		instance.Spec.SuggestionSpec,
		instance.Status.SuggestionParameterID)
	if err != nil {
		instance.Status.Condition = katibv1alpha1.ConditionFailed
		return true, err
	}
	trials := getSuggestReply.Trials
	if len(trials) <= 0 {
		log.Printf("Study %s is completed", instance.Status.StudyID)
		instance.Status.Condition = katibv1alpha1.ConditionCompleted
		now := metav1.Now()
		instance.Status.CompletionTime = &now
		return true, nil
	}
	log.Printf("Study: %s Suggestions %v", instance.Status.StudyID, getSuggestReply)
	wkind, err := getWorkerKind(instance.Spec.WorkerSpec)
	if err != nil {
		log.Printf("getWorkerKind error %v", err)
		instance.Status.Condition = katibv1alpha1.ConditionFailed
		return true, err
	}
	for _, t := range trials {
		wid, err := r.spawnWorker(instance, c, instance.Status.StudyID, t, instance.Spec.WorkerSpec, wkind, false)
		if err != nil {
			log.Printf("Spawn worker error %v", err)
			instance.Status.Condition = katibv1alpha1.ConditionFailed
			return true, err
		}
		instance.Status.Trials = append(
			instance.Status.Trials,
			katibv1alpha1.TrialSet{
				TrialID: t.TrialId,
				WorkerList: []katibv1alpha1.WorkerCondition{
					katibv1alpha1.WorkerCondition{
						WorkerID:  wid,
						Kind:      wkind,
						Condition: katibv1alpha1.ConditionCreated,
						StartTime: metav1.Now(),
					},
				},
			},
		)
	}
	//Update Suggestion Count
	sspr := &katibapi.SetSuggestionParametersRequest{
		StudyId:              instance.Status.StudyID,
		SuggestionAlgorithm:  instance.Spec.SuggestionSpec.SuggestionAlgorithm,
		ParamId:              instance.Status.SuggestionParameterID,
		SuggestionParameters: sps,
	}
	_, err = c.SetSuggestionParameters(context.Background(), sspr)
	if err != nil {
		log.Printf("Study %s Suggestion Count update Error %v", instance.Status.StudyID, err)
		return false, err
	}
	instance.Status.SuggestionCount += 1
	return true, nil
}

type WorkerInstance struct {
	StudyID         string
	TrialID         string
	WorkerID        string
	NameSpace       string
	HyperParameters []*katibapi.Parameter
}

func (r *ReconcileStudyJobController) spawnWorker(instance *katibv1alpha1.StudyJob, c katibapi.ManagerClient, studyID string, trial *katibapi.Trial, workerSpec *katibv1alpha1.WorkerSpec, wkind string, dryrun bool) (string, error) {
	wid, wm, err := getWorkerManifest(c, studyID, trial, workerSpec, wkind, instance.Namespace, false)
	if err != nil {
		return "", err
	}
	BUFSIZE := 1024
	job := createWorkerJobObj(wkind)
	if err := k8syaml.NewYAMLOrJSONDecoder(wm, BUFSIZE).Decode(job); err != nil {
		log.Printf("Yaml decode error %v", err)
		return "", err
	}
	if err := controllerutil.SetControllerReference(instance, job.(metav1.Object), r.scheme); err != nil {
		log.Printf("SetControllerReference error %v", err)
		return "", err
	}
	if err := r.Create(context.TODO(), job); err != nil {
		log.Printf("Job Create error %v", err)
		return "", err
	}

	return wid, nil
}

func (r *ReconcileStudyJobController) spawnMetricsCollector(instance *katibv1alpha1.StudyJob, c katibapi.ManagerClient, studyID string, trialID string, workerID string, namespace string, mcs *katibv1alpha1.MetricsCollectorSpec) error {
	var mcjob batchv1beta.CronJob
	BUFSIZE := 1024
	wkind, err := getWorkerKind(instance.Spec.WorkerSpec)
	if err != nil {
		log.Printf("getWorkerKind error %v", err)
		return err
	}
	mcm, err := getMetricsCollectorManifest(studyID, trialID, workerID, wkind, namespace, mcs)
	if err != nil {
		log.Printf("getMetricsCollectorManifest error %v", err)
		return err
	}

	if err := k8syaml.NewYAMLOrJSONDecoder(mcm, BUFSIZE).Decode(&mcjob); err != nil {
		log.Printf("MetricsCollector Yaml decode error %v", err)
		return err
	}

	if err := controllerutil.SetControllerReference(instance, &mcjob, r.scheme); err != nil {
		log.Printf("MetricsCollector SetControllerReference error %v", err)
		return err
	}

	if err := r.Create(context.TODO(), &mcjob); err != nil {
		log.Printf("MetricsCollector Job Create error %v", err)
		return err
	}
	return nil
}
