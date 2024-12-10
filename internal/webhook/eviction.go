package webhook

import (
	"context"
	"net/http"

	pdbautoscaler "github.com/paulgmiller/k8s-pdb-autoscaler/api/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type EvictionHandler struct {
	Client  client.Client
	decoder *admission.Decoder
}

// this webhook updates the pdbwatcher's spec if there is a newish (configurable) eviction to cause a reconcile and see if we need to scale up
func (e *EvictionHandler) Handle(ctx context.Context, req admission.Request) admission.Response {

	logger := log.FromContext(ctx)

	logger.Info("Received eviction request", "namespace", req.Namespace, "podname", req.Name)

	currentEviction := pdbautoscaler.Eviction{
		PodName:      req.Name,
		EvictionTime: metav1.Now(),
	}

	// Fetch the pod to get its labels
	pod := &corev1.Pod{}
	err := e.Client.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, pod)
	if err != nil {
		logger.Error(err, "Error: Unable to fetch Pod")
		return admission.Errored(http.StatusInternalServerError, err)
	}

	podObj := pod.DeepCopy()
	updatePodCondition(&podObj.Status, &v1.PodCondition{
		Type:    v1.DisruptionTarget,
		Status:  v1.ConditionTrue,
		Reason:  "EvictionAttempt",
		Message: "eviction attempt recorded by eviction webhook",
	})

	// List all PDBWatchers in the namespace. Is this expensive for every eviction are we cacching this list and pdbs?
	pdbWatcherList := &pdbautoscaler.PDBWatcherList{}
	err = e.Client.List(ctx, pdbWatcherList, &client.ListOptions{Namespace: req.Namespace})
	if err != nil {
		logger.Error(err, "Error: Unable to list PDBWatchers")
		return admission.Errored(http.StatusInternalServerError, err)
	}

	// Find the applicable PDBWatcher
	var applicablePDBWatcher *pdbautoscaler.PDBWatcher
	for _, pdbWatcher := range pdbWatcherList.Items {
		pdbWatcher := pdbWatcher
		// Fetch the associated PDB
		pdb := &policyv1.PodDisruptionBudget{}
		err := e.Client.Get(ctx, types.NamespacedName{Name: pdbWatcher.Name, Namespace: pdbWatcher.Namespace}, pdb)
		if err != nil {
			logger.Error(err, "Error: Unable to fetch PDB:", "pdbname", pdbWatcher.Name)
			return admission.Errored(http.StatusInternalServerError, err)
		}

		// Check if the PDB selector matches the evicted pod's labels
		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			logger.Error(err, "Error: Invalid PDB selector", "pdbname", pdbWatcher.Name)
			continue
		}

		if selector.Matches(labels.Set(pod.Labels)) {
			applicablePDBWatcher = &pdbWatcher
			break
		}
	}

	if applicablePDBWatcher == nil {
		logger.Info("No applicable PDBWatcher found")
		return admission.Allowed("no applicable PDBWatcher")
	}

	logger.Info("Found pdbwatcher", "name", applicablePDBWatcher.Name)

	/* Can't do this because we might miss our last eviction
	//only update if we're 1 minute since last eviction to avoid swarms.
	//impolite to mutex here as it would block api server. Could have a single channel and channel read updates

	if applicablePDBWatcher.Spec.LastEviction.EvictionTime != "" {
		evictionTime, err := time.Parse(time.RFC3339, applicablePDBWatcher.Spec.LastEviction.EvictionTime)
		if err != nil {
			logger.Error(err, "Failed to parse eviction time "+applicablePDBWatcher.Spec.LastEviction.EvictionTime)
		} else {
			if now.Sub(evictionTime) < time.Minute { //mak configurable in CRD
				logger.Info("Eviction logged successfully", "podName", req.Name, "evictionTime", currentEviction.EvictionTime)
				return admission.Allowed("eviction ignored")
			}
		}
	}
	*/

	applicablePDBWatcher.Spec.LastEviction = currentEviction

	err = e.Client.Update(ctx, applicablePDBWatcher)
	if err != nil {
		//handle conflicts when many evictions happen in parallel? or doesn't matter if we lose the conficts
		logger.Error(err, "Unable to update PDBWatcher status")
		return admission.Errored(http.StatusInternalServerError, err) //Is this a problem if webhook doesn't ignore failures?
	}

	logger.Info("Eviction logged successfully", "podName", req.Name, "evictionTime", currentEviction.EvictionTime)
	return admission.Allowed("eviction allowed")
}

// what the heck does this do
func (e *EvictionHandler) InjectDecoder(d *admission.Decoder) error {
	e.decoder = d
	return nil
}

func updatePodCondition(status *v1.PodStatus, condition *v1.PodCondition) bool {
	condition.LastTransitionTime = metav1.Now()
	// Try to find this pod condition.
	conditionIndex, oldCondition := getPodCondition(status, condition.Type)

	if oldCondition == nil {
		// We are adding new pod condition.
		status.Conditions = append(status.Conditions, *condition)
		return true
	}
	// We are updating an existing condition, so we need to check if it has changed.
	if condition.Status == oldCondition.Status {
		condition.LastTransitionTime = oldCondition.LastTransitionTime
	}

	isEqual := condition.Status == oldCondition.Status &&
		condition.Reason == oldCondition.Reason &&
		condition.Message == oldCondition.Message &&
		condition.LastProbeTime.Equal(&oldCondition.LastProbeTime) &&
		condition.LastTransitionTime.Equal(&oldCondition.LastTransitionTime)

	status.Conditions[conditionIndex] = *condition
	// Return true if one of the fields have changed.
	return !isEqual
}

func getPodCondition(status *v1.PodStatus, conditionType v1.PodConditionType) (int, *v1.PodCondition) {
	if status == nil {
		return -1, nil
	}
	return getPodConditionFromList(status.Conditions, conditionType)
}

// GetPodConditionFromList extracts the provided condition from the given list of condition and
// returns the index of the condition and the condition. Returns -1 and nil if the condition is not present.
func getPodConditionFromList(conditions []v1.PodCondition, conditionType v1.PodConditionType) (int, *v1.PodCondition) {
	if conditions == nil {
		return -1, nil
	}
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return i, &conditions[i]
		}
	}
	return -1, nil
}
