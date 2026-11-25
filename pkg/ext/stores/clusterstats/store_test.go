package clusterstats

import (
	"context"
	"testing"

	ext "github.com/rancher/rancher/pkg/apis/ext.cattle.io/v1"
	apiv3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/rancher/wrangler/v3/pkg/generic/fake"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apiserver/pkg/registry/rest"
)

// The store must only support get and list. The extension API server decides
// which verbs to expose based on the interfaces the store implements.
func TestStoreOnlyImplementsGetAndList(t *testing.T) {
	var s rest.Storage = &Store{}

	_, ok := s.(rest.Getter)
	assert.True(t, ok, "get")
	_, ok = s.(rest.Lister)
	assert.True(t, ok, "list")

	_, ok = s.(rest.Watcher)
	assert.False(t, ok, "watch")
	_, ok = s.(rest.Creater)
	assert.False(t, ok, "create")
	_, ok = s.(rest.Updater)
	assert.False(t, ok, "update")
	_, ok = s.(rest.Patcher)
	assert.False(t, ok, "patch")
	_, ok = s.(rest.GracefulDeleter)
	assert.False(t, ok, "delete")
	_, ok = s.(rest.CollectionDeleter)
	assert.False(t, ok, "deletecollection")
}

func newNode(cluster, name string, mutate ...func(*apiv3.Node)) *apiv3.Node {
	n := &apiv3.Node{
		ObjectMeta: metav1.ObjectMeta{Namespace: cluster, Name: name},
		Spec:       apiv3.NodeSpec{Worker: true},
		Status: apiv3.NodeStatus{
			NodeName:   name,
			NodeLabels: map[string]string{corev1.LabelOSStable: "linux"},
			InternalNodeStatus: corev1.NodeStatus{
				Capacity:    resources("4", "8Gi", "110"),
				Allocatable: resources("3", "7Gi", "100"),
			},
			Requested: resources("1", "1Gi", "10"),
			Limits:    resources("2", "2Gi", "10"),
		},
	}
	for _, m := range mutate {
		m(n)
	}
	return n
}

func resources(cpu, mem, pods string) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem),
		corev1.ResourcePods:   resource.MustParse(pods),
	}
}

func assertQuantity(t *testing.T, wantStr string, got map[string]resource.Quantity, name string) {
	t.Helper()
	q := got[name]
	want := resource.MustParse(wantStr)
	assert.Zero(t, want.Cmp(q), "%s: want %s, got %s", name, wantStr, q.String())
}

func TestCompute(t *testing.T) {
	ctrl := gomock.NewController(t)
	nodeCache := fake.NewMockCacheInterface[*apiv3.Node](ctrl)

	nodes := []*apiv3.Node{
		newNode("c-1", "worker-linux"),
		newNode("c-1", "worker-windows", func(n *apiv3.Node) {
			n.Status.NodeLabels[corev1.LabelOSStable] = "windows"
		}),
		// Tainted control plane nodes are counted but not aggregated.
		newNode("c-1", "cp", func(n *apiv3.Node) {
			n.Spec.Worker = false
			n.Spec.ControlPlane = true
			n.Spec.InternalNodeSpec.Taints = []corev1.Taint{{Key: nodeRoleControlPlaneHyphen, Effect: corev1.TaintEffectNoSchedule}}
		}),
		// Cordoned nodes are counted but not aggregated.
		newNode("c-1", "cordoned", func(n *apiv3.Node) {
			n.Spec.InternalNodeSpec.Unschedulable = true
		}),
	}
	nodeCache.EXPECT().List("c-1", labels.Everything()).Return(nodes, nil)

	status, err := newAggregator(nodeCache).compute("c-1")
	require.NoError(t, err)

	assert.Equal(t, 4, status.NodeCount)
	assert.Equal(t, 1, status.LinuxWorkerCount)
	assert.Equal(t, 1, status.WindowsWorkerCount)
	assertQuantity(t, "8", status.Capacity, "cpu")
	assertQuantity(t, "16Gi", status.Capacity, "memory")
	assertQuantity(t, "220", status.Capacity, "pods")
	assertQuantity(t, "6", status.Allocatable, "cpu")
	assertQuantity(t, "2", status.Requested, "cpu")
	assertQuantity(t, "4Gi", status.Limits, "memory")
}

func TestComputeNodeNotSynced(t *testing.T) {
	ctrl := gomock.NewController(t)
	nodeCache := fake.NewMockCacheInterface[*apiv3.Node](ctrl)
	nodeCache.EXPECT().List("c-1", labels.Everything()).Return([]*apiv3.Node{
		newNode("c-1", "n", func(n *apiv3.Node) { n.Spec.Worker = false }),
	}, nil)

	_, err := newAggregator(nodeCache).compute("c-1")
	assert.Error(t, err)
}

func TestAggregatorCachesUntilNodeChanges(t *testing.T) {
	ctrl := gomock.NewController(t)
	nodeCache := fake.NewMockCacheInterface[*apiv3.Node](ctrl)
	a := newAggregator(nodeCache)

	nodes := []*apiv3.Node{newNode("c-1", "n1")}
	nodeCache.EXPECT().List("c-1", labels.Everything()).DoAndReturn(func(string, labels.Selector) ([]*apiv3.Node, error) {
		return nodes, nil
	}).Times(3)

	first := a.get("c-1")
	assert.Equal(t, 1, first.status.NodeCount)
	assert.NotEmpty(t, first.resourceVersion)

	// No node event: served from memory, no List call.
	assert.Equal(t, first, a.get("c-1"))

	// A node event that doesn't change the stats keeps the resourceVersion.
	_, _ = a.onNodeChange("c-1/n1", nodes[0])
	assert.Equal(t, first.resourceVersion, a.get("c-1").resourceVersion)

	// A new node changes the stats and the resourceVersion.
	nodes = append(nodes, newNode("c-1", "n2"))
	_, _ = a.onNodeChange("c-1/n2", nodes[1])
	second := a.get("c-1")
	assert.Equal(t, 2, second.status.NodeCount)
	assert.NotEqual(t, first.resourceVersion, second.resourceVersion)
}

func TestAggregatorNodeDeletion(t *testing.T) {
	ctrl := gomock.NewController(t)
	nodeCache := fake.NewMockCacheInterface[*apiv3.Node](ctrl)
	a := newAggregator(nodeCache)

	nodeCache.EXPECT().List("c-1", labels.Everything()).Return([]*apiv3.Node{newNode("c-1", "n1")}, nil)
	assert.Equal(t, 1, a.get("c-1").status.NodeCount)

	// Deleted nodes are passed as nil: the key alone must refresh the cluster.
	nodeCache.EXPECT().List("c-1", labels.Everything()).Return(nil, nil)
	_, _ = a.onNodeChange("c-1/n1", nil)
	assert.Equal(t, 0, a.get("c-1").status.NodeCount)
}

func TestAggregatorKeepsLastStatsOnError(t *testing.T) {
	ctrl := gomock.NewController(t)
	nodeCache := fake.NewMockCacheInterface[*apiv3.Node](ctrl)
	a := newAggregator(nodeCache)

	nodeCache.EXPECT().List("c-1", labels.Everything()).Return([]*apiv3.Node{newNode("c-1", "n1")}, nil)
	good := a.get("c-1")

	unsynced := newNode("c-1", "n2", func(n *apiv3.Node) { n.Spec.Worker = false })
	nodeCache.EXPECT().List("c-1", labels.Everything()).Return([]*apiv3.Node{newNode("c-1", "n1"), unsynced}, nil).Times(2)
	_, _ = a.onNodeChange("c-1/n2", unsynced)
	assert.Equal(t, good, a.get("c-1"))
	// Still dirty: the next read retries.
	assert.Equal(t, good, a.get("c-1"))
}

func TestAggregatorForgetsDeletedClusters(t *testing.T) {
	ctrl := gomock.NewController(t)
	nodeCache := fake.NewMockCacheInterface[*apiv3.Node](ctrl)
	a := newAggregator(nodeCache)

	nodeCache.EXPECT().List("c-1", labels.Everything()).Return(nil, nil)
	a.get("c-1")
	require.Contains(t, a.stats, "c-1")

	_, _ = a.onClusterChange("c-1", &apiv3.Cluster{})
	assert.Contains(t, a.stats, "c-1")

	_, _ = a.onClusterChange("c-1", nil)
	assert.NotContains(t, a.stats, "c-1")
}

func TestStoreGet(t *testing.T) {
	ctrl := gomock.NewController(t)
	clusterCache := fake.NewMockNonNamespacedCacheInterface[*apiv3.Cluster](ctrl)
	nodeCache := fake.NewMockCacheInterface[*apiv3.Node](ctrl)
	s := newStore(clusterCache, nodeCache)

	clusterCache.EXPECT().Get("c-1").Return(&apiv3.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c-1", UID: "uid-1", Labels: map[string]string{"env": "prod"}},
	}, nil)
	nodeCache.EXPECT().List("c-1", labels.Everything()).Return([]*apiv3.Node{newNode("c-1", "n1")}, nil)

	obj, err := s.Get(context.Background(), "c-1", &metav1.GetOptions{})
	require.NoError(t, err)
	stat := obj.(*ext.ClusterStat)
	assert.Equal(t, "c-1", stat.Name)
	assert.Equal(t, "uid-1", string(stat.UID))
	assert.Equal(t, map[string]string{"env": "prod"}, stat.Labels)
	assert.Equal(t, Kind, stat.Kind)
	assert.Equal(t, 1, stat.Status.NodeCount)
	assertQuantity(t, "4", stat.Status.Capacity, "cpu")

	// Mutating the returned object must not corrupt the cached stats.
	stat.Status.Capacity["cpu"] = resource.MustParse("999")
	clusterCache.EXPECT().Get("c-1").Return(&apiv3.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c-1"}}, nil)
	obj, err = s.Get(context.Background(), "c-1", &metav1.GetOptions{})
	require.NoError(t, err)
	assertQuantity(t, "4", obj.(*ext.ClusterStat).Status.Capacity, "cpu")
}

func TestStoreGetNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	clusterCache := fake.NewMockNonNamespacedCacheInterface[*apiv3.Cluster](ctrl)
	s := newStore(clusterCache, fake.NewMockCacheInterface[*apiv3.Node](ctrl))

	clusterCache.EXPECT().Get("nope").Return(nil, apierrors.NewNotFound(apiv3.Resource("clusters"), "nope"))
	_, err := s.Get(context.Background(), "nope", &metav1.GetOptions{})
	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err))
	assert.Equal(t, "clusterstats.ext.cattle.io \"nope\" not found", err.Error())
}

func TestStoreList(t *testing.T) {
	ctrl := gomock.NewController(t)
	clusterCache := fake.NewMockNonNamespacedCacheInterface[*apiv3.Cluster](ctrl)
	nodeCache := fake.NewMockCacheInterface[*apiv3.Node](ctrl)
	s := newStore(clusterCache, nodeCache)

	clusters := []*apiv3.Cluster{
		{ObjectMeta: metav1.ObjectMeta{Name: "c-2"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "c-1"}},
	}
	nodeCache.EXPECT().List("c-1", labels.Everything()).Return([]*apiv3.Node{newNode("c-1", "n1")}, nil).AnyTimes()
	nodeCache.EXPECT().List("c-2", labels.Everything()).Return(nil, nil).AnyTimes()

	t.Run("all", func(t *testing.T) {
		clusterCache.EXPECT().List(labels.Everything()).Return(clusters, nil)
		obj, err := s.List(context.Background(), &metainternalversion.ListOptions{})
		require.NoError(t, err)
		list := obj.(*ext.ClusterStatList)
		require.Len(t, list.Items, 2)
		assert.Equal(t, "c-1", list.Items[0].Name)
		assert.Equal(t, 1, list.Items[0].Status.NodeCount)
		assert.Equal(t, "c-2", list.Items[1].Name)
	})

	t.Run("label selector is passed to the cluster cache", func(t *testing.T) {
		selector := labels.SelectorFromSet(labels.Set{"env": "prod"})
		clusterCache.EXPECT().List(selector).Return(clusters[:1], nil)
		obj, err := s.List(context.Background(), &metainternalversion.ListOptions{LabelSelector: selector})
		require.NoError(t, err)
		assert.Len(t, obj.(*ext.ClusterStatList).Items, 1)
	})

	t.Run("metadata.name field selector", func(t *testing.T) {
		clusterCache.EXPECT().List(labels.Everything()).Return(clusters, nil)
		obj, err := s.List(context.Background(), &metainternalversion.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("metadata.name", "c-2"),
		})
		require.NoError(t, err)
		list := obj.(*ext.ClusterStatList)
		require.Len(t, list.Items, 1)
		assert.Equal(t, "c-2", list.Items[0].Name)
	})

	t.Run("unsupported field selector", func(t *testing.T) {
		_, err := s.List(context.Background(), &metainternalversion.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("status.nodeCount", "1"),
		})
		assert.True(t, apierrors.IsBadRequest(err))
	})
}

func TestConvertToTable(t *testing.T) {
	s := &Store{}
	stat := &ext.ClusterStat{
		ObjectMeta: metav1.ObjectMeta{Name: "c-1"},
		Status: ext.ClusterStatStatus{
			NodeCount:   2,
			Allocatable: map[string]resource.Quantity{"cpu": resource.MustParse("4"), "memory": resource.MustParse("8Gi"), "pods": resource.MustParse("110")},
			Requested:   map[string]resource.Quantity{"cpu": resource.MustParse("500m"), "memory": resource.MustParse("1Gi"), "pods": resource.MustParse("5")},
		},
	}
	table, err := s.ConvertToTable(context.Background(), &ext.ClusterStatList{Items: []ext.ClusterStat{*stat}}, nil)
	require.NoError(t, err)
	require.Len(t, table.Rows, 1)
	assert.Equal(t, []any{"c-1", "2", "500m/4", "1Gi/8Gi", "5/110"}, table.Rows[0].Cells)
}
