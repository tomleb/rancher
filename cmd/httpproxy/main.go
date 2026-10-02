// Command httpproxy runs pkg/httpproxy's "/proxy/" handler as a standalone HTTP server, for
// exercising changes to that package against a real cluster without standing up all of Rancher.
//
// It builds the handful of real, cluster-backed caches httpproxy needs (management.cattle.io
// Clusters and ProxyEndpoints, provisioning.cattle.io Clusters, and core Secrets) directly from a
// kubeconfig, instead of the full *config.ScaledContext the production server assembles.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rancher/lasso/pkg/controller"
	"github.com/rancher/rancher/pkg/controllers"
	management "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io"
	provisioning "github.com/rancher/rancher/pkg/generated/controllers/provisioning.cattle.io"
	"github.com/rancher/rancher/pkg/httpproxy"
	"github.com/rancher/rancher/pkg/settings"
	"github.com/rancher/rancher/pkg/wrangler"
	"github.com/rancher/steve/pkg/auth"
	wranglercore "github.com/rancher/wrangler/v3/pkg/generated/controllers/core"
	"github.com/rancher/wrangler/v3/pkg/generic"
	"github.com/rancher/wrangler/v3/pkg/signals"
	"github.com/sirupsen/logrus"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizerfactory"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	var (
		addr          string
		kubeconfig    string
		kubeContext   string
		allowedHosts  string
		asUser        string
		asGroups      string
		cacheSyncWait time.Duration
	)
	// A local FlagSet avoids colliding with flags that transitively imported packages (e.g.
	// sigs.k8s.io/controller-runtime) register on the global flag.CommandLine at init time.
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	fs.StringVar(&addr, "addr", ":8080", "address to serve the /proxy/ handler on")
	fs.StringVar(&kubeconfig, "kubeconfig", "", "path to a kubeconfig file (defaults to $KUBECONFIG or ~/.kube/config)")
	fs.StringVar(&kubeContext, "context", "", "kubeconfig context to use (defaults to the current context)")
	fs.StringVar(&allowedHosts, "allowed-hosts", "", "comma-separated list of hosts the proxy is allowed to reach; overrides the whitelist-domain setting default")
	fs.StringVar(&asUser, "as", "httpproxy-standalone", "username to impersonate for authorization checks against the cluster")
	fs.StringVar(&asGroups, "as-group", "system:masters", "comma-separated list of groups to impersonate for authorization checks")
	fs.DurationVar(&cacheSyncWait, "cache-sync-timeout", time.Minute, "how long to wait for the cluster caches to sync on startup")
	_ = fs.Parse(os.Args[1:])

	if allowedHosts != "" {
		if err := settings.WhitelistDomain.Set(allowedHosts); err != nil {
			logrus.Fatalf("failed to set allowed hosts: %v", err)
		}
	}

	ctx := signals.SetupSignalContext()

	restConfig, err := buildRestConfig(kubeconfig, kubeContext)
	if err != nil {
		logrus.Fatalf("failed to build kubeconfig: %v", err)
	}

	k8sClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		logrus.Fatalf("failed to build k8s client: %v", err)
	}

	controllerFactory, err := controller.NewSharedControllerFactoryFromConfigWithOptions(
		restConfig, wrangler.Scheme, controllers.GetOptsFromEnv(controllers.Scaled))
	if err != nil {
		logrus.Fatalf("failed to build controller factory: %v", err)
	}
	factoryOpts := &generic.FactoryOptions{SharedControllerFactory: controllerFactory}

	mgmtFactory, err := management.NewFactoryFromConfigWithOptions(restConfig, factoryOpts)
	if err != nil {
		logrus.Fatalf("failed to build management.cattle.io controllers: %v", err)
	}
	provFactory, err := provisioning.NewFactoryFromConfigWithOptions(restConfig, factoryOpts)
	if err != nil {
		logrus.Fatalf("failed to build provisioning.cattle.io controllers: %v", err)
	}
	coreFactory, err := wranglercore.NewFactoryFromConfigWithOptions(restConfig, factoryOpts)
	if err != nil {
		logrus.Fatalf("failed to build core controllers: %v", err)
	}

	mgmtV3 := mgmtFactory.Management().V3()
	mgmtClustersCache := mgmtV3.Cluster().Cache()
	proxyEndpointCache := mgmtV3.ProxyEndpoint().Cache()
	provClustersCache := provFactory.Provisioning().V1().Cluster().Cache()
	secretsCache := coreFactory.Core().V1().Secret().Cache()

	if err := controllerFactory.SharedCacheFactory().Start(ctx); err != nil {
		logrus.Fatalf("failed to start caches: %v", err)
	}
	syncCtx, cancel := context.WithTimeout(ctx, cacheSyncWait)
	defer cancel()
	for gvk, synced := range controllerFactory.SharedCacheFactory().WaitForCacheSync(syncCtx) {
		if !synced {
			logrus.Warnf("cache for %s did not sync before startup timeout; related lookups may come back empty", gvk)
		}
	}

	authorizerConfig, err := (&authorizerfactory.DelegatingAuthorizerConfig{
		SubjectAccessReviewClient: k8sClient.AuthorizationV1(),
		AllowCacheTTL:             time.Second * time.Duration(settings.AuthorizationCacheTTLSeconds.GetInt()),
		DenyCacheTTL:              time.Second * time.Duration(settings.AuthorizationDenyCacheTTLSeconds.GetInt()),
		WebhookRetryBackoff:       &auth.WebhookBackoff,
	}).New()
	if err != nil {
		logrus.Fatalf("failed to build authorizer: %v", err)
	}

	proxyHandler, err := httpproxy.NewProxyWithDeps("/proxy/", whitelistSupplier, httpproxy.Dependencies{
		Credentials:        secretsCache,
		MgmtClustersCache:  mgmtClustersCache,
		ProvClustersCache:  provClustersCache,
		ProxyEndpointCache: proxyEndpointCache,
		Authorizer:         authorizerConfig,
	})
	if err != nil {
		logrus.Fatalf("failed to build proxy handler: %v", err)
	}

	impersonated := &user.DefaultInfo{Name: asUser, Groups: strings.Split(asGroups, ",")}
	mux := http.NewServeMux()
	mux.Handle("/proxy/", impersonate(impersonated, logRequests(proxyHandler)))

	logrus.Infof("serving /proxy/ on %s as user %q (groups: %v), allowed hosts: %v", addr, asUser, impersonated.Groups, whitelistSupplier())
	server := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logrus.Fatalf("server failed: %v", err)
	}
}

func whitelistSupplier() []string {
	v := settings.WhitelistDomain.Get()
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}

func impersonate(u *user.DefaultInfo, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(request.WithUser(r.Context(), u)))
	})
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logrus.Infof("proxying %s %s", r.Method, r.URL.String())
		next.ServeHTTP(w, r)
	})
}

func buildRestConfig(kubeconfig, kubeContext string) (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		loadingRules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if kubeContext != "" {
		overrides.CurrentContext = kubeContext
	}
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	restConfig.QPS = 50
	restConfig.Burst = 100
	return restConfig, nil
}
