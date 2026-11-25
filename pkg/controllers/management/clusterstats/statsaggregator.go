package clusterstats

import (
	"context"
	"sync"
	"time"

	apimgmtv3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/clustermanager"
	v3 "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/types/config"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	// quietPeriod marks the minimum period between sync calls for the same Cluster
	quietPeriod = time.Second * 10
)

// StatsAggregator used to aggregate node stats (node counts, capacity,
// allocatable, requested, limits and the NoDiskPressure/NoMemoryPressure
// conditions) into the Cluster status. Those stats are now served from memory
// by the ext.cattle.io ClusterStat API (see pkg/ext/stores/clusterstats) and
// the corresponding Cluster status fields are no longer updated.
//
// The controller still records the Kubernetes version of the cluster in the
// Cluster status, removes a legacy annotation from the cluster agent, and
// enqueues the Cluster when its Nodes change.
type StatsAggregator struct {
	Clusters       v3.ClusterInterface
	ClusterManager *clustermanager.Manager

	// lastReconcile is a map[string]time.Time storing the last execution time of the sync reconciler for every Cluster key
	lastReconcile sync.Map
}

func Register(ctx context.Context, management *config.ManagementContext, clusterManager *clustermanager.Manager) {
	clustersClient := management.Management.Clusters("")
	machinesClient := management.Management.Nodes("")

	s := &StatsAggregator{
		Clusters:       clustersClient,
		ClusterManager: clusterManager,
	}

	clustersClient.AddHandler(ctx, "cluster-stats", s.sync)
	// This handler enqueues the corresponding mgmt.Cluster for every mgmt.Node reconciliation. Other handlers may also rely on this.
	// Nonetheless, in order to prevent unnecessary reconciliation when a cluster has many nodes, we are throttling (and deduplicating) the trigger.
	machinesClient.AddHandler(ctx, "cluster-stats", s.machineChanged)
}

func (s *StatsAggregator) sync(key string, cluster *apimgmtv3.Cluster) (runtime.Object, error) {
	if cluster == nil {
		s.lastReconcile.Delete(key)
		return nil, nil
	}

	if d := s.getMinimumWaitTime(key); d > 0 {
		// re-enqueue this controller after the quiet period
		s.Clusters.Controller().EnqueueAfter(cluster.Namespace, cluster.Name, d)
		return nil, nil
	}

	updated, err := s.reconcile(cluster)
	if err == nil {
		s.lastReconcile.Store(key, time.Now())
	}
	return updated, err
}

func (s *StatsAggregator) getMinimumWaitTime(key string) time.Duration {
	if v, ok := s.lastReconcile.Load(key); ok {
		lastReconcile := v.(time.Time)
		if wait := quietPeriod - time.Since(lastReconcile); wait > 0 {
			// only return positive values
			// the workqueue implementation accounts for negative or zero values, but let's play safe
			return wait
		}
	}
	return 0
}

func (s *StatsAggregator) reconcile(cluster *apimgmtv3.Cluster) (*apimgmtv3.Cluster, error) {
	origStatus := cluster.Status.DeepCopy()
	cluster = cluster.DeepCopy()

	s.updateVersion(cluster)

	// Remove "agent.cluster.cattle.io/upgraded-v1.22" annotation
	if err := s.removeLegacyUpgradedAnnotation(cluster); err != nil {
		return nil, err
	}

	if versionChanged(origStatus, &cluster.Status) {
		// Use UpdateStatus to only update the status subresource - Update() ignores status changes.
		// Return nil to avoid norman's redundant Update() write-back.
		if _, err := s.Clusters.UpdateStatus(cluster); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func (s *StatsAggregator) updateVersion(cluster *apimgmtv3.Cluster) {
	userContext, err := s.ClusterManager.UserContextNoControllersReconnecting(cluster.Name, false)
	if err != nil {
		return
	}

	callWithTimeout(func() {
		// This has the tendency to timeout
		version, err := userContext.K8sClient.Discovery().ServerVersion()
		if err != nil {
			return
		}

		if version != nil {
			// These fields can vary depending on the node serving the request
			// Since they are not consumed, we omit them from the status to avoid unnecessary flickering of the fields
			version.BuildDate, version.Platform = "", ""
		}

		if version == nil {
			return
		}

		currentVersionOk := cluster.Status.Version != nil
		versionOk := version != nil

		// if we don't have an older version recorded, the new one could not be queried, or they are both populated and different
		if currentVersionOk != versionOk || (currentVersionOk && *cluster.Status.Version != *version) {
			cluster.Status.Version = version
		}
	})
}

// removeLegacyUpgradedAnnotation removes an annotation on the cluster agent deployment template
// which will force a restart of the deployment. This annotation was used to stop handlers running from when the
// cluster was running k8s 1.22, however this is applied to every newly created cluster causing unnecessary rollouts.
func (s *StatsAggregator) removeLegacyUpgradedAnnotation(cluster *apimgmtv3.Cluster) error {
	userContext, err := s.ClusterManager.UserContextNoControllersReconnecting(cluster.Name, false)
	if err != nil || userContext == nil {
		return nil
	}

	deployment, err := userContext.Apps.Deployments("cattle-system").Get("cattle-cluster-agent", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}

	if deployment.Spec.Template.Annotations == nil {
		return nil
	}

	// Add annotation to the agent deployment template in order to force a restart
	if deployment.Spec.Template.Annotations["agent.cluster.cattle.io/upgraded-v1.22"] != "" {
		logrus.Tracef("statsAggregator: updated cluster %s to v1.22, annotating agent deployment", cluster.Name)
		toUpdate := deployment.DeepCopy()
		delete(toUpdate.Spec.Template.Annotations, "agent.cluster.cattle.io/upgraded-v1.22")
		_, err = userContext.Apps.Deployments("cattle-system").Update(toUpdate)
		if err != nil {
			return err
		}
	}
	return nil
}

func versionChanged(currentStatus, newStatus *apimgmtv3.ClusterStatus) bool {
	currentVersionOk := currentStatus.Version != nil
	versionOk := newStatus.Version != nil

	// if we don't have an older version recorded, the new one could not be queried, or they are both populated and different
	return currentVersionOk != versionOk || (currentVersionOk && *currentStatus.Version != *newStatus.Version)
}

func callWithTimeout(do func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		do()
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
	}
}

func (s *StatsAggregator) machineChanged(_ string, machine *apimgmtv3.Node) (runtime.Object, error) {
	if machine == nil {
		return nil, nil
	}

	if d := s.getMinimumWaitTime(machine.Namespace); d > 0 {
		s.Clusters.Controller().EnqueueAfter("", machine.Namespace, d)
	} else {
		// EnqueueAfter with a zero duration short-circuits to workqueue.Add, bypassing the rate-limiter
		s.Clusters.Controller().Enqueue("", machine.Namespace)
	}

	return nil, nil
}
