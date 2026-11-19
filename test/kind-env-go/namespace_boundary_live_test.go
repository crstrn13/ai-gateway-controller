//go:build e2e

package kindenv

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/opendatahub-io/ai-gateway-controller/test/kind-env-go/boundary"
)

// Stage 7 (live): the namespace-boundary contract from test-namespace-boundary.sh, run
// against the real cluster state. The tenant's HTTPRoute, the shared Gateway, and the
// ReferenceGrant list must satisfy the isolation predicate (already unit-tested with
// synthetic fixtures) — the route attaches to the tenant Gateway, keeps its backend
// in-tenant, is Accepted/ResolvedRefs, the Gateway admits routes only by the tenant
// selector, no ReferenceGrant widens the boundary, and the ext-proc is denied Secrets.
func TestNamespaceBoundaryLive(t *testing.T) {
	f := features.New("Namespace boundary (live): the tenant route/gateway satisfy isolation").
		Assess("the live HTTPRoute, Gateway and ReferenceGrants pass the namespace-boundary predicate", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			route := &unstructured.Unstructured{}
			route.SetGroupVersionKind(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"})
			if err := c.Client().Resources().Get(ctx, "external-model-demo-model", tenantNamespace, route); err != nil {
				t.Fatalf("get HTTPRoute: %v", err)
			}

			gw := &unstructured.Unstructured{}
			gw.SetGroupVersionKind(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway"})
			if err := c.Client().Resources().Get(ctx, "maas-default-gateway", apiNamespace, gw); err != nil {
				t.Fatalf("get Gateway: %v", err)
			}

			grants := listReferenceGrants(ctx, t, c, tenantNamespace)

			// The real ext-proc Secret-API verdict feeds the predicate.
			sa, err := extProcServiceAccount(ctx, c)
			if err != nil {
				t.Fatalf("resolve ext-proc SA: %v", err)
			}
			allowed, err := canI(ctx, c, "system:serviceaccount:"+tenantNamespace+":"+sa, tenantNamespace, "get", "", "secrets")
			if err != nil {
				t.Fatalf("subject access review: %v", err)
			}
			secretAPI := "no"
			if allowed {
				secretAPI = "yes"
			}

			expect := boundary.NamespaceBoundaryExpect{
				Tenant:      tenantNamespace,
				GatewayNS:   apiNamespace,
				GatewayName: "maas-default-gateway",
				SecretAPI:   secretAPI,
			}
			if !boundary.NamespaceBoundaryOK(route.Object, gw.Object, grants, expect) {
				t.Errorf("live namespace boundary not satisfied (secretAPI=%s, grants=%d)", secretAPI, len(grants["items"].([]any)))
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// listReferenceGrants returns the ReferenceGrants in a namespace as a {"items":[...]} map,
// the shape NamespaceBoundaryOK expects.
func listReferenceGrants(ctx context.Context, t *testing.T, c *envconf.Config, namespace string) map[string]any {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1beta1", Kind: "ReferenceGrantList"})
	items := []any{}
	if err := c.Client().Resources().WithNamespace(namespace).List(ctx, list); err == nil {
		for i := range list.Items {
			items = append(items, list.Items[i].Object)
		}
	}
	return map[string]any{"items": items}
}
