package cluster

import (
	"fmt"
	"net/http"

	apisv3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/webhook/admission"
	v3 "github.com/rancher/rancher/pkg/generated/controllers/management.cattle.io/v3"
	objectsv3 "github.com/rancher/rancher/pkg/webhook/generated/objects/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/webhook/resources/common"
	admissionv1 "k8s.io/api/admission/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	v1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	authorizationv1 "k8s.io/client-go/kubernetes/typed/authorization/v1"
)

// NewValidator returns a new validator for management clusters.
func NewValidator(
	sar authorizationv1.SubjectAccessReviewInterface,
	userCache v3.UserCache,
) *Validator {
	return &Validator{
		admitter: admitter{
			sar:       sar,
			userCache: userCache, // userCache is nil for downstream clusters.
		},
	}
}

// Validator ValidatingWebhook for management clusters.
type Validator struct {
	admitter admitter
}

var managementGVR = schema.GroupVersionResource{
	Group:    "management.cattle.io",
	Version:  "v3",
	Resource: "clusters",
}

// GVR returns the GroupVersionKind for this CRD.
func (v *Validator) GVR() schema.GroupVersionResource {
	return managementGVR
}

// Operations returns list of operations handled by this validator.
func (v *Validator) Operations() []admissionregistrationv1.OperationType {
	return []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update, admissionregistrationv1.Delete}
}

// ValidatingWebhook returns the ValidatingWebhook used for this CRD.
func (v *Validator) ValidatingWebhook(clientConfig admissionregistrationv1.WebhookClientConfig) []admissionregistrationv1.ValidatingWebhook {
	valWebhook := admission.NewDefaultValidatingWebhook(v, clientConfig, admissionregistrationv1.ClusterScope, v.Operations())
	valWebhook.FailurePolicy = admission.Ptr(admissionregistrationv1.Ignore)
	return []admissionregistrationv1.ValidatingWebhook{*valWebhook}
}

// Admitters returns the admitter objects used to validate clusters.
func (v *Validator) Admitters() []admission.Admitter {
	return []admission.Admitter{&v.admitter}
}

type admitter struct {
	sar       authorizationv1.SubjectAccessReviewInterface
	userCache v3.UserCache
}

// Admit handles the webhook admission request sent to this webhook.
func (a *admitter) Admit(request *admission.Request) (*admissionv1.AdmissionResponse, error) {
	oldCluster, newCluster, err := objectsv3.ClusterOldAndNewFromRequest(&request.AdmissionRequest)
	if err != nil {
		return nil, fmt.Errorf("failed get old and new clusters from request: %w", err)
	}

	response, err := a.validateFleetPermissions(request, oldCluster, newCluster)
	if err != nil {
		return nil, fmt.Errorf("failed to validate fleet permissions: %w", err)
	}
	if !response.Allowed {
		return response, nil
	}

	if a.userCache != nil {
		// The following checks don't make sense for downstream clusters (userCache == nil)
		if request.Operation == admissionv1.Create {
			fieldErr, err := common.CheckCreatorPrincipalName(a.userCache, newCluster)
			if err != nil {
				return nil, fmt.Errorf("error checking creator principal: %w", err)
			}
			if fieldErr != nil {
				return admission.ResponseBadRequest(fieldErr.Error()), nil
			}
		} else if request.Operation == admissionv1.Update {
			if fieldErr := common.CheckCreatorAnnotationsOnUpdate(oldCluster, newCluster); fieldErr != nil {
				return admission.ResponseBadRequest(fieldErr.Error()), nil
			}
		}
	}

	// Note: PSACT (PodSecurityAdmissionConfigurationTemplate) validation against RKE1's
	// kube-apiserver AdmissionConfiguration used to run here. RKE1 (RancherKubernetesEngineConfig)
	// was removed from rancher/rancher (commit 1f22fcaec, "Remove rancher/rke dependency"), so
	// RKE1 clusters can no longer exist and this validation path was removed (see the dedicated
	// RKE1-removal commit for validatePSACT/checkPSAConfigOnCluster).

	return admission.ResponseAllowed(), nil
}

func toExtra(extra map[string]authenticationv1.ExtraValue) map[string]v1.ExtraValue {
	result := map[string]v1.ExtraValue{}
	for k, v := range extra {
		result[k] = v1.ExtraValue(v)
	}
	return result
}

// validateFleetPermissions validates whether the request maker has required permissions around FleetWorkspace.
func (a *admitter) validateFleetPermissions(request *admission.Request, oldCluster, newCluster *apisv3.Cluster) (*admissionv1.AdmissionResponse, error) {
	// Ensure that the FleetWorkspaceName field cannot be unset once it is set, as it would cause (likely unintentional)
	// cluster deletion. Note that we're only enforcing this rule on UPDATE because Spec.FleetWorkspaceName will be
	// empty on cluster deletion, which is fine.
	fleetWorkspaceUnset := newCluster.Spec.FleetWorkspaceName == "" && oldCluster.Spec.FleetWorkspaceName != ""
	if request.Operation == admissionv1.Update && fleetWorkspaceUnset {
		return &admissionv1.AdmissionResponse{
			Result: &metav1.Status{
				Status:  "Failure",
				Message: "once set, field FleetWorkspaceName cannot be made empty",
				Reason:  metav1.StatusReasonInvalid,
				Code:    http.StatusBadRequest,
			},
			Allowed: false,
		}, nil
	}

	// If the FleetWorkspaceName is empty or unchanged, there's no need to make a SAR request.
	if newCluster.Spec.FleetWorkspaceName == "" || oldCluster.Spec.FleetWorkspaceName == newCluster.Spec.FleetWorkspaceName {
		return &admissionv1.AdmissionResponse{
			Allowed: true,
		}, nil
	}

	resp, err := a.sar.Create(request.Context, &v1.SubjectAccessReview{
		Spec: v1.SubjectAccessReviewSpec{
			ResourceAttributes: &v1.ResourceAttributes{
				Verb:     "fleetaddcluster",
				Version:  "v3",
				Resource: "fleetworkspaces",
				Group:    "management.cattle.io",
				Name:     newCluster.Spec.FleetWorkspaceName,
			},
			User:   request.UserInfo.Username,
			Groups: request.UserInfo.Groups,
			Extra:  toExtra(request.UserInfo.Extra),
			UID:    request.UserInfo.UID,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to check SubjectAccessReview for cluster [%s]: %w", newCluster.Name, err)
	}

	if !resp.Status.Allowed {
		return &admissionv1.AdmissionResponse{
			Result: &metav1.Status{
				Status:  "Failure",
				Message: resp.Status.Reason,
				Reason:  metav1.StatusReasonUnauthorized,
				Code:    http.StatusUnauthorized,
			},
			Allowed: false,
		}, nil
	}

	return admission.ResponseAllowed(), nil
}

// validatePSACT validates the cluster spec when PodSecurityAdmissionConfigurationTemplate is used.
//
// NOTE: RKE1 (RancherKubernetesEngineConfig) support was removed from rancher/rancher
// (commit 1f22fcaec, "Remove rancher/rke dependency"). This function relied entirely on
// RKE1-specific fields (Spec.RancherKubernetesEngineConfig) that no longer exist on
// ClusterSpec, and has been deleted along with its helper checkPSAConfigOnCluster.
// See the git commit message for this removal for full context; needs sign-off from
// another team before merging.

