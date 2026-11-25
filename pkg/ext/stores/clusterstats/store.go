// Package clusterstats implements the store for the ClusterStat resource,
// statistics aggregated in memory from the management.cattle.io Nodes of a
// cluster. Only the get and list verbs are supported.
package clusterstats

import (
	"context"
	"fmt"
	"slices"
	"strings"

	ext "github.com/rancher/rancher/pkg/apis/ext.cattle.io/v1"
	apiv3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	v3 "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/wrangler"
	steveext "github.com/rancher/steve/pkg/ext"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/rest"
)

const (
	Kind         = "ClusterStat"
	SingularName = "clusterstat"
)

var (
	GVK = ext.SchemeGroupVersion.WithKind(Kind)
	GVR = ext.SchemeGroupVersion.WithResource(ext.ClusterStatResourceName)
)

var (
	_ rest.Getter                   = &Store{}
	_ rest.Lister                   = &Store{}
	_ rest.Storage                  = &Store{}
	_ rest.Scoper                   = &Store{}
	_ rest.SingularNameProvider     = &Store{}
	_ rest.GroupVersionKindProvider = &Store{}
)

// Store serves ClusterStats from memory.
//
// Authorization is entirely done by the extension API server before the store
// is called: users get the get verb on the ClusterStat of the clusters they are
// members of through the cluster membership roles (see
// pkg/controllers/management/auth/project_cluster). The store therefore
// doesn't filter anything itself.
//
// +k8s:openapi-gen=false
// +k8s:deepcopy-gen=false
type Store struct {
	clusterCache v3.ClusterCache
	aggregator   *aggregator
}

// New creates the store and registers the handlers that keep the stats up to
// date as the management.cattle.io Nodes change.
func New(ctx context.Context, wranglerCtx *wrangler.Context) *Store {
	s := newStore(wranglerCtx.Mgmt.Cluster().Cache(), wranglerCtx.Mgmt.Node().Cache())
	s.aggregator.register(ctx, wranglerCtx.Mgmt.Node(), wranglerCtx.Mgmt.Cluster())
	return s
}

func newStore(clusterCache v3.ClusterCache, nodeCache v3.NodeCache) *Store {
	return &Store{
		clusterCache: clusterCache,
		aggregator:   newAggregator(nodeCache),
	}
}

// GroupVersionKind implements [rest.GroupVersionKindProvider].
func (s *Store) GroupVersionKind(_ schema.GroupVersion) schema.GroupVersionKind {
	return GVK
}

// NamespaceScoped implements [rest.Scoper].
func (s *Store) NamespaceScoped() bool {
	return false
}

// GetSingularName implements [rest.SingularNameProvider].
func (s *Store) GetSingularName() string {
	return SingularName
}

// New implements [rest.Storage].
func (s *Store) New() runtime.Object {
	return &ext.ClusterStat{}
}

// NewList implements [rest.Lister].
func (s *Store) NewList() runtime.Object {
	return &ext.ClusterStatList{}
}

// Destroy implements [rest.Storage].
func (s *Store) Destroy() {}

// Get implements [rest.Getter].
func (s *Store) Get(_ context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	cluster, err := s.clusterCache.Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, apierrors.NewNotFound(GVR.GroupResource(), name)
		}
		return nil, apierrors.NewInternalError(fmt.Errorf("error getting cluster %s: %w", name, err))
	}

	return s.toClusterStat(cluster), nil
}

// List implements [rest.Lister].
//
// Label selectors match the labels of the management.cattle.io Clusters, which
// are copied to the ClusterStats. Field selectors are only supported on
// metadata.name.
func (s *Store) List(_ context.Context, options *metainternalversion.ListOptions) (runtime.Object, error) {
	selector := labels.Everything()
	if options != nil && options.LabelSelector != nil {
		selector = options.LabelSelector
	}

	var nameFilter string
	if options != nil && options.FieldSelector != nil && !options.FieldSelector.Empty() {
		for _, req := range options.FieldSelector.Requirements() {
			if req.Field != "metadata.name" || req.Operator == "!=" {
				return nil, apierrors.NewBadRequest(fmt.Sprintf("unsupported field selector %q: only metadata.name= is supported", options.FieldSelector.String()))
			}
			nameFilter = req.Value
		}
	}

	clusters, err := s.clusterCache.List(selector)
	if err != nil {
		return nil, apierrors.NewInternalError(fmt.Errorf("error listing clusters: %w", err))
	}

	slices.SortFunc(clusters, func(a, b *apiv3.Cluster) int {
		return strings.Compare(a.Name, b.Name)
	})

	list := &ext.ClusterStatList{
		Items: make([]ext.ClusterStat, 0, len(clusters)),
	}
	for _, cluster := range clusters {
		if nameFilter != "" && cluster.Name != nameFilter {
			continue
		}
		list.Items = append(list.Items, *s.toClusterStat(cluster))
	}

	return list, nil
}

func (s *Store) toClusterStat(cluster *apiv3.Cluster) *ext.ClusterStat {
	stats := s.aggregator.get(cluster.Name)

	var lbls map[string]string
	if len(cluster.Labels) > 0 {
		lbls = make(map[string]string, len(cluster.Labels))
		for k, v := range cluster.Labels {
			lbls[k] = v
		}
	}

	return &ext.ClusterStat{
		TypeMeta: metav1.TypeMeta{
			APIVersion: GVK.GroupVersion().String(),
			Kind:       Kind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              cluster.Name,
			UID:               cluster.UID,
			CreationTimestamp: cluster.CreationTimestamp,
			Labels:            lbls,
			ResourceVersion:   stats.resourceVersion,
		},
		Status: *stats.status.DeepCopy(),
	}
}

// ConvertToTable implements [rest.TableConvertor].
func (s *Store) ConvertToTable(ctx context.Context, object runtime.Object, tableOptions runtime.Object) (*metav1.Table, error) {
	return steveext.ConvertToTable(ctx, object, tableOptions, GVR.GroupResource(), columnDefinitions, printClusterStat)
}

var columnDefinitions = []metav1.TableColumnDefinition{
	{Name: "Name", Type: "string", Format: "name", Description: metav1.ObjectMeta{}.SwaggerDoc()["name"]},
	{Name: "Nodes", Type: "integer", Description: "NodeCount is the number of nodes in the cluster"},
	{Name: "CPU", Type: "string", Description: "Requested / allocatable CPU"},
	{Name: "Memory", Type: "string", Description: "Requested / allocatable memory"},
	{Name: "Pods", Type: "string", Description: "Requested / allocatable pods"},
}

func printClusterStat(stat *ext.ClusterStat) []string {
	usage := func(name string) string {
		requested := stat.Status.Requested[name]
		allocatable := stat.Status.Allocatable[name]
		return requested.String() + "/" + allocatable.String()
	}
	return []string{
		stat.Name,
		fmt.Sprint(stat.Status.NodeCount),
		usage("cpu"),
		usage("memory"),
		usage("pods"),
	}
}
