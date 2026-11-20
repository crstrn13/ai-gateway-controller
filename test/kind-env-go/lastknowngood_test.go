//go:build e2e

package kindenv

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/opendatahub-io/ai-gateway-controller/pkg/envelope"
	"github.com/praxis-proxy/dockyard/funcs"
	"github.com/praxis-proxy/dockyard/gateway"
)

// Stage 2 (increment 7): the last-known-good assertions ported from e2e-extproc.sh
// (22/23). Feeding the routing-overlay ConfigMap invalid content must not take the data
// plane down: the ext-proc keeps the last valid revision serving, so once the system
// re-converges the mounted / accepted / serving digests all equal the prior valid
// digest and requests still return 200. Restoring valid content recovers cleanly.
func TestLastKnownGood(t *testing.T) {
	f := features.New("Last-known-good: invalid overlay keeps the last valid revision serving").
		Assess("invalid overlay content converges back to the last valid digest and keeps serving 200, then recovers", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
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
			chat := gateway.ChatRequest{Model: "demo", Messages: gateway.User("hello")}

			// Precondition: converged on provider-a. Capture the valid content + digest.
			if err := waitOverlay(ctx, c, "a", 2*time.Minute); err != nil {
				t.Fatalf("precondition overlay on provider-a: %v", err)
			}
			validContent, err := overlayContent(ctx, c)
			if err != nil {
				t.Fatalf("read valid overlay content: %v", err)
			}
			validDigest, err := envelope.ComputeDigestFromWire([]byte(validContent))
			if err != nil {
				t.Fatalf("recompute valid digest: %v", err)
			}

			// Always restore the valid overlay content, whatever happens.
			defer func() {
				if err := setOverlayContent(ctx, c, validContent); err != nil {
					t.Errorf("restore valid overlay content: %v", err)
					return
				}
				if err := waitOverlay(ctx, c, "a", 2*time.Minute); err != nil {
					t.Errorf("recover overlay on provider-a: %v", err)
				}
			}()

			// 22. Corrupt the overlay ConfigMap; the ext-proc must keep the last valid
			// revision serving, re-converging mounted/accepted/serving on validDigest.
			if err := setOverlayContent(ctx, c, "{invalid-overlay"); err != nil {
				t.Fatalf("write invalid overlay: %v", err)
			}
			if err := waitLastKnownGood(ctx, c, "a", validDigest, 3*time.Minute); err != nil {
				t.Fatalf("did not hold last-known-good: %v", err)
			}
			lkg := postChat(ctx, t, authed, demoPath, chat)
			assertStatus(t, lkg, http.StatusOK, "last_known_good")

			// 23 is asserted by the deferred restore + overlay re-convergence on A.
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// overlayContent returns the current routing-overlay.json payload from the ConfigMap.
func overlayContent(ctx context.Context, c *envconf.Config) (string, error) {
	var cm corev1.ConfigMap
	if err := c.Client().Resources().Get(ctx, overlayConfigMap, tenantNamespace, &cm); err != nil {
		return "", err
	}
	return cm.Data[overlayContentKey], nil
}

// setOverlayContent overwrites the routing-overlay.json payload (retry-on-conflict).
func setOverlayContent(ctx context.Context, c *envconf.Config, content string) error {
	r := c.Client().Resources()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cm corev1.ConfigMap
		if err := r.Get(ctx, overlayConfigMap, tenantNamespace, &cm); err != nil {
			return err
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[overlayContentKey] = content
		return r.Update(ctx, &cm)
	})
}

// lastKnownGoodState reads what the ext-proc actually has — the mounted overlay file
// (digest + active provider) and the accepted/serving digests from its logs — ignoring
// the ConfigMap, which may hold invalid content. This is e2e-extproc.sh's
// last_known_good_state.
func lastKnownGoodState(ctx context.Context, c *envconf.Config) (st overlayState, err error) {
	mounted, err := funcs.ExecInDeployment(ctx, c, tenantNamespace, extDeployment, "cat", overlayMountPath)
	if err != nil {
		return st, err
	}
	if d, derr := envelope.ComputeDigestFromWire([]byte(mounted)); derr == nil {
		st.mounted = d
	}
	st.activeA = strings.Contains(mounted, "provider-provider-a")
	st.activeB = strings.Contains(mounted, "provider-provider-b")

	logs, err := funcs.DeploymentLogs(ctx, c, tenantNamespace, extDeployment, "", 200)
	if err != nil {
		return st, err
	}
	st.accepted, st.serving = lastRevisions(logs)
	return st, nil
}

// waitLastKnownGood polls until the mounted/accepted/serving digests all equal
// expectedDigest with the wanted provider active, stable for two samples.
func waitLastKnownGood(ctx context.Context, c *envconf.Config, wanted, expectedDigest string, timeout time.Duration) error {
	stable := 0
	var last overlayState
	return funcs.Retry(ctx, timeout, 2*time.Second, fmt.Sprintf("last-known-good %q@%s", wanted, expectedDigest), func(ctx context.Context) error {
		st, err := lastKnownGoodState(ctx, c)
		if err != nil {
			return err
		}
		if held(st, wanted, expectedDigest) && st == last {
			stable++
		} else {
			stable = 0
		}
		last = st
		if stable >= 2 {
			return nil
		}
		return fmt.Errorf("not yet 2 stable samples: %+v", st)
	})
}

func held(st overlayState, wanted, expectedDigest string) bool {
	if st.mounted != expectedDigest || st.accepted != expectedDigest || st.serving != expectedDigest {
		return false
	}
	if wanted == "a" {
		return st.activeA && !st.activeB
	}
	return st.activeB && !st.activeA
}
