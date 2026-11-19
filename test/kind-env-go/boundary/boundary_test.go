package boundary

import (
	"encoding/json"
	"testing"
)

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

const (
	validRouteJSON   = `{"metadata":{"namespace":"tenant-a"},"spec":{"parentRefs":[{"namespace":"maas-system","name":"gateway"}],"rules":[{"backendRefs":[{"name":"praxis"}]}]},"status":{"parents":[{"conditions":[{"type":"Accepted","status":"True"},{"type":"ResolvedRefs","status":"True"}]}]}}`
	validGatewayJSON = `{"spec":{"listeners":[{"allowedRoutes":{"namespaces":{"from":"Selector","selector":{"matchLabels":{"local-env.opendatahub.io/gateway-tenant":"tenant-a"}}}}}]}}`
	noGrantsJSON     = `{"items":[]}`
)

var expectTenantA = NamespaceBoundaryExpect{Tenant: "tenant-a", GatewayNS: "maas-system", GatewayName: "gateway", SecretAPI: "no"}

func TestNamespaceBoundaryAcceptsValidTenantRoute(t *testing.T) {
	route := mustJSON(t, validRouteJSON)
	gateway := mustJSON(t, validGatewayJSON)
	grants := mustJSON(t, noGrantsJSON)
	if !NamespaceBoundaryOK(route, gateway, grants, expectTenantA) {
		t.Error("valid tenant route should pass the namespace boundary")
	}
}

func TestNamespaceBoundaryRejectsWrongParentNamespace(t *testing.T) {
	route := mustJSON(t, validRouteJSON)
	// Point the parentRef at a different Gateway namespace.
	route["spec"].(map[string]any)["parentRefs"].([]any)[0].(map[string]any)["namespace"] = "wrong-gateway-ns"
	if NamespaceBoundaryOK(route, mustJSON(t, validGatewayJSON), mustJSON(t, noGrantsJSON), expectTenantA) {
		t.Error("wrong parent namespace must be rejected")
	}
}

func TestNamespaceBoundaryRejectsCrossNamespaceBackend(t *testing.T) {
	route := mustJSON(t, validRouteJSON)
	// Point the backend at another tenant's namespace.
	route["spec"].(map[string]any)["rules"].([]any)[0].(map[string]any)["backendRefs"].([]any)[0].(map[string]any)["namespace"] = "other-tenant"
	if NamespaceBoundaryOK(route, mustJSON(t, validGatewayJSON), mustJSON(t, noGrantsJSON), expectTenantA) {
		t.Error("cross-namespace backend must be rejected")
	}
}

func TestNamespaceBoundaryRejectsNonSelectorGateway(t *testing.T) {
	gateway := mustJSON(t, validGatewayJSON)
	// A Gateway that admits All namespaces breaks tenant isolation.
	ns := gateway["spec"].(map[string]any)["listeners"].([]any)[0].(map[string]any)["allowedRoutes"].(map[string]any)["namespaces"].(map[string]any)
	ns["from"] = "All"
	if NamespaceBoundaryOK(mustJSON(t, validRouteJSON), gateway, mustJSON(t, noGrantsJSON), expectTenantA) {
		t.Error("a non-Selector Gateway must be rejected")
	}
}

func TestNamespaceBoundaryRejectsReferenceGrant(t *testing.T) {
	grants := mustJSON(t, `{"items":[{"metadata":{"name":"widens-boundary"}}]}`)
	if NamespaceBoundaryOK(mustJSON(t, validRouteJSON), mustJSON(t, validGatewayJSON), grants, expectTenantA) {
		t.Error("any ReferenceGrant must fail the boundary (none should exist)")
	}
}
