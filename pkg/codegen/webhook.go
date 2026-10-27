package main

// Generates pkg/generated/objects (admission-request unmarshal helpers used by
// the webhook handlers in pkg/webhook) and the aggregated webhook docs.

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"reflect"
	"strings"
	"text/template"

	auditlogv1 "github.com/rancher/rancher/pkg/apis/auditlog.cattle.io/v1"
	catalogv1 "github.com/rancher/rancher/pkg/apis/catalog.cattle.io/v1"
	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	provv1 "github.com/rancher/rancher/pkg/apis/provisioning.cattle.io/v1"
	rkev1 "github.com/rancher/rancher/pkg/apis/rke.cattle.io/v1"
	"github.com/rancher/wrangler/v3/pkg/controller-gen/args"
	"golang.org/x/tools/imports"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	webhookObjectsDir   = "pkg/generated/objects"
	webhookResourcesDir = "pkg/webhook/resources"
	webhookDocsFile     = "pkg/webhook/docs.md"
)

func generateWebhook() error {
	if err := generateWebhookDocs(webhookResourcesDir, webhookDocsFile); err != nil {
		return fmt.Errorf("generate webhook docs: %w", err)
	}
	return generateWebhookObjectsFromRequest(webhookObjectsDir, webhookObjectGroups())
}

func webhookObjectGroups() map[string]args.Group {
	return map[string]args.Group{
		"catalog.cattle.io": {
			Types: []interface{}{
				&catalogv1.ClusterRepo{},
			},
		},
		"management.cattle.io": {
			Types: []interface{}{
				&v3.Cluster{},
				&v3.ClusterRoleTemplateBinding{},
				&v3.Feature{},
				&v3.FleetWorkspace{},
				&v3.PodSecurityAdmissionConfigurationTemplate{},
				&v3.GlobalRole{},
				&v3.GlobalRoleBinding{},
				&v3.RoleTemplate{},
				&v3.ProjectRoleTemplateBinding{},
				&v3.NodeDriver{},
				&v3.Project{},
				&v3.ProxyEndpoint{},
				&v3.Setting{},
				&v3.AuthConfig{},
				&v3.User{},
			},
		},
		"provisioning.cattle.io": {
			Types: []interface{}{
				&provv1.Cluster{},
			},
		},
		"core": {
			Types: []interface{}{
				&unstructured.Unstructured{},
				&corev1.LimitRange{},
				&corev1.Namespace{},
				&corev1.ResourceQuota{},
				&corev1.Secret{},
			},
		},
		"autoscaling": {
			Types: []any{
				&autoscalingv1.Scale{},
			},
		},
		"rbac.authorization.k8s.io": {
			Types: []interface{}{
				&rbacv1.Role{},
				&rbacv1.RoleBinding{},
				&rbacv1.ClusterRole{},
				&rbacv1.ClusterRoleBinding{},
			},
		},
		"auditlog.cattle.io": {
			Types: []interface{}{
				&auditlogv1.AuditPolicy{},
			},
		},
		"rke.cattle.io": {
			Types: []interface{}{
				&rkev1.ETCDSnapshot{},
				&rkev1.RKEControlPlane{},
			},
		},
	}
}

type webhookTypeInfo struct {
	Name, Type, Package string
}

func generateWebhookObjectsFromRequest(outputDir string, groups map[string]args.Group) error {
	temp := template.Must(template.New("objectsFromRequest").Funcs(template.FuncMap{
		"replace": strings.ReplaceAll,
	}).Parse(webhookObjectsFromRequestTemplate))

	for groupName, group := range groups {
		var packageName string
		types := make([]webhookTypeInfo, 0, len(group.Types))

		for _, t := range group.Types {
			rt := reflect.TypeOf(t)
			ti := webhookTypeInfo{
				Type: rt.String(),
			}
			if rt.Kind() == reflect.Pointer {
				// PkgPath returns an empty string for pointers
				// Elem returns a Type associated with the dereferenced type.
				rt = rt.Elem()
			}
			ti.Package = rt.PkgPath()
			ti.Name = rt.Name()
			packageName = path.Base(ti.Package)
			types = append(types, ti)
		}

		groupDir := path.Join(outputDir, groupName, packageName)
		if err := os.MkdirAll(groupDir, 0755); err != nil {
			return err
		}

		data := map[string]interface{}{
			"types":   types,
			"package": packageName,
		}

		var content bytes.Buffer
		if err := temp.Execute(&content, data); err != nil {
			return err
		}

		if err := gofmtAndWriteWebhookFile(path.Join(groupDir, "objects.go"), content.Bytes()); err != nil {
			return err
		}
	}

	return nil
}

func gofmtAndWriteWebhookFile(path string, content []byte) error {
	formatted, err := imports.Process(path, content, &imports.Options{FormatOnly: true, Comments: true})
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
	}()

	_, err = f.Write(formatted)
	return err
}

const webhookObjectsFromRequestTemplate = `
package {{ .package }}

import (
	"encoding/json"
	"fmt"

	{{ range .types }}
	"{{ .Package }}"{{ end }}
	admissionv1 "k8s.io/api/admission/v1"
)
{{ range .types }}

// {{ .Name }}OldAndNewFromRequest gets the old and new {{ .Name }} objects, respectively, from the webhook request.
// If the request is a Delete operation, then the new object is the zero value for {{ .Name }}.
// Similarly, if the request is a Create operation, then the old object is the zero value for {{ .Name }}.
func {{ .Name }}OldAndNewFromRequest(request *admissionv1.AdmissionRequest) ({{ .Type }}, {{ .Type }}, error) {
	if request == nil {
		return nil, nil, fmt.Errorf("nil request")
	}

	object := {{ replace .Type "*" "&" }}{}
	oldObject := {{ replace .Type "*" "&" }}{}

	if request.Operation != admissionv1.Delete {
		err := json.Unmarshal(request.Object.Raw, object)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to unmarshal request object: %w", err)
		}
	}

	if request.Operation == admissionv1.Create {
		return oldObject, object, nil
	}

	err := json.Unmarshal(request.OldObject.Raw, oldObject)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal request oldObject: %w", err)
	}

	return oldObject, object, nil
}

// {{ .Name }}FromRequest returns a {{ .Name }} object from the webhook request.
// If the operation is a Delete operation, then the old object is returned.
// Otherwise, the new object is returned.
func {{ .Name }}FromRequest(request *admissionv1.AdmissionRequest) ({{ .Type }}, error) {
	if request == nil {
		return nil, fmt.Errorf("nil request")
	}

	object := {{ replace .Type "*" "&" }}{}
	raw := request.Object.Raw
	
	if request.Operation == admissionv1.Delete {
		raw = request.OldObject.Raw
	}

	err := json.Unmarshal(raw, object)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal request object: %w", err)
	}

	return object, nil
}
{{ end }}
`
