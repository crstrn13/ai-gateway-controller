// Package boundary holds the pure, cluster-free helpers behind the Stage 7
// credential/namespace-boundary contracts. Ported from test/kind-env's
// test-namespace-boundary.sh (the jq predicate) and the manifest half of
// test-provider-credentials.sh; unit tested in boundary_test.go.
package boundary

// NamespaceBoundaryExpect is the expected identity a tenant HTTPRoute + Gateway pair must
// satisfy for the tenant to be correctly isolated.
type NamespaceBoundaryExpect struct {
	Tenant      string
	GatewayNS   string
	GatewayName string
	// SecretAPI is the ext-proc's Kubernetes Secret-API access verdict ("no" when the
	// data plane is correctly denied the Secret API).
	SecretAPI string
}

// gatewayTenantLabel is the Gateway listener selector label that pins a tenant.
const gatewayTenantLabel = "local-env.opendatahub.io/gateway-tenant"

// NamespaceBoundaryOK reports whether a tenant's HTTPRoute (route) and the shared Gateway
// (gateway), together with the ReferenceGrant list (grants), satisfy the namespace
// isolation contract: the route attaches to the expected Gateway, its backend stays in
// the tenant namespace, it is Accepted + ResolvedRefs, the Gateway admits routes only by
// the tenant selector, no ReferenceGrant widens the boundary, and the ext-proc is denied
// the Secret API. Mirrors test-namespace-boundary.sh's check_boundary.
func NamespaceBoundaryOK(route, gateway, grants map[string]any, expect NamespaceBoundaryExpect) bool {
	routeNS := str(dig(route, "metadata", "namespace"))

	parent := firstStr(dig(route, "spec", "parentRefs"), "namespace", routeNS)
	parentName := firstStr(dig(route, "spec", "parentRefs"), "name", "")
	backend := firstStr(dig(route, "spec", "rules", 0, "backendRefs"), "namespace", routeNS)

	accepted := countConditions(route, "Accepted")
	resolved := countConditions(route, "ResolvedRefs")

	allowed := str(dig(gateway, "spec", "listeners", 0, "allowedRoutes", "namespaces", "from"))
	selector := str(dig(gateway, "spec", "listeners", 0, "allowedRoutes", "namespaces", "selector", "matchLabels", gatewayTenantLabel))

	grantItems, _ := dig(grants, "items").([]any)

	return parent == expect.GatewayNS &&
		parentName == expect.GatewayName &&
		backend == expect.Tenant &&
		accepted >= 1 &&
		resolved >= 1 &&
		allowed == "Selector" &&
		selector == expect.Tenant &&
		len(grantItems) == 0 &&
		expect.SecretAPI == "no"
}

// countConditions counts route.status.parents[].conditions[] with the given type set True.
func countConditions(route map[string]any, condType string) int {
	parents, _ := dig(route, "status", "parents").([]any)
	n := 0
	for _, p := range parents {
		pm, _ := p.(map[string]any)
		conds, _ := pm["conditions"].([]any)
		for _, cond := range conds {
			cm, _ := cond.(map[string]any)
			if str(cm["type"]) == condType && str(cm["status"]) == "True" {
				n++
			}
		}
	}
	return n
}

// firstStr returns field from the first element of a list (which must be []any of maps),
// falling back to fallback when the list is empty or the field is absent/empty.
func firstStr(list any, field, fallback string) string {
	items, ok := list.([]any)
	if !ok || len(items) == 0 {
		return fallback
	}
	first, _ := items[0].(map[string]any)
	if v := str(first[field]); v != "" {
		return v
	}
	return fallback
}

// dig navigates nested maps (string key) and slices (int index), returning nil if any
// step is missing or the wrong type.
func dig(v any, path ...any) any {
	cur := v
	for _, step := range path {
		switch k := step.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = m[k]
		case int:
			s, ok := cur.([]any)
			if !ok || k < 0 || k >= len(s) {
				return nil
			}
			cur = s[k]
		default:
			return nil
		}
	}
	return cur
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
