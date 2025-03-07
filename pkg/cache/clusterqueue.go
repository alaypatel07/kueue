/*
Copyright 2023 The Kubernetes Authors.

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

package cache

import (
	"errors"
	"math"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta1"
	"sigs.k8s.io/kueue/pkg/hierarchy"
	"sigs.k8s.io/kueue/pkg/metrics"
	"sigs.k8s.io/kueue/pkg/resources"
	utilac "sigs.k8s.io/kueue/pkg/util/admissioncheck"
	"sigs.k8s.io/kueue/pkg/workload"
)

var (
	errQueueAlreadyExists = errors.New("queue already exists")
	oneQuantity           = resource.MustParse("1")
)

// clusterQueue is the internal implementation of kueue.clusterQueue that
// holds admitted workloads.
type clusterQueue struct {
	Name              string
	ResourceGroups    []ResourceGroup
	Workloads         map[string]*workload.Info
	WorkloadsNotReady sets.Set[string]
	NamespaceSelector labels.Selector
	Preemption        kueue.ClusterQueuePreemption
	FairWeight        resource.Quantity
	FlavorFungibility kueue.FlavorFungibility
	// Aggregates AdmissionChecks from both .spec.AdmissionChecks and .spec.AdmissionCheckStrategy
	// Sets hold ResourceFlavors to which an AdmissionCheck should apply.
	// In case its empty, it means an AdmissionCheck should apply to all ResourceFlavor
	AdmissionChecks map[string]sets.Set[kueue.ResourceFlavorReference]
	Status          metrics.ClusterQueueStatus
	// AllocatableResourceGeneration will be increased when some admitted workloads are
	// deleted, or the resource groups are changed.
	AllocatableResourceGeneration int64

	AdmittedUsage resources.FlavorResourceQuantities
	// localQueues by (namespace/name).
	localQueues                                        map[string]*queue
	podsReadyTracking                                  bool
	hasMissingFlavors                                  bool
	hasMissingOrInactiveAdmissionChecks                bool
	hasMultipleSingleInstanceControllersChecks         bool
	hasFlavorIndependentAdmissionCheckAppliedPerFlavor bool
	admittedWorkloadsCount                             int
	isStopped                                          bool
	workloadInfoOptions                                []workload.InfoOption

	resourceNode ResourceNode
	hierarchy.ClusterQueue[*cohort]
}

func (c *clusterQueue) GetName() string {
	return c.Name
}

// implement dominantResourceShareNode interface

func (c *clusterQueue) parentResources() ResourceNode {
	return c.Parent().resourceNode
}

// implements hierarchicalResourceNode interface.

func (c *clusterQueue) getResourceNode() ResourceNode {
	return c.resourceNode
}

func (c *clusterQueue) parentHRN() hierarchicalResourceNode {
	return c.Parent()
}

type queue struct {
	key                string
	reservingWorkloads int
	admittedWorkloads  int
	//TODO: rename this to better distinguish between reserved and "in use" quantities
	usage         resources.FlavorResourceQuantities
	admittedUsage resources.FlavorResourceQuantities
}

// FitInCohort supports the legacy
// features.MultiplePreemptions=false path. It doesn't take into
// account BorrowingLimits. To be cleaned up in v0.10, when we delete
// the old code.
func (c *ClusterQueueSnapshot) FitInCohort(q resources.FlavorResourceQuantities) bool {
	for fr, value := range q {
		if available(c, fr, false) < value {
			return false
		}
	}
	return true
}

func (c *clusterQueue) Active() bool {
	return c.Status == active
}

var defaultPreemption = kueue.ClusterQueuePreemption{
	ReclaimWithinCohort: kueue.PreemptionPolicyNever,
	WithinClusterQueue:  kueue.PreemptionPolicyNever,
}

var defaultFlavorFungibility = kueue.FlavorFungibility{WhenCanBorrow: kueue.Borrow, WhenCanPreempt: kueue.TryNextFlavor}

func (c *clusterQueue) updateClusterQueue(in *kueue.ClusterQueue, resourceFlavors map[kueue.ResourceFlavorReference]*kueue.ResourceFlavor, admissionChecks map[string]AdmissionCheck, oldParent *cohort) error {
	if c.updateQuotasAndResourceGroups(in.Spec.ResourceGroups) || oldParent != c.Parent() {
		c.AllocatableResourceGeneration += 1
		if oldParent != nil && oldParent != c.Parent() {
			updateCohortResourceNode(oldParent)
		}
		if c.HasParent() {
			// clusterQueue will be updated as part of tree update.
			updateCohortResourceNode(c.Parent())
		} else {
			// since ClusterQueue has no parent, it won't be updated
			// as part of tree update.
			updateClusterQueueResourceNode(c)
		}
	}

	nsSelector, err := metav1.LabelSelectorAsSelector(in.Spec.NamespaceSelector)
	if err != nil {
		return err
	}
	c.NamespaceSelector = nsSelector

	c.isStopped = ptr.Deref(in.Spec.StopPolicy, kueue.None) != kueue.None

	c.AdmissionChecks = utilac.NewAdmissionChecks(in)

	c.UpdateWithFlavors(resourceFlavors)
	c.updateWithAdmissionChecks(admissionChecks)

	if in.Spec.Preemption != nil {
		c.Preemption = *in.Spec.Preemption
	} else {
		c.Preemption = defaultPreemption
	}

	if in.Spec.FlavorFungibility != nil {
		c.FlavorFungibility = *in.Spec.FlavorFungibility
		if c.FlavorFungibility.WhenCanBorrow == "" {
			c.FlavorFungibility.WhenCanBorrow = defaultFlavorFungibility.WhenCanBorrow
		}
		if c.FlavorFungibility.WhenCanPreempt == "" {
			c.FlavorFungibility.WhenCanPreempt = defaultFlavorFungibility.WhenCanPreempt
		}
	} else {
		c.FlavorFungibility = defaultFlavorFungibility
	}

	c.FairWeight = oneQuantity
	if fs := in.Spec.FairSharing; fs != nil && fs.Weight != nil {
		c.FairWeight = *fs.Weight
	}

	return nil
}

func createdResourceGroups(kueueRgs []kueue.ResourceGroup) []ResourceGroup {
	rgs := make([]ResourceGroup, len(kueueRgs))
	for i, kueueRg := range kueueRgs {
		rgs[i] = ResourceGroup{
			CoveredResources: sets.New(kueueRg.CoveredResources...),
			Flavors:          make([]kueue.ResourceFlavorReference, 0, len(kueueRg.Flavors)),
		}
		for _, fIn := range kueueRg.Flavors {
			rgs[i].Flavors = append(rgs[i].Flavors, fIn.Name)
		}
	}
	return rgs
}

// updateQuotasAndResourceGroups updates Quotas and ResourceGroups.
// It returns true if any changes were made.
func (c *clusterQueue) updateQuotasAndResourceGroups(in []kueue.ResourceGroup) bool {
	oldRG := c.ResourceGroups
	oldQuotas := c.resourceNode.Quotas
	c.ResourceGroups = createdResourceGroups(in)
	c.resourceNode.Quotas = createResourceQuotas(in)

	// Start at 1, for backwards compatibility.
	return c.AllocatableResourceGeneration == 0 ||
		!equality.Semantic.DeepEqual(oldRG, c.ResourceGroups) ||
		!equality.Semantic.DeepEqual(oldQuotas, c.resourceNode.Quotas)
}

func (c *clusterQueue) updateQueueStatus() {
	status := active
	if c.hasMissingFlavors || c.hasMissingOrInactiveAdmissionChecks || c.isStopped || c.hasMultipleSingleInstanceControllersChecks || c.hasFlavorIndependentAdmissionCheckAppliedPerFlavor {
		status = pending
	}
	if c.Status == terminating {
		status = terminating
	}
	if status != c.Status {
		c.Status = status
		metrics.ReportClusterQueueStatus(c.Name, c.Status)
	}
}

func (c *clusterQueue) inactiveReason() (string, string) {
	switch c.Status {
	case terminating:
		return "Terminating", "Can't admit new workloads; clusterQueue is terminating"
	case pending:
		reasons := make([]string, 0, 3)
		if c.isStopped {
			reasons = append(reasons, "Stopped")
		}
		if c.hasMissingFlavors {
			reasons = append(reasons, "FlavorNotFound")
		}
		if c.hasMissingOrInactiveAdmissionChecks {
			reasons = append(reasons, "CheckNotFoundOrInactive")
		}

		if c.hasMultipleSingleInstanceControllersChecks {
			reasons = append(reasons, "MultipleSingleInstanceControllerChecks")
		}

		if c.hasFlavorIndependentAdmissionCheckAppliedPerFlavor {
			reasons = append(reasons, "FlavorIndependentAdmissionCheckAppliedPerFlavor")
		}

		if len(reasons) == 0 {
			return "Unknown", "Can't admit new workloads."
		}

		return reasons[0], strings.Join([]string{"Can't admit new workloads:", strings.Join(reasons, ", ")}, " ")
	}
	return "Ready", "Can admit new flavors"
}

// UpdateWithFlavors updates a ClusterQueue based on the passed ResourceFlavors set.
// Exported only for testing.
func (c *clusterQueue) UpdateWithFlavors(flavors map[kueue.ResourceFlavorReference]*kueue.ResourceFlavor) {
	c.hasMissingFlavors = c.updateLabelKeys(flavors)
	c.updateQueueStatus()
}

func (c *clusterQueue) updateLabelKeys(flavors map[kueue.ResourceFlavorReference]*kueue.ResourceFlavor) bool {
	var flavorNotFound bool
	for i := range c.ResourceGroups {
		rg := &c.ResourceGroups[i]
		if len(rg.Flavors) == 0 {
			rg.LabelKeys = nil
			continue
		}
		keys := sets.New[string]()
		for _, fName := range rg.Flavors {
			if flv, exist := flavors[fName]; exist {
				for k := range flv.Spec.NodeLabels {
					keys.Insert(k)
				}
			} else {
				flavorNotFound = true
			}
		}

		if keys.Len() > 0 {
			rg.LabelKeys = keys
		}
	}

	return flavorNotFound
}

// updateWithAdmissionChecks updates a ClusterQueue based on the passed AdmissionChecks set.
func (c *clusterQueue) updateWithAdmissionChecks(checks map[string]AdmissionCheck) {
	hasMissing := false
	hasSpecificChecks := false
	checksPerController := make(map[string]int, len(c.AdmissionChecks))
	singleInstanceControllers := sets.New[string]()
	for acName, flavors := range c.AdmissionChecks {
		if ac, found := checks[acName]; !found {
			hasMissing = true
		} else {
			if !ac.Active {
				hasMissing = true
			}
			checksPerController[ac.Controller]++
			if ac.SingleInstanceInClusterQueue {
				singleInstanceControllers.Insert(ac.Controller)
			}
			if ac.FlavorIndependent && flavors.Len() != 0 {
				hasSpecificChecks = true
			}
		}
	}

	update := false
	if hasMissing != c.hasMissingOrInactiveAdmissionChecks {
		c.hasMissingOrInactiveAdmissionChecks = hasMissing
		update = true
	}

	hasMultipleSICC := false
	for controller, checks := range checksPerController {
		if singleInstanceControllers.Has(controller) && checks > 1 {
			hasMultipleSICC = true
		}
	}

	if c.hasMultipleSingleInstanceControllersChecks != hasMultipleSICC {
		c.hasMultipleSingleInstanceControllersChecks = hasMultipleSICC
		update = true
	}

	if c.hasFlavorIndependentAdmissionCheckAppliedPerFlavor != hasSpecificChecks {
		c.hasFlavorIndependentAdmissionCheckAppliedPerFlavor = hasSpecificChecks
		update = true
	}

	if update {
		c.updateQueueStatus()
	}
}

func (c *clusterQueue) addWorkload(w *kueue.Workload) error {
	k := workload.Key(w)
	if _, exist := c.Workloads[k]; exist {
		return errors.New("workload already exists in ClusterQueue")
	}
	wi := workload.NewInfo(w, c.workloadInfoOptions...)
	c.Workloads[k] = wi
	c.updateWorkloadUsage(wi, 1)
	if c.podsReadyTracking && !apimeta.IsStatusConditionTrue(w.Status.Conditions, kueue.WorkloadPodsReady) {
		c.WorkloadsNotReady.Insert(k)
	}
	c.reportActiveWorkloads()
	return nil
}

func (c *clusterQueue) deleteWorkload(w *kueue.Workload) {
	k := workload.Key(w)
	wi, exist := c.Workloads[k]
	if !exist {
		return
	}
	c.updateWorkloadUsage(wi, -1)
	if c.podsReadyTracking && !apimeta.IsStatusConditionTrue(w.Status.Conditions, kueue.WorkloadPodsReady) {
		c.WorkloadsNotReady.Delete(k)
	}
	// we only increase the AllocatableResourceGeneration cause the add of workload won't make more
	// workloads fit in ClusterQueue.
	c.AllocatableResourceGeneration++

	delete(c.Workloads, k)
	c.reportActiveWorkloads()
}

func (c *clusterQueue) reportActiveWorkloads() {
	metrics.AdmittedActiveWorkloads.WithLabelValues(c.Name).Set(float64(c.admittedWorkloadsCount))
	metrics.ReservingActiveWorkloads.WithLabelValues(c.Name).Set(float64(len(c.Workloads)))
}

// updateWorkloadUsage updates the usage of the ClusterQueue for the workload
// and the number of admitted workloads for local queues.
func (c *clusterQueue) updateWorkloadUsage(wi *workload.Info, m int64) {
	admitted := workload.IsAdmitted(wi.Obj)
	frUsage := wi.FlavorResourceUsage()
	for fr, q := range frUsage {
		if m == 1 {
			addUsage(c, fr, q)
		}
		if m == -1 {
			removeUsage(c, fr, q)
		}
	}
	if admitted {
		updateFlavorUsage(frUsage, c.AdmittedUsage, m)
		c.admittedWorkloadsCount += int(m)
	}
	qKey := workload.QueueKey(wi.Obj)
	if lq, ok := c.localQueues[qKey]; ok {
		updateFlavorUsage(frUsage, lq.usage, m)
		lq.reservingWorkloads += int(m)
		if admitted {
			updateFlavorUsage(frUsage, lq.admittedUsage, m)
			lq.admittedWorkloads += int(m)
		}
	}
}

func updateFlavorUsage(newUsage resources.FlavorResourceQuantities, oldUsage resources.FlavorResourceQuantities, m int64) {
	for fr, q := range newUsage {
		oldUsage[fr] += q * m
	}
}

func (c *clusterQueue) addLocalQueue(q *kueue.LocalQueue) error {
	qKey := queueKey(q)
	if _, ok := c.localQueues[qKey]; ok {
		return errQueueAlreadyExists
	}
	// We need to count the workloads, because they could have been added before
	// receiving the queue add event.
	qImpl := &queue{
		key:                qKey,
		reservingWorkloads: 0,
		usage:              make(resources.FlavorResourceQuantities),
	}
	qImpl.resetFlavorsAndResources(c.resourceNode.Usage, c.AdmittedUsage)
	for _, wl := range c.Workloads {
		if workloadBelongsToLocalQueue(wl.Obj, q) {
			frq := wl.FlavorResourceUsage()
			updateFlavorUsage(frq, qImpl.usage, 1)
			qImpl.reservingWorkloads++
			if workload.IsAdmitted(wl.Obj) {
				updateFlavorUsage(frq, qImpl.admittedUsage, 1)
				qImpl.admittedWorkloads++
			}
		}
	}
	c.localQueues[qKey] = qImpl
	return nil
}

func (c *clusterQueue) deleteLocalQueue(q *kueue.LocalQueue) {
	qKey := queueKey(q)
	delete(c.localQueues, qKey)
}

func (c *clusterQueue) flavorInUse(flavor string) bool {
	for _, rg := range c.ResourceGroups {
		for _, fName := range rg.Flavors {
			if kueue.ResourceFlavorReference(flavor) == fName {
				return true
			}
		}
	}
	return false
}

func (q *queue) resetFlavorsAndResources(cqUsage resources.FlavorResourceQuantities, cqAdmittedUsage resources.FlavorResourceQuantities) {
	// Clean up removed flavors or resources.
	q.usage = resetUsage(q.usage, cqUsage)
	q.admittedUsage = resetUsage(q.admittedUsage, cqAdmittedUsage)
}

func resetUsage(lqUsage resources.FlavorResourceQuantities, cqUsage resources.FlavorResourceQuantities) resources.FlavorResourceQuantities {
	usedFlavorResources := make(resources.FlavorResourceQuantities, len(cqUsage))
	for fr := range cqUsage {
		usedFlavorResources[fr] = lqUsage[fr]
	}
	return usedFlavorResources
}

func workloadBelongsToLocalQueue(wl *kueue.Workload, q *kueue.LocalQueue) bool {
	return wl.Namespace == q.Namespace && wl.Spec.QueueName == q.Name
}

// The methods below implement several interfaces. See
// dominantResourceShareNode, resourceGroupNode, and netQuotaNode.

func (c *clusterQueue) fairWeight() *resource.Quantity {
	return &c.FairWeight
}

func (c *clusterQueue) usageFor(fr resources.FlavorResource) int64 {
	return c.resourceNode.Usage[fr]
}

func (c *clusterQueue) QuotaFor(fr resources.FlavorResource) ResourceQuota {
	return c.resourceNode.Quotas[fr]
}

func (c *clusterQueue) resourceGroups() []ResourceGroup {
	return c.ResourceGroups
}

// DominantResourceShare returns a value from 0 to 1,000,000 representing the maximum of the ratios
// of usage above nominal quota to the lendable resources in the cohort, among all the resources
// provided by the ClusterQueue, and divided by the weight.
// If zero, it means that the usage of the ClusterQueue is below the nominal quota.
// The function also returns the resource name that yielded this value.
// Also for a weight of zero, this will return 9223372036854775807.
func (c *ClusterQueueSnapshot) DominantResourceShare() (int, corev1.ResourceName) {
	return dominantResourceShare(c, nil, 0)
}

func (c *ClusterQueueSnapshot) DominantResourceShareWith(wlReq resources.FlavorResourceQuantities) (int, corev1.ResourceName) {
	return dominantResourceShare(c, wlReq, 1)
}

func (c *ClusterQueueSnapshot) DominantResourceShareWithout(wlReq resources.FlavorResourceQuantities) (int, corev1.ResourceName) {
	return dominantResourceShare(c, wlReq, -1)
}

type dominantResourceShareNode interface {
	HasParent() bool
	parentResources() ResourceNode
	fairWeight() *resource.Quantity

	netQuotaNode
}

func dominantResourceShare(node dominantResourceShareNode, wlReq resources.FlavorResourceQuantities, m int64) (int, corev1.ResourceName) {
	if !node.HasParent() {
		return 0, ""
	}
	if node.fairWeight().IsZero() {
		return math.MaxInt, ""
	}

	borrowing := make(map[corev1.ResourceName]int64)
	for fr, quota := range remainingQuota(node) {
		b := m*wlReq[fr] - quota
		if b > 0 {
			borrowing[fr.Resource] += b
		}
	}
	if len(borrowing) == 0 {
		return 0, ""
	}

	var drs int64 = -1
	var dRes corev1.ResourceName

	lendable := node.parentResources().calculateLendable()
	for rName, b := range borrowing {
		if lr := lendable[rName]; lr > 0 {
			ratio := b * 1000 / lr
			// Use alphabetical order to get a deterministic resource name.
			if ratio > drs || (ratio == drs && rName < dRes) {
				drs = ratio
				dRes = rName
			}
		}
	}
	dws := drs * 1000 / node.fairWeight().MilliValue()
	return int(dws), dRes
}
