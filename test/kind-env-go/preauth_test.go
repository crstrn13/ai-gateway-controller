//go:build e2e

package kindenv

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/praxis-proxy/dockyard/funcs"
	"github.com/praxis-proxy/dockyard/gateway"
)

// Stage 2 (increment 6): the pre-auth trust-boundary assertions ported from
// e2e-extproc.sh (34/35). payload-pre-processing is the component that authorizes and
// resolves a request before the gateway contacts a provider. With it scaled to zero the
// gateway must fail closed — reject the request (non-200) AND never reach the provider
// (the katan backend sees no new request) — then recover once it is scaled back up.
const (
	apiNamespace    = "maas-system"
	preAuthDeploy   = "payload-pre-processing"
	katanADeploy    = "katan-a"
	providerReqLine = "POST /v1/chat/completions"
)

func TestPreAuthTrustBoundary(t *testing.T) {
	f := features.New("Pre-auth trust boundary: gateway fails closed when payload-pre-processing is down").
		Assess("with pre-auth down the request is rejected and the provider is never contacted, then recovers", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
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
			authed := gateway.New(gwFwd.URL("http"), gateway.WithBearer(key))
			demoPath := "/" + tenantNamespace + "/demo/v1/chat/completions"

			// Always restore pre-auth, whatever happens below (assertion 35).
			defer func() {
				if err := scaleDeployment(ctx, c, apiNamespace, preAuthDeploy, 1); err != nil {
					t.Errorf("restore pre-auth replicas: %v", err)
					return
				}
				if err := waitDeploymentReplicas(ctx, c, apiNamespace, preAuthDeploy, 1, 2*time.Minute); err != nil {
					t.Errorf("pre-auth did not become ready again: %v", err)
				}
			}()

			before, err := providerRequestCount(ctx, c)
			if err != nil {
				t.Fatalf("provider request count (before): %v", err)
			}

			// 34. Scale pre-auth to zero and wait for it to stop.
			if err := scaleDeployment(ctx, c, apiNamespace, preAuthDeploy, 0); err != nil {
				t.Fatalf("scale pre-auth to 0: %v", err)
			}
			if err := waitDeploymentReplicas(ctx, c, apiNamespace, preAuthDeploy, 0, 2*time.Minute); err != nil {
				t.Fatalf("pre-auth did not stop: %v", err)
			}

			// A request that needs pre-auth to resolve must now fail closed: path model
			// "demo" but a conflicting body model, mirroring the shell probe.
			payload := map[string]any{
				"model":    "unauthorized-model",
				"messages": []map[string]any{{"role": "user", "content": "hello"}},
			}
			resp, err := authed.Post(ctx, demoPath, payload, gateway.ModelHeader("demo"))
			if err != nil {
				t.Fatalf("pre-auth-unavailable request: %v", err)
			}
			if resp.StatusCode == http.StatusOK {
				t.Errorf("request succeeded (%d) with pre-auth down; must fail closed", resp.StatusCode)
			}

			after, err := providerRequestCount(ctx, c)
			if err != nil {
				t.Fatalf("provider request count (after): %v", err)
			}
			if after != before {
				t.Errorf("provider was contacted with pre-auth down: katan requests before=%d after=%d", before, after)
			}
			// 35 is asserted by the deferred scale-back to 1 ready replica.
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// scaleDeployment sets a Deployment's replica count (retry-on-conflict).
func scaleDeployment(ctx context.Context, c *envconf.Config, namespace, name string, replicas int32) error {
	r := c.Client().Resources()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var dep appsv1.Deployment
		if err := r.Get(ctx, name, namespace, &dep); err != nil {
			return err
		}
		dep.Spec.Replicas = &replicas
		return r.Update(ctx, &dep)
	})
}

// waitDeploymentReplicas polls until a Deployment's readyReplicas equals wanted.
func waitDeploymentReplicas(ctx context.Context, c *envconf.Config, namespace, name string, wanted int32, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var dep appsv1.Deployment
		if err := c.Client().Resources().Get(ctx, name, namespace, &dep); err != nil {
			return err
		}
		if dep.Status.ReadyReplicas == wanted {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("deployment %s/%s readyReplicas=%d, want %d", namespace, name, dep.Status.ReadyReplicas, wanted)
		}
		time.Sleep(time.Second)
	}
}

// providerRequestCount counts inference requests the katan-a backend has logged, used to
// prove the provider was (not) contacted across a trust-boundary probe.
func providerRequestCount(ctx context.Context, c *envconf.Config) (int, error) {
	logs, err := funcs.DeploymentLogs(ctx, c, apiNamespace, katanADeploy, "", 300)
	if err != nil {
		return 0, err
	}
	return strings.Count(logs, providerReqLine), nil
}
