package node

import (
	"context"
	"fmt"
	"strings"

	features "github.com/openshift/api/features"
	mcfgalphav1 "github.com/openshift/api/machineconfiguration/v1alpha1"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	helpers "github.com/openshift/machine-config-operator/pkg/helpers"

	mcfgv1 "github.com/openshift/api/machineconfiguration/v1"
	"github.com/openshift/machine-config-operator/pkg/apihelpers"
	ctrlcommon "github.com/openshift/machine-config-operator/pkg/controller/common"
	"github.com/openshift/machine-config-operator/pkg/daemon/constants"
	daemonconsts "github.com/openshift/machine-config-operator/pkg/daemon/constants"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// syncStatusOnly for MachineConfigNode
func (ctrl *Controller) syncStatusOnly(pool *mcfgv1.MachineConfigPool) error {
	cc, err := ctrl.ccLister.Get(ctrlcommon.ControllerConfigName)
	if err != nil {
		return err
	}
	nodes, err := ctrl.getNodesForPool(pool)
	if err != nil {
		return err
	}

	machineConfigStates := []*mcfgalphav1.MachineConfigNode{}
	fg, err := ctrl.fgAcessor.CurrentFeatureGates()
	list := fg.KnownFeatures()
	mcnExists := false
	for _, feature := range list {
		if feature == features.FeatureGateMachineConfigNodes {
			mcnExists = true
		}
	}
	if err != nil {
		klog.Errorf("Could not get FG: %v", err)
	} else if mcnExists && fg.Enabled(features.FeatureGateMachineConfigNodes) {
		for _, node := range nodes {
			ms, err := ctrl.client.MachineconfigurationV1alpha1().MachineConfigNodes().Get(context.TODO(), node.Name, metav1.GetOptions{})
			if err != nil {
				klog.Errorf("Could not find our MachineConfigNode for node. %s: %v", node.Name, err)
				continue
			}
			machineConfigStates = append(machineConfigStates, ms)
		}
	}
	newStatus := calculateStatus(fg, machineConfigStates, cc, pool, nodes)
	if equality.Semantic.DeepEqual(pool.Status, newStatus) {
		return nil
	}

	newPool := pool
	newPool.Status = newStatus
	_, err = ctrl.client.MachineconfigurationV1().MachineConfigPools().UpdateStatus(context.TODO(), newPool, metav1.UpdateOptions{})

	if pool.Spec.Configuration.Name != newPool.Spec.Configuration.Name {
		ctrl.eventRecorder.Eventf(pool, corev1.EventTypeNormal, "Updating", "Pool %s now targeting %s", pool.Name, getPoolUpdateLine(newPool))
	}
	if pool.Status.Configuration.Name != newPool.Status.Configuration.Name {
		ctrl.eventRecorder.Eventf(pool, corev1.EventTypeNormal, "Completed", "Pool %s has completed update to %s", pool.Name, getPoolUpdateLine(newPool))
	}
	return err
}

//nolint:gocyclo
func calculateStatus(fg featuregates.FeatureGate, mcs []*mcfgalphav1.MachineConfigNode, cconfig *mcfgv1.ControllerConfig, pool *mcfgv1.MachineConfigPool, nodes []*corev1.Node) mcfgv1.MachineConfigPoolStatus {
	certExpirys := []mcfgv1.CertExpiry{}
	if cconfig != nil {
		for _, cert := range cconfig.Status.ControllerCertificates {
			if cert.BundleFile == "KubeAPIServerServingCAData" {
				certExpirys = append(certExpirys, mcfgv1.CertExpiry{
					Bundle:  cert.BundleFile,
					Subject: cert.Subject,
					Expiry:  cert.NotAfter,
				},
				)
			}
		}
	}
	machineCount := int32(len(nodes))
	updatingPinnedImageSetMachines := int32(0)

	var degradedMachines, readyMachines, updatedMachines, unavailableMachines, updatingMachines []*corev1.Node
	degradedReasons := []string{}

	// if we represent updating properly here, we will also represent updating properly in the CO
	// so this solves the cordoning RFE and the upgradeable RFE
	// updating == updatePrepared, updateExecuted, updatedComplete, postAction, cordoning, draining
	// updated == nodeResumed, updated
	// ready == nodeResumed, updated
	// unavailable == draining, cordoned
	// degraded == if the condition.Reason == error
	// this ensures that a MCP only enters Upgradeable==False if the node actually needs to upgrade to the new MC
	for _, state := range mcs {
		var ourNode *corev1.Node
		for _, n := range nodes {
			if state.Name == n.Name {
				ourNode = n
				break
			}
		}
		if ourNode == nil {
			klog.Errorf("Could not find specified node %s", state.Name)
		}
		if len(state.Status.Conditions) == 0 {
			// not ready yet
			break
		}
		if fg.Enabled(features.FeatureGatePinnedImages) {
			if isPinnedImageSetNodeUpdating(state) {
				updatingPinnedImageSetMachines++
			}
		}
		for _, cond := range state.Status.Conditions {
			if strings.Contains(cond.Message, "Error:") {
				degradedMachines = append(degradedMachines, ourNode)
				// populate the degradedReasons from the MachineConfigNodePinnedImageSetsDegraded condition
				if fg.Enabled(features.FeatureGatePinnedImages) {
					if mcfgalphav1.StateProgress(cond.Type) == mcfgalphav1.MachineConfigNodePinnedImageSetsDegraded && cond.Status == metav1.ConditionTrue {
						degradedReasons = append(degradedReasons, fmt.Sprintf("Node %s is reporting: %q", ourNode.Name, cond.Message))
					}
				}
				continue
			}

			if cond.Status == metav1.ConditionUnknown {
				switch mcfgalphav1.StateProgress(cond.Type) {
				case mcfgalphav1.MachineConfigNodeUpdatePrepared:
					updatingMachines = append(updatedMachines, ourNode)
				case mcfgalphav1.MachineConfigNodeUpdateExecuted:
					updatingMachines = append(updatingMachines, ourNode)
				case mcfgalphav1.MachineConfigNodeUpdatePostActionComplete:
					updatingMachines = append(updatingMachines, ourNode)
				case mcfgalphav1.MachineConfigNodeUpdateComplete:
					updatingMachines = append(updatingMachines, ourNode)
				case mcfgalphav1.MachineConfigNodeResumed:
					updatedMachines = append(updatingMachines, ourNode)
					readyMachines = append(readyMachines, ourNode)
				case mcfgalphav1.MachineConfigNodeUpdateCompatible:
					updatedMachines = append(updatingMachines, ourNode)
				case mcfgalphav1.MachineConfigNodeUpdateDrained:
					unavailableMachines = append(unavailableMachines, ourNode)
					updatingMachines = append(updatingMachines, ourNode)
				case mcfgalphav1.MachineConfigNodeUpdateCordoned:
					unavailableMachines = append(unavailableMachines, ourNode)
					updatingMachines = append(updatingMachines, ourNode)
				case mcfgalphav1.MachineConfigNodeUpdated:
					updatedMachines = append(updatedMachines, ourNode)
					readyMachines = append(readyMachines, ourNode)
				}
			}
		}
	}
	degradedMachineCount := int32(len(degradedMachines))
	updatedMachineCount := int32(len(updatedMachines))
	unavailableMachineCount := int32(len(unavailableMachines))
	updatingMachineCount := int32(len(updatingMachines))
	readyMachineCount := int32(len(readyMachines))

	// this is # 1 priority, get the upgrade states actually reporting
	if degradedMachineCount+readyMachineCount+unavailableMachineCount+updatingMachineCount != int32(len(nodes)) {
		updatedMachines = getUpdatedMachines(pool, nodes)
		updatedMachineCount = int32(len(updatedMachines))

		readyMachines = getReadyMachines(pool, nodes)
		readyMachineCount = int32(len(readyMachines))

		unavailableMachines = getUnavailableMachines(nodes, pool)
		unavailableMachineCount = int32(len(unavailableMachines))

		degradedMachines = getDegradedMachines(nodes)
		degradedMachineCount = int32(len(degradedMachines))
	}

	for _, n := range degradedMachines {
		reason, ok := n.Annotations[daemonconsts.MachineConfigDaemonReasonAnnotationKey]
		if ok && reason != "" {
			degradedReasons = append(degradedReasons, fmt.Sprintf("Node %s is reporting: %q", n.Name, reason))
		}
	}

	status := mcfgv1.MachineConfigPoolStatus{
		ObservedGeneration:      pool.Generation,
		MachineCount:            machineCount,
		UpdatedMachineCount:     updatedMachineCount,
		ReadyMachineCount:       readyMachineCount,
		UnavailableMachineCount: unavailableMachineCount,
		DegradedMachineCount:    degradedMachineCount,
		CertExpirys:             certExpirys,
	}

	// update synchronizer status for pinned image sets
	if fg.Enabled(features.FeatureGatePinnedImages) {
		// TODO: update counts to be more granular
		status.PoolSynchronizersStatus = []mcfgv1.PoolSynchronizerStatus{
			{
				PoolSynchronizerType:    mcfgv1.PinnedImageSets,
				MachineCount:            int64(machineCount),
				UpdatedMachineCount:     int64(machineCount - updatingPinnedImageSetMachines),
				ReadyMachineCount:       int64(readyMachineCount),
				UnavailableMachineCount: int64(unavailableMachineCount),
				AvailableMachineCount:   int64(machineCount - unavailableMachineCount),
			},
		}
	}

	status.Configuration = pool.Status.Configuration

	conditions := pool.Status.Conditions
	for i := range conditions {
		status.Conditions = append(status.Conditions, conditions[i])
	}

	allUpdated := updatedMachineCount == machineCount &&
		readyMachineCount == machineCount &&
		unavailableMachineCount == 0

	if allUpdated {
		//TODO: update api to only have one condition regarding status of update.
		updatedMsg := fmt.Sprintf("All nodes are updated with %s", getPoolUpdateLine(pool))
		supdated := apihelpers.NewMachineConfigPoolCondition(mcfgv1.MachineConfigPoolUpdated, corev1.ConditionTrue, "", updatedMsg)
		apihelpers.SetMachineConfigPoolCondition(&status, *supdated)

		supdating := apihelpers.NewMachineConfigPoolCondition(mcfgv1.MachineConfigPoolUpdating, corev1.ConditionFalse, "", "")
		apihelpers.SetMachineConfigPoolCondition(&status, *supdating)
		if status.Configuration.Name != pool.Spec.Configuration.Name || !equality.Semantic.DeepEqual(status.Configuration.Source, pool.Spec.Configuration.Source) {
			klog.Infof("Pool %s: %s", pool.Name, updatedMsg)
			status.Configuration = pool.Spec.Configuration
		}
	} else {
		supdated := apihelpers.NewMachineConfigPoolCondition(mcfgv1.MachineConfigPoolUpdated, corev1.ConditionFalse, "", "")
		apihelpers.SetMachineConfigPoolCondition(&status, *supdated)
		if pool.Spec.Paused {
			supdating := apihelpers.NewMachineConfigPoolCondition(mcfgv1.MachineConfigPoolUpdating, corev1.ConditionFalse, "", fmt.Sprintf("Pool is paused; will not update to %s", getPoolUpdateLine(pool)))
			apihelpers.SetMachineConfigPoolCondition(&status, *supdating)
		} else {
			supdating := apihelpers.NewMachineConfigPoolCondition(mcfgv1.MachineConfigPoolUpdating, corev1.ConditionTrue, "", fmt.Sprintf("All nodes are updating to %s", getPoolUpdateLine(pool)))
			apihelpers.SetMachineConfigPoolCondition(&status, *supdating)
		}
	}

	var nodeDegraded bool
	for _, m := range degradedMachines {
		klog.Infof("Degraded Machine: %v and Degraded Reason: %v", m.Name, m.Annotations[constants.MachineConfigDaemonReasonAnnotationKey])
	}
	if degradedMachineCount > 0 {
		nodeDegraded = true
		sdegraded := apihelpers.NewMachineConfigPoolCondition(mcfgv1.MachineConfigPoolNodeDegraded, corev1.ConditionTrue, fmt.Sprintf("%d nodes are reporting degraded status on sync", len(degradedMachines)), strings.Join(degradedReasons, ", "))
		apihelpers.SetMachineConfigPoolCondition(&status, *sdegraded)
	} else {
		sdegraded := apihelpers.NewMachineConfigPoolCondition(mcfgv1.MachineConfigPoolNodeDegraded, corev1.ConditionFalse, "", "")
		apihelpers.SetMachineConfigPoolCondition(&status, *sdegraded)
	}

	// here we now set the MCP Degraded field, the node_controller is the one making the call right now
	// but we might have a dedicated controller or control loop somewhere else that understands how to
	// set Degraded. For now, the node_controller understand NodeDegraded & RenderDegraded = Degraded.

	pinnedImageSetsDegraded := false
	if fg.Enabled(features.FeatureGatePinnedImages) {
		pinnedImageSetsDegraded = apihelpers.IsMachineConfigPoolConditionTrue(pool.Status.Conditions, mcfgv1.MachineConfigPoolPinnedImageSetsDegraded)
	}

	renderDegraded := apihelpers.IsMachineConfigPoolConditionTrue(pool.Status.Conditions, mcfgv1.MachineConfigPoolRenderDegraded)
	if nodeDegraded || renderDegraded || pinnedImageSetsDegraded {
		sdegraded := apihelpers.NewMachineConfigPoolCondition(mcfgv1.MachineConfigPoolDegraded, corev1.ConditionTrue, "", "")
		apihelpers.SetMachineConfigPoolCondition(&status, *sdegraded)

	} else {
		sdegraded := apihelpers.NewMachineConfigPoolCondition(mcfgv1.MachineConfigPoolDegraded, corev1.ConditionFalse, "", "")
		apihelpers.SetMachineConfigPoolCondition(&status, *sdegraded)
	}

	return status
}

func isPinnedImageSetNodeUpdating(mcs *mcfgalphav1.MachineConfigNode) bool {
	var updating int32
	for _, set := range mcs.Status.PinnedImageSets {
		if set.CurrentGeneration != set.DesiredGeneration {
			updating++
		}
	}
	return updating > 0
}

func getPoolUpdateLine(pool *mcfgv1.MachineConfigPool) string {
	targetConfig := pool.Spec.Configuration.Name
	mcLine := fmt.Sprintf("MachineConfig %s", targetConfig)

	if !ctrlcommon.IsLayeredPool(pool) {
		return mcLine
	}

	targetImage, ok := pool.Annotations[ctrlcommon.ExperimentalNewestLayeredImageEquivalentConfigAnnotationKey]
	if !ok {
		return mcLine
	}

	return fmt.Sprintf("%s / Image %s", mcLine, targetImage)
}

// isNodeManaged checks whether the MCD has ever run on a node
func isNodeManaged(node *corev1.Node) bool {
	if helpers.IsWindows(node) {
		klog.V(4).Infof("Node %v is a windows node so won't be managed by MCO", node.Name)
		return false
	}
	if node.Annotations == nil {
		return false
	}
	cconfig, ok := node.Annotations[daemonconsts.CurrentMachineConfigAnnotationKey]
	if !ok || cconfig == "" {
		return false
	}
	return true
}

// isNodeDone returns true if the current == desired and the MCD has marked done.
func isNodeDone(node *corev1.Node) bool {
	if node.Annotations == nil {
		return false
	}
	cconfig, ok := node.Annotations[daemonconsts.CurrentMachineConfigAnnotationKey]
	if !ok || cconfig == "" {
		return false
	}
	dconfig, ok := node.Annotations[daemonconsts.DesiredMachineConfigAnnotationKey]
	if !ok || dconfig == "" {
		return false
	}

	return cconfig == dconfig && isNodeMCDState(node, daemonconsts.MachineConfigDaemonStateDone)
}

// isNodeDoneAt checks whether a node is fully updated to a targetConfig
func isNodeDoneAt(node *corev1.Node, pool *mcfgv1.MachineConfigPool) bool {
	return isNodeDone(node) && node.Annotations[daemonconsts.CurrentMachineConfigAnnotationKey] == pool.Spec.Configuration.Name
}

// isNodeMCDState checks the MCD state against the state parameter
func isNodeMCDState(node *corev1.Node, state string) bool {
	dstate, ok := node.Annotations[daemonconsts.MachineConfigDaemonStateAnnotationKey]
	if !ok || dstate == "" {
		return false
	}

	return dstate == state
}

// isNodeMCDFailing checks if the MCD has unsuccessfully applied an update
func isNodeMCDFailing(node *corev1.Node) bool {
	if node.Annotations[daemonconsts.CurrentMachineConfigAnnotationKey] == node.Annotations[daemonconsts.DesiredMachineConfigAnnotationKey] {
		return false
	}
	return isNodeMCDState(node, daemonconsts.MachineConfigDaemonStateDegraded) ||
		isNodeMCDState(node, daemonconsts.MachineConfigDaemonStateUnreconcilable)
}

// getUpdatedMachines filters the provided nodes to return the nodes whose
// current config matches the desired config, which also matches the target config,
// and the "done" flag is set.
func getUpdatedMachines(pool *mcfgv1.MachineConfigPool, nodes []*corev1.Node) []*corev1.Node {
	var updated []*corev1.Node
	for _, node := range nodes {
		lns := ctrlcommon.NewLayeredNodeState(node)
		if lns.IsDoneAt(pool) {
			updated = append(updated, node)
		}
	}
	return updated
}

// getReadyMachines filters the provided nodes to return the nodes
// that are updated and marked ready
func getReadyMachines(pool *mcfgv1.MachineConfigPool, nodes []*corev1.Node) []*corev1.Node {
	updated := getUpdatedMachines(pool, nodes)
	var ready []*corev1.Node
	for _, node := range updated {
		if isNodeReady(node) {
			ready = append(ready, node)
		}
	}
	return ready
}

func checkNodeReady(node *corev1.Node) error {
	for i := range node.Status.Conditions {
		cond := &node.Status.Conditions[i]
		// We consider the node for scheduling only when its:
		// - NodeReady condition status is ConditionTrue,
		// - NodeDiskPressure condition status is ConditionFalse,
		// - NodeNetworkUnavailable condition status is ConditionFalse.
		if cond.Type == corev1.NodeReady && cond.Status != corev1.ConditionTrue {
			return fmt.Errorf("node %s is reporting NotReady=%v", node.Name, cond.Status)
		}
		if cond.Type == corev1.NodeDiskPressure && cond.Status != corev1.ConditionFalse {
			return fmt.Errorf("node %s is reporting OutOfDisk=%v", node.Name, cond.Status)
		}
		if cond.Type == corev1.NodeNetworkUnavailable && cond.Status != corev1.ConditionFalse {
			return fmt.Errorf("node %s is reporting NetworkUnavailable=%v", node.Name, cond.Status)
		}
	}
	// Ignore nodes that are marked unschedulable
	if node.Spec.Unschedulable {
		return fmt.Errorf("node %s is reporting Unschedulable", node.Name)
	}
	return nil
}

func isNodeReady(node *corev1.Node) bool {
	return checkNodeReady(node) == nil
}

// isNodeUnavailable is a helper function for getUnavailableMachines
// See the docs of getUnavailableMachines for more info
func isNodeUnavailable(node *corev1.Node) bool {
	// Unready nodes are unavailable
	if !isNodeReady(node) {
		return true
	}
	// Ready nodes are not unavailable
	if isNodeDone(node) {
		return false
	}
	// Now we know the node isn't ready - the current config must not
	// equal target.  We want to further filter down on the MCD state.
	// If a MCD is in a terminal (failing) state then we can safely retarget it.
	// to a different config.  Or to say it another way, a node is unavailable
	// if the MCD is working, or hasn't started work but the configs differ.
	return !isNodeMCDFailing(node)
}

// getUnavailableMachines returns the set of nodes which are
// either marked unscheduleable, or have a MCD actively working.
// If the MCD is actively working (or hasn't started) then the
// node *may* go unschedulable in the future, so we don't want to
// potentially start another node update exceeding our maxUnavailable.
// Somewhat the opposite of getReadyNodes().
func getUnavailableMachines(nodes []*corev1.Node, pool *mcfgv1.MachineConfigPool) []*corev1.Node {
	var unavail []*corev1.Node
	for _, node := range nodes {
		lns := ctrlcommon.NewLayeredNodeState(node)
		if lns.IsUnavailable(pool) {
			unavail = append(unavail, node)
		}
	}

	return unavail
}

func getDegradedMachines(nodes []*corev1.Node) []*corev1.Node {
	var degraded []*corev1.Node
	for _, node := range nodes {
		if node.Annotations == nil {
			continue
		}
		dconfig, ok := node.Annotations[daemonconsts.DesiredMachineConfigAnnotationKey]
		if !ok || dconfig == "" {
			continue
		}
		dstate, ok := node.Annotations[daemonconsts.MachineConfigDaemonStateAnnotationKey]
		if !ok || dstate == "" {
			continue
		}

		if dstate == daemonconsts.MachineConfigDaemonStateDegraded || dstate == daemonconsts.MachineConfigDaemonStateUnreconcilable {
			degraded = append(degraded, node)
		}
	}
	return degraded
}

func getNamesFromNodes(nodes []*corev1.Node) []string {
	if len(nodes) == 0 {
		return nil
	}

	names := []string{}
	for _, node := range nodes {
		names = append(names, node.Name)
	}

	return names
}
