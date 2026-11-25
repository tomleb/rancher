package clusterstats

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	ext "github.com/rancher/rancher/pkg/apis/ext.cattle.io/v1"
	apiv3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	v3 "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
)

const (
	nodeRoleControlPlane       = "node-role.kubernetes.io/controlplane"
	nodeRoleControlPlaneHyphen = "node-role.kubernetes.io/control-plane"
	nodeRoleETCD               = "node-role.kubernetes.io/etcd"
)

// aggregator keeps the ClusterStat of every cluster in memory.
//
// Node changes only mark the node's cluster as dirty. The stats of a dirty
// cluster are recomputed from the node cache the next time they are read, so a
// burst of node updates (eg: on startup, or a large cluster heartbeating) costs
// a single recomputation per cluster instead of one per node event.
type aggregator struct {
	nodeCache v3.NodeCache

	mu    sync.Mutex
	dirty map[string]struct{}
	stats map[string]clusterStats
	// rv is bumped every time the stats of any cluster change. It's used as
	// the resourceVersion of the ClusterStat so clients polling with list
	// (eg: steve's synthetic watcher) can tell which objects changed.
	rv uint64
}

type clusterStats struct {
	status          ext.ClusterStatStatus
	resourceVersion string
}

func newAggregator(nodeCache v3.NodeCache) *aggregator {
	return &aggregator{
		nodeCache: nodeCache,
		dirty:     map[string]struct{}{},
		stats:     map[string]clusterStats{},
	}
}

// register watches management.cattle.io Nodes, which live in the namespace
// named after their cluster, and Clusters to forget deleted ones.
func (a *aggregator) register(ctx context.Context, nodes v3.NodeController, clusters v3.ClusterController) {
	nodes.OnChange(ctx, "ext-cluster-stats", a.onNodeChange)
	clusters.OnChange(ctx, "ext-cluster-stats", a.onClusterChange)
}

// onNodeChange uses the key rather than the object because the object is nil
// when the node was deleted, and a deletion must also refresh the stats.
func (a *aggregator) onNodeChange(key string, node *apiv3.Node) (*apiv3.Node, error) {
	clusterName, _, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return node, nil
	}
	a.markDirty(clusterName)
	return node, nil
}

func (a *aggregator) markDirty(clusterName string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dirty[clusterName] = struct{}{}
}

// get returns the stats of the given cluster, recomputing them if any of its
// nodes changed since the last read.
func (a *aggregator) get(clusterName string) clusterStats {
	a.mu.Lock()
	defer a.mu.Unlock()

	current, known := a.stats[clusterName]
	_, isDirty := a.dirty[clusterName]
	if known && !isDirty {
		return current
	}

	status, err := a.compute(clusterName)
	if err != nil {
		// Keep the cluster dirty so the next read retries, and serve the last
		// known stats (or empty stats) in the meantime. This mirrors the
		// cluster-stats controller which leaves the cluster status untouched
		// until every node has synced.
		a.dirty[clusterName] = struct{}{}
		return current
	}
	delete(a.dirty, clusterName)

	if !known || statusChanged(&current.status, &status) {
		a.rv++
		current = clusterStats{status: status, resourceVersion: strconv.FormatUint(a.rv, 10)}
		a.stats[clusterName] = current
	}
	return current
}

// onClusterChange forgets the stats of deleted clusters so they don't
// accumulate in memory. OnChange is used rather than OnRemove because the
// latter would add a finalizer to every Cluster.
func (a *aggregator) onClusterChange(key string, cluster *apiv3.Cluster) (*apiv3.Cluster, error) {
	if cluster != nil {
		return cluster, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.stats, key)
	delete(a.dirty, key)
	return nil, nil
}

// compute aggregates the stats of a cluster from its nodes. The logic matches
// pkg/controllers/management/clusterstats.
func (a *aggregator) compute(clusterName string) (ext.ClusterStatStatus, error) {
	allNodes, err := a.nodeCache.List(clusterName, labels.Everything())
	if err != nil {
		return ext.ClusterStatStatus{}, err
	}

	status := ext.ClusterStatStatus{
		NodeCount: len(allNodes),
	}

	capacity := newResourceSums()
	allocatable := newResourceSums()
	requested := newResourceSums()
	limits := newResourceSums()

	for _, node := range allNodes {
		// If none are set, then the nodes syncer has not completed.
		if !node.Spec.Worker && !node.Spec.ControlPlane && !node.Spec.Etcd {
			return ext.ClusterStatStatus{}, fmt.Errorf("node role cannot be determined because node %s has not finished syncing", node.Status.NodeName)
		}
		if isTaintedNoExecuteNoSchedule(node) && !node.Spec.Worker {
			continue
		}
		if node.Spec.InternalNodeSpec.Unschedulable {
			continue
		}

		if node.Spec.Worker {
			switch node.Status.NodeLabels[corev1.LabelOSStable] {
			case "linux":
				status.LinuxWorkerCount++
			case "windows":
				status.WindowsWorkerCount++
			}
		}

		capacity.add(node.Status.InternalNodeStatus.Capacity)
		allocatable.add(node.Status.InternalNodeStatus.Allocatable)
		requested.add(node.Status.Requested)
		limits.add(node.Status.Limits)
	}

	status.Capacity = capacity
	status.Allocatable = allocatable
	status.Requested = requested
	status.Limits = limits
	return status, nil
}

type resourceSums map[string]resource.Quantity

func newResourceSums() resourceSums {
	return resourceSums{
		string(corev1.ResourcePods):   resource.Quantity{},
		string(corev1.ResourceMemory): resource.Quantity{},
		string(corev1.ResourceCPU):    resource.Quantity{},
	}
}

func (s resourceSums) add(list corev1.ResourceList) {
	if list == nil {
		return
	}
	for name, q := range map[corev1.ResourceName]*resource.Quantity{
		corev1.ResourcePods:   list.Pods(),
		corev1.ResourceMemory: list.Memory(),
		corev1.ResourceCPU:    list.Cpu(),
	} {
		sum := s[string(name)]
		sum.Add(*q)
		s[string(name)] = sum
	}
}

func isTaintedNoExecuteNoSchedule(node *apiv3.Node) bool {
	for _, taint := range node.Spec.InternalNodeSpec.Taints {
		isServer := taint.Key == nodeRoleControlPlane || taint.Key == nodeRoleETCD || taint.Key == nodeRoleControlPlaneHyphen
		if isServer && (taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute) {
			return true
		}
	}
	return false
}

func statusChanged(old, new *ext.ClusterStatStatus) bool {
	return old.NodeCount != new.NodeCount ||
		old.LinuxWorkerCount != new.LinuxWorkerCount ||
		old.WindowsWorkerCount != new.WindowsWorkerCount ||
		quantitiesChanged(old.Capacity, new.Capacity) ||
		quantitiesChanged(old.Allocatable, new.Allocatable) ||
		quantitiesChanged(old.Requested, new.Requested) ||
		quantitiesChanged(old.Limits, new.Limits)
}

func quantitiesChanged(old, new map[string]resource.Quantity) bool {
	if len(old) != len(new) {
		return true
	}
	for k, v := range old {
		n, ok := new[k]
		if !ok || v.Cmp(n) != 0 {
			return true
		}
	}
	return false
}
