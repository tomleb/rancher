package apiservice

import (
	"fmt"
	"os"

	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/features"
	"github.com/rancher/rancher/pkg/namespace"
	"github.com/rancher/rancher/pkg/settings"
	"github.com/rancher/rancher/pkg/utils"
	corev1 "k8s.io/api/core/v1"
)

const RancherServiceName = "rancher"
const RancherInternalServiceName = "rancher-internal"

// appSelectorEnvVar carries the chart's rancher.fullname, which names the
// Rancher Deployment, the pods' "app" label, and the Services
// ("<fullname>" and "<fullname>-internal"). It is unset outside the chart
// (e.g. the Docker install, where setupRancherService creates the literal
// rancher and rancher-internal Services). See chart/templates/deployment.yaml
// and chart/templates/service.yaml.
const appSelectorEnvVar = "IMPERATIVE_API_APP_SELECTOR"

// RancherFullname returns the name of the Rancher server's Deployment and
// Service in cattle-system: the chart's rancher.fullname, or "rancher" when
// not installed by the chart. It is only "rancher" in a chart install whose
// release fullname happens to be "rancher".
func RancherFullname() string {
	if fullname := os.Getenv(appSelectorEnvVar); fullname != "" {
		return fullname
	}
	return RancherServiceName
}

// AgentServiceName is the Service fronting the cattle-cluster-agent pods on a
// downstream cluster. It plays the role of RancherInternalServiceName there.
const AgentServiceName = "cattle-cluster-agent"

// InternalServiceName returns the name of the Service in cattle-system that
// fronts this process's internal (:444) listener:
//   - cattle-cluster-agent when running as the downstream agent;
//   - "<RancherFullname>-internal" otherwise (rancher-internal unless the
//     chart's fullname differs).
//
// The internal server URL and the tls-rancher-internal cert's SANs must both
// be derived from it, or clients of the internal URL fail the TLS handshake.
func InternalServiceName() string {
	if features.MCMAgent.Enabled() {
		return AgentServiceName
	}
	return RancherFullname() + "-internal"
}

func (h *handler) SetupInternalServerURL(key string, setting *v3.Setting) (*v3.Setting, error) {
	if key != settings.ServerURL.Name {
		return setting, nil
	}

	internalURL, internalCA, err := h.getInternalServerAndURL()
	if err != nil {
		return nil, err
	}

	// purposely update CA before URL, because we only wait for internalURL != "" when checking if it's initialized
	if settings.InternalCACerts.Get() != internalCA {
		if err := settings.InternalCACerts.Set(internalCA); err != nil {
			return setting, err
		}
	}

	if settings.InternalServerURL.Get() != internalURL {
		if err := settings.InternalServerURL.Set(internalURL); err != nil {
			return setting, err
		}
	}

	return setting, nil
}

func (h *handler) getClusterIP() (string, error) {
	serviceName := InternalServiceName()
	service, err := h.services.Get(namespace.System, serviceName)
	if err != nil {
		return "", err
	}
	if service.Spec.ClusterIP == "" {
		return "", fmt.Errorf("waiting on service %s/%s to be assigned a ClusterIP", namespace.System, serviceName)
	}
	return service.Spec.ClusterIP, nil
}

func (h *handler) getInternalServerAndURL() (string, string, error) {
	serverURL := settings.ServerURL.Get()
	ca := settings.CACerts.Get()

	tlsSecret, err := h.secretsCache.Get(namespace.System, "tls-rancher-internal-ca")
	if err != nil {
		return "", "", err
	}
	internalCA := string(tlsSecret.Data[corev1.TLSCertKey])

	clusterIPService := false
	if features.MCMAgent.Enabled() {
		if _, err := h.deploymentCache.Get(namespace.System, AgentServiceName); err == nil {
			clusterIPService = true
		}
	} else {
		if dp, err := h.deploymentCache.Get(namespace.System, RancherFullname()); err == nil && dp.Spec.Replicas != nil && *dp.Spec.Replicas != 0 {
			clusterIPService = true
		} else if _, err := h.daemonSetCache.Get(namespace.System, RancherFullname()); err == nil {
			clusterIPService = true
		}
	}

	if h.embedded {
		clusterIPService = true
	}

	if clusterIPService {
		clusterIP, err := h.getClusterIP()
		if utils.IsPlainIPV6(clusterIP) {
			clusterIP = fmt.Sprintf("[%s]", clusterIP)
		}
		return fmt.Sprintf("https://%s", clusterIP), internalCA, err
	}

	return serverURL, ca, nil
}
