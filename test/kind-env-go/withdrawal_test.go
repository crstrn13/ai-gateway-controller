//go:build e2e

package kindenv

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	inferencev1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
	"github.com/praxis-proxy/dockyard/funcs"
	"github.com/praxis-proxy/dockyard/gateway"
)

// Stage 3 (increment 3): the all-zero withdrawal scenario from
// e2e-static-provider-weights.py. Setting every provider weight to zero must withdraw the
// model entirely — the ExternalModel goes Failed with Ready=False/NoRoutes, its HTTPRoute
// is deleted, and requests fail closed (no provider is contacted) — then recover when a
// positive weight is restored.
func TestAllZeroWithdrawal(t *testing.T) {
	f := features.New("All-zero withdrawal: zero total weight withdraws the model and fails closed").
		Assess("setting all weights to zero withdraws the route and fails requests closed, then recovers", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			apiFwd, err := funcs.PortForwardService(ctx, c, apiNamespace, maasAPIService, maasAPIHTTPSPort)
			if err != nil {
				t.Fatalf("port-forward maas-api: %v", err)
			}
			defer apiFwd.Close()
			caPEM, err := runOwnedCA(ctx, c)
			if err != nil {
				t.Fatalf("read run-owned CA: %v", err)
			}
			key, err := mintMaaSKey(ctx, apiFwd.Addr(), caPEM)
			if err != nil {
				t.Fatalf("mint MaaS key: %v", err)
			}
			gwFwd, err := funcs.PortForwardService(ctx, c, apiNamespace, gatewayService, gatewayHTTPPort)
			if err != nil {
				t.Fatalf("port-forward gateway: %v", err)
			}
			defer gwFwd.Close()
			authed := gateway.New(gwFwd.URL("http"), gateway.WithBearer(key), gateway.WithTimeout(30*time.Second))
			demoPath := "/" + tenantNamespace + "/demo/v1/chat/completions"
			chat := map[string]any{"model": "demo", "messages": []map[string]any{{"role": "user", "content": "withdraw"}}}

			if err := waitModelPhase(ctx, c, "Ready", 2*time.Minute); err != nil {
				t.Fatalf("precondition model Ready: %v", err)
			}
			routeName, err := modelRouteName(ctx, c)
			if err != nil || routeName == "" {
				t.Fatalf("read model HTTPRoute name: %v", err)
			}

			// Always restore provider-a and the route.
			defer func() {
				if err := patchExternalModelWeights(ctx, c, 1, 0); err != nil {
					t.Errorf("restore weights: %v", err)
					return
				}
				if err := waitModelPhase(ctx, c, "Ready", 2*time.Minute); err != nil {
					t.Errorf("recover model Ready: %v", err)
				}
				if err := waitOverlay(ctx, c, "a", 2*time.Minute); err != nil {
					t.Errorf("recover overlay on provider-a: %v", err)
				}
			}()

			// Withdraw: every weight zero.
			if err := patchExternalModelWeights(ctx, c, 0, 0); err != nil {
				t.Fatalf("set all weights to zero: %v", err)
			}

			// The model fails closed with NoRoutes and its HTTPRoute is removed.
			if err := waitModelWithdrawn(ctx, c, routeName, 3*time.Minute); err != nil {
				t.Fatalf("model did not withdraw: %v", err)
			}

			// A request now fails closed and reaches no provider.
			resp, err := authed.Post(ctx, demoPath, chat)
			if err != nil {
				t.Fatalf("withdrawal probe request: %v", err)
			}
			if resp.StatusCode == http.StatusOK {
				t.Errorf("request succeeded (%d) after withdrawal; must fail closed", resp.StatusCode)
			}
			if classifyKatan(resp.String()) != "" {
				t.Errorf("a provider was contacted after withdrawal (body attributed to a backend)")
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// modelRouteName returns the ExternalModel's current HTTPRoute name.
func modelRouteName(ctx context.Context, c *envconf.Config) (string, error) {
	var em inferencev1alpha1.ExternalModel
	if err := c.Client().Resources().Get(ctx, externalModelName, tenantNamespace, &em); err != nil {
		return "", err
	}
	return em.Status.HTTPRouteName, nil
}

// waitModelWithdrawn polls until the model is Failed with a Ready=False/NoRoutes condition
// at its observed generation and its HTTPRoute is gone.
func waitModelWithdrawn(ctx context.Context, c *envconf.Config, routeName string, timeout time.Duration) error {
	return funcs.Retry(ctx, timeout, 2*time.Second, "model withdrawn", func(ctx context.Context) error {
		var em inferencev1alpha1.ExternalModel
		if err := c.Client().Resources().Get(ctx, externalModelName, tenantNamespace, &em); err != nil {
			return err
		}
		noRoutes := false
		for _, cond := range em.Status.Conditions {
			if cond.Type == "Ready" && string(cond.Status) == "False" && cond.Reason == "NoRoutes" {
				noRoutes = true
			}
		}
		routeGone, err := httpRouteAbsent(ctx, c, routeName)
		if err != nil {
			return err
		}
		if em.Status.Phase == "Failed" && noRoutes && em.Generation == em.Status.ObservedGeneration && routeGone {
			return nil
		}
		return fmt.Errorf("phase=%q noRoutes=%v routeGone=%v gen=%d observed=%d",
			em.Status.Phase, noRoutes, routeGone, em.Generation, em.Status.ObservedGeneration)
	})
}

// httpRouteAbsent reports whether the named HTTPRoute no longer exists.
func httpRouteAbsent(ctx context.Context, c *envconf.Config, name string) (bool, error) {
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"})
	err := c.Client().Resources().Get(ctx, name, tenantNamespace, route)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}
