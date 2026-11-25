package clusterstats

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	ext "github.com/rancher/rancher/pkg/apis/ext.cattle.io/v1"
	apiv3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	generatedopenapi "github.com/rancher/rancher/pkg/generated/openapi"
	steveext "github.com/rancher/steve/pkg/ext"
	"github.com/rancher/wrangler/v3/pkg/generic/fake"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	openapicommon "k8s.io/kube-openapi/pkg/common"
)

// TestExtensionAPIServer installs the store in a real extension API server and
// checks that only the get and list verbs are served.
func TestExtensionAPIServer(t *testing.T) {
	ctrl := gomock.NewController(t)
	clusterCache := fake.NewMockNonNamespacedCacheInterface[*apiv3.Cluster](ctrl)
	nodeCache := fake.NewMockCacheInterface[*apiv3.Node](ctrl)
	cluster := &apiv3.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c-1"}}
	clusterCache.EXPECT().Get("c-1").Return(cluster, nil).AnyTimes()
	clusterCache.EXPECT().List(gomock.Any()).Return([]*apiv3.Cluster{cluster}, nil).AnyTimes()
	nodeCache.EXPECT().List("c-1", labels.Everything()).Return([]*apiv3.Node{newNode("c-1", "n1")}, nil).AnyTimes()

	scheme := runtime.NewScheme()
	steveext.AddToScheme(scheme)
	require.NoError(t, ext.AddToScheme(scheme))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	server, err := steveext.NewExtensionAPIServer(scheme, serializer.NewCodecFactory(scheme), steveext.ExtensionAPIServerOptions{
		GetOpenAPIDefinitions: func(ref openapicommon.ReferenceCallback) map[string]openapicommon.OpenAPIDefinition {
			return generatedopenapi.GetOpenAPIDefinitions(ref)
		},
		OpenAPIDefinitionNameReplacements: map[string]string{
			"com.github.rancher.rancher.pkg.apis.ext.cattle.io.v1": "io.cattle.ext.v1",
		},
		Listener: ln,
		Authenticator: authenticator.RequestFunc(func(*http.Request) (*authenticator.Response, bool, error) {
			return &authenticator.Response{User: &user.DefaultInfo{Name: "admin"}}, true, nil
		}),
		Authorizer: authorizer.AuthorizerFunc(func(context.Context, authorizer.Attributes) (authorizer.Decision, string, error) {
			return authorizer.DecisionAllow, "", nil
		}),
	})
	require.NoError(t, err)
	require.NoError(t, server.Install(ext.ClusterStatResourceName, GVK, newStore(clusterCache, nodeCache)))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, server.Run(ctx))

	do := func(method, path string, body string) (int, []byte) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)
		b, _ := io.ReadAll(w.Result().Body)
		return w.Code, b
	}

	t.Run("discovery advertises only get and list", func(t *testing.T) {
		code, body := do(http.MethodGet, "/apis/ext.cattle.io/v1", "")
		require.Equal(t, http.StatusOK, code, string(body))
		var list metav1.APIResourceList
		require.NoError(t, json.Unmarshal(body, &list))
		idx := slices.IndexFunc(list.APIResources, func(r metav1.APIResource) bool { return r.Name == "clusterstats" })
		require.NotEqual(t, -1, idx, string(body))
		r := list.APIResources[idx]
		assert.Equal(t, "ClusterStat", r.Kind)
		assert.Equal(t, "clusterstat", r.SingularName)
		assert.False(t, r.Namespaced)
		assert.ElementsMatch(t, []string{"get", "list"}, []string(r.Verbs))
	})

	t.Run("get", func(t *testing.T) {
		code, body := do(http.MethodGet, "/apis/ext.cattle.io/v1/clusterstats/c-1", "")
		require.Equal(t, http.StatusOK, code, string(body))
		var stat ext.ClusterStat
		require.NoError(t, json.Unmarshal(body, &stat))
		assert.Equal(t, "c-1", stat.Name)
		assert.Equal(t, 1, stat.Status.NodeCount)
		assertQuantity(t, "4", stat.Status.Capacity, "cpu")
	})

	t.Run("list", func(t *testing.T) {
		code, body := do(http.MethodGet, "/apis/ext.cattle.io/v1/clusterstats", "")
		require.Equal(t, http.StatusOK, code, string(body))
		var list ext.ClusterStatList
		require.NoError(t, json.Unmarshal(body, &list))
		require.Len(t, list.Items, 1)
		assert.Equal(t, "c-1", list.Items[0].Name)
	})

	t.Run("table", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/apis/ext.cattle.io/v1/clusterstats", nil)
		req.Header.Set("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var table metav1.Table
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &table))
		require.Len(t, table.Rows, 1)
		assert.Equal(t, []any{"c-1", "1", "1/3", "1Gi/7Gi", "10/100"}, table.Rows[0].Cells)
	})

	for _, tc := range []struct {
		name, method, path, body string
	}{
		{"watch", http.MethodGet, "/apis/ext.cattle.io/v1/clusterstats?watch=true", ""},
		{"watch single", http.MethodGet, "/apis/ext.cattle.io/v1/clusterstats?watch=true&fieldSelector=metadata.name%3Dc-1", ""},
		{"create", http.MethodPost, "/apis/ext.cattle.io/v1/clusterstats", `{"apiVersion":"ext.cattle.io/v1","kind":"ClusterStat","metadata":{"name":"c-2"}}`},
		{"update", http.MethodPut, "/apis/ext.cattle.io/v1/clusterstats/c-1", `{"apiVersion":"ext.cattle.io/v1","kind":"ClusterStat","metadata":{"name":"c-1"}}`},
		{"delete", http.MethodDelete, "/apis/ext.cattle.io/v1/clusterstats/c-1", ""},
		{"deletecollection", http.MethodDelete, "/apis/ext.cattle.io/v1/clusterstats", ""},
	} {
		t.Run(tc.name+" is not allowed", func(t *testing.T) {
			code, body := do(tc.method, tc.path, tc.body)
			assert.Equal(t, http.StatusMethodNotAllowed, code, string(body))
		})
	}
}
