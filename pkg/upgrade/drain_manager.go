/*
Copyright 2022 NVIDIA CORPORATION & AFFILIATES
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

package upgrade

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubectl/pkg/drain"

	"github.com/Mellanox/network-operator/api/v1alpha1"
	"github.com/Mellanox/network-operator/pkg/consts"
)

// DrainConfiguration contains the drain specification and the list of nodes to schedule drain on
type DrainConfiguration struct {
	Spec  *v1alpha1.DrainSpec
	Nodes []*corev1.Node
}

// DrainManagerImpl implements DrainManager interface and can perform nodes drain based on received DrainConfiguration
type DrainManagerImpl struct {
	k8sInterface             kubernetes.Interface
	drainingNodes            *StringSet
	nodeUpgradeStateProvider NodeUpgradeStateProvider

	log logr.Logger
}

// DrainManager is an interface that allows to schedule nodes drain based on DrainSpec
type DrainManager interface {
	ScheduleNodesDrain(ctx context.Context, drainConfig *DrainConfiguration) error
}

// ScheduleNodesDrain receives DrainConfiguration and schedules drain for each node in the list.
// When the node gets scheduled, it's marked as being drained and therefore will not be scheduled for drain twice
// if the initial drain didn't complete yet.
// During the drain the node is cordoned first, and then pods on the node are evicted.
// If the drain is successful, the node moves to UpgradeStatePodRestart state,
// otherwise it moves to UpgradeStateDrainFailed state.
func (m *DrainManagerImpl) ScheduleNodesDrain(ctx context.Context, drainConfig *DrainConfiguration) error {
	m.log.V(consts.LogLevelInfo).Info("Drain Manager, starting Node Drain")

	if len(drainConfig.Nodes) == 0 {
		m.log.V(consts.LogLevelInfo).Info("Drain Manager, no nodes scheduled to drain")
		return nil
	}

	drainSpec := drainConfig.Spec

	if drainSpec == nil {
		return fmt.Errorf("drain spec should not be empty")
	}
	if !drainSpec.Enable {
		m.log.V(consts.LogLevelInfo).Info("Drain Manager, drain is disabled")
		return nil
	}

	drainHelper := &drain.Helper{
		Ctx:    ctx,
		Client: m.k8sInterface,
		Force:  drainSpec.Force,
		// OFED Drivers Pods are part of a DaemonSet, so, this option needs to be set to true
		IgnoreAllDaemonSets: true,
		DeleteEmptyDirData:  drainSpec.DeleteEmptyDir,
		GracePeriodSeconds:  -1,
		Timeout:             time.Duration(drainSpec.TimeoutSecond),
		PodSelector:         drainSpec.PodSelector,
		OnPodDeletedOrEvicted: func(pod *corev1.Pod, usingEviction bool) {
			verbStr := "Deleted"
			if usingEviction {
				verbStr = "Evicted"
			}
			m.log.V(consts.LogLevelInfo).Info(fmt.Sprintf("%s pod from Node %s/%s", verbStr, pod.Namespace, pod.Name))
		},
		Out:    os.Stdout,
		ErrOut: os.Stdout,
	}

	for _, node := range drainConfig.Nodes {
		if !m.drainingNodes.Has(node.Name) {
			m.log.V(consts.LogLevelInfo).Info("Schedule drain for node", "node", node.Name)

			m.drainingNodes.Add(node.Name)
			go func() {
				defer m.drainingNodes.Remove(node.Name)
				err := drain.RunCordonOrUncordon(drainHelper, node, true)
				if err != nil {
					m.log.V(consts.LogLevelError).Error(err, "Failed to cordon node", "node", node.Name)
					_ = m.nodeUpgradeStateProvider.ChangeNodeUpgradeState(ctx, node, UpgradeStateDrainFailed)
					return
				}
				m.log.V(consts.LogLevelInfo).Info("Cordoned the node", "node", node.Name)

				err = drain.RunNodeDrain(drainHelper, node.Name)
				if err != nil {
					m.log.V(consts.LogLevelError).Error(err, "Failed to drain node", "node", node.Name)
					_ = m.nodeUpgradeStateProvider.ChangeNodeUpgradeState(ctx, node, UpgradeStateDrainFailed)
					return
				}
				m.log.V(consts.LogLevelInfo).Info("Drained the node", "node", node.Name)

				_ = m.nodeUpgradeStateProvider.ChangeNodeUpgradeState(ctx, node, UpgradeStatePodRestart)
			}()
		} else {
			m.log.V(consts.LogLevelInfo).Info("Node is already being drained, skipping", "node", node.Name)
		}
	}
	return nil
}

func NewDrainManager(
	k8sInterface kubernetes.Interface,
	nodeUpgradeStateProvider NodeUpgradeStateProvider,
	log logr.Logger) *DrainManagerImpl {
	mgr := &DrainManagerImpl{
		k8sInterface:             k8sInterface,
		log:                      log,
		drainingNodes:            NewStringSet(),
		nodeUpgradeStateProvider: nodeUpgradeStateProvider,
	}

	return mgr
}
