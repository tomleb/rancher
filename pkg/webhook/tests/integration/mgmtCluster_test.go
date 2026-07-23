package integration_test

import (
	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (m *IntegrationSuite) TestManagementCluster() {
	// NOTE: RKE1 (RancherKubernetesEngineConfig) support was removed from rancher's
	// management.cattle.io/v3 ClusterSpecBase type; this test previously exercised the
	// RKE1-specific creation path and has been updated to use a non-RKE1 (K3s) config.
	newObj := func() *v3.Cluster { return &v3.Cluster{} }
	validCreateObj := &v3.Cluster{
		ObjectMeta: v1.ObjectMeta{
			Name: "test-cluster",
		},
		Spec: v3.ClusterSpec{
			K3sConfig: &v3.K3sConfig{},
		},
	}

	validDelete := func() *v3.Cluster {
		return validCreateObj
	}
	endPoints := &endPointObjs[*v3.Cluster]{
		invalidCreate:  nil,
		newObj:         newObj,
		validCreateObj: validCreateObj,
		invalidUpdate:  nil,
		validUpdate:    nil,
		validDelete:    validDelete,
	}
	validateEndpoints(m.T(), endPoints, m.clientFactory)
}
