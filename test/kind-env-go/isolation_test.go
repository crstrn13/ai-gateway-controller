//go:build e2e

package kindenv

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

// Stage 2 (increment 4): the trust-boundary / isolation assertions ported from
// e2e-extproc.sh (15 sink_fail_closed, 24 secret_api_denied). These are static
// cluster-state checks, not gateway traffic: the provider-selection sink Service must
// fail closed (no selector → no endpoints), and the ext-proc ServiceAccount must be
// denied the Kubernetes Secret API so a compromised data plane cannot read credentials.
const sinkService = "provider-selection-required-demo-model"

func TestExtProcIsolation(t *testing.T) {
	f := features.New("ExtProc isolation: sink fails closed, Secret API denied").
		Assess("provider-selection sink Service has no selector (fails closed)", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			var svc corev1.Service
			err := c.Client().Resources().Get(ctx, sinkService, tenantNamespace, &svc)
			switch {
			case apierrors.IsNotFound(err):
				// Absent is fail-closed: no Service, no endpoints.
			case err != nil:
				t.Fatalf("get sink service %s/%s: %v", tenantNamespace, sinkService, err)
			case len(svc.Spec.Selector) != 0:
				t.Errorf("sink service %s has selector %v, want none (fail-closed)", sinkService, svc.Spec.Selector)
			}
			return ctx
		}).
		Assess("ext-proc ServiceAccount is denied the Secret API", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			// The SA the ext-proc Deployment runs as (defaults to the Deployment name).
			sa, err := extProcServiceAccount(ctx, c)
			if err != nil {
				t.Fatalf("resolve ext-proc service account: %v", err)
			}
			allowed, err := canI(ctx, c, "system:serviceaccount:"+tenantNamespace+":"+sa, tenantNamespace, "get", "", "secrets")
			if err != nil {
				t.Fatalf("subject access review: %v", err)
			}
			if allowed {
				t.Errorf("ext-proc SA %q can get secrets; the data plane must be denied the Secret API", sa)
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// extProcServiceAccount returns the ServiceAccount the ext-proc Deployment runs as
// (its spec.template.spec.serviceAccountName, defaulting to the Deployment name).
func extProcServiceAccount(ctx context.Context, c *envconf.Config) (string, error) {
	var dep appsv1.Deployment
	if err := c.Client().Resources().Get(ctx, extDeployment, tenantNamespace, &dep); err != nil {
		return "", err
	}
	if sa := dep.Spec.Template.Spec.ServiceAccountName; sa != "" {
		return sa, nil
	}
	return extDeployment, nil
}

// canI runs a SubjectAccessReview for the given user and resource attributes, returning
// whether the action is allowed. The Go equivalent of `kubectl auth can-i <verb>
// <resource> --as=<user> -n <namespace>`.
func canI(ctx context.Context, c *envconf.Config, user, namespace, verb, group, resource string) (bool, error) {
	clientset, err := kubernetes.NewForConfig(c.Client().RESTConfig())
	if err != nil {
		return false, err
	}
	sar := &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User: user,
			ResourceAttributes: &authzv1.ResourceAttributes{
				Namespace: namespace,
				Verb:      verb,
				Group:     group,
				Resource:  resource,
			},
		},
	}
	out, err := clientset.AuthorizationV1().SubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return out.Status.Allowed, nil
}
