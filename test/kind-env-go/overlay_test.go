//go:build e2e

package kindenv

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	inferencev1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
	"github.com/opendatahub-io/ai-gateway-controller/pkg/envelope"
	"github.com/praxis-proxy/dockyard/feature"
	"github.com/praxis-proxy/dockyard/gateway"
)

// Stage 2 (increment 2): the overlay-digest convergence block ported from
// e2e-extproc.sh (assertions 17-21). The ext-proc data plane publishes the routing
// overlay as a ConfigMap, projects it into the pod, and accepts a revision only once
// the published / mounted / accepted / serving digests all agree. A provider
// weight-switch must drive that whole pipeline to converge on the new provider,
// leave the ext-proc pod identity untouched (reconcile, not restart), and be a
// semantic no-op when the same spec is re-applied.
const (
	extDeployment           = "payload-processing-external-model"
	overlayConfigMap        = "routing-overlay"
	overlayContentKey       = "routing-overlay.json"
	overlayMountPath        = "/etc/praxis/routing/routing-overlay.json"
	contentDigestAnnotation = "inference.opendatahub.io/routing-overlay-content-digest"
	externalModelName       = "demo-model"
)

// acceptedRevisionRE extracts the accepted/serving revision digests the ext-proc logs
// when it adopts an overlay snapshot (ANSI colour codes stripped first). The quotes
// are optional: the startup "snapshot initialized" line quotes the digest, while the
// hot-reload "overlay reloaded" line does not.
var (
	acceptedRevisionRE = regexp.MustCompile(`accepted_revision="?([0-9a-f]{64})"?`)
	servingRevisionRE  = regexp.MustCompile(`serving_revision="?([0-9a-f]{64})"?`)
	ansiRE             = regexp.MustCompile(`\x1b\[[0-9;]*m`)
)

func TestOverlayConvergence(t *testing.T) {
	f := features.New("Overlay convergence: provider weight switch, identity stability, semantic no-op").
		Assess("weight switch to B converges, attributes to katan-b, keeps ext-proc identity, resets to A, re-apply is a no-op", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			// A key + gateway port-forward to prove attribution after each switch.
			apiFwd, err := feature.PortForwardService(ctx, c, "maas-system", maasAPIService, maasAPIHTTPSPort)
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
			gwFwd, err := feature.PortForwardService(ctx, c, "maas-system", gatewayService, gatewayHTTPPort)
			if err != nil {
				t.Fatalf("port-forward gateway: %v", err)
			}
			defer gwFwd.Close()
			authed := gateway.New(gwFwd.URL("http"), gateway.WithBearer(key))
			demoPath := "/" + tenantNamespace + "/demo/v1/chat/completions"
			chat := gateway.ChatRequest{Model: "demo", Messages: gateway.User("hello")}

			// Baseline: the suite provisions provider-a active ([1,0]). Record the
			// ext-proc pod identity so we can prove the switch reconciles in place.
			before, err := extProcIdentity(ctx, c)
			if err != nil {
				t.Fatalf("ext-proc identity (before): %v", err)
			}

			// Always restore provider-a active, whatever happens below.
			defer func() {
				if err := patchExternalModelWeights(ctx, c, 1, 0); err != nil {
					t.Errorf("restore provider-a weights: %v", err)
				}
			}()

			// 17. Switch to provider-b and wait for the overlay to converge on B.
			if err := patchExternalModelWeights(ctx, c, 0, 1); err != nil {
				t.Fatalf("patch weights to B: %v", err)
			}
			if err := waitOverlay(ctx, c, "b", 3*time.Minute); err != nil {
				t.Fatalf("overlay did not converge on provider-b: %v", err)
			}

			// 18. The authorized request is now served by katan-b.
			b := postChat(ctx, t, authed, demoPath, chat)
			assertStatus(t, b, http.StatusOK, "provider_b_request")
			assertBackend(t, b, "katan-b-", "katan-a-")

			// 19. The ext-proc pod was reconciled in place: same UID, no restarts.
			after, err := extProcIdentity(ctx, c)
			if err != nil {
				t.Fatalf("ext-proc identity (after): %v", err)
			}
			if after.uid != before.uid || after.restarts != 0 || before.restarts != 0 {
				t.Errorf("ext-proc identity changed: before=%+v after=%+v (want same uid, 0 restarts)", before, after)
			}

			// 20. Reset to provider-a and wait for the overlay to converge back.
			if err := patchExternalModelWeights(ctx, c, 1, 0); err != nil {
				t.Fatalf("patch weights back to A: %v", err)
			}
			if err := waitOverlay(ctx, c, "a", 3*time.Minute); err != nil {
				t.Fatalf("overlay did not converge back on provider-a: %v", err)
			}

			// 21. Re-applying the identical ExternalModel spec is a semantic no-op:
			// the controller must not churn the routing-overlay ConfigMap.
			noopBefore, err := overlayIdentity(ctx, c)
			if err != nil {
				t.Fatalf("overlay identity (before no-op): %v", err)
			}
			if err := touchExternalModel(ctx, c); err != nil {
				t.Fatalf("re-apply ExternalModel: %v", err)
			}
			time.Sleep(5 * time.Second)
			noopAfter, err := overlayIdentity(ctx, c)
			if err != nil {
				t.Fatalf("overlay identity (after no-op): %v", err)
			}
			if noopBefore != noopAfter {
				t.Errorf("semantic no-op churned the overlay: before=%q after=%q", noopBefore, noopAfter)
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// overlayState is the set of digests that must agree for the overlay to be converged,
// plus which provider the published content selects.
type overlayState struct {
	declared, recomputed, mounted, accepted, serving string
	activeA, activeB                                 bool
}

func (s overlayState) converged(wanted string) bool {
	if s.declared == "" || s.declared != s.recomputed || s.declared != s.mounted ||
		s.declared != s.accepted || s.declared != s.serving {
		return false
	}
	if wanted == "a" {
		return s.activeA && !s.activeB
	}
	return s.activeB && !s.activeA
}

// readOverlayState gathers the published (ConfigMap annotation), recomputed (digest of
// the published content), mounted (digest of the projected file), and accepted/serving
// (ext-proc log) digests — the Go form of e2e-extproc.sh's overlay_state.
func readOverlayState(ctx context.Context, c *envconf.Config) (overlayState, error) {
	var st overlayState
	var cm corev1.ConfigMap
	if err := c.Client().Resources().Get(ctx, overlayConfigMap, tenantNamespace, &cm); err != nil {
		return st, fmt.Errorf("get configmap %s/%s: %w", tenantNamespace, overlayConfigMap, err)
	}
	content := cm.Data[overlayContentKey]
	st.declared = cm.Annotations[contentDigestAnnotation]
	st.activeA = strings.Contains(content, "provider-provider-a")
	st.activeB = strings.Contains(content, "provider-provider-b")
	if content != "" {
		if d, err := envelope.ComputeDigestFromWire([]byte(content)); err == nil {
			st.recomputed = d
		}
	}

	mounted, err := feature.ExecInDeployment(ctx, c, tenantNamespace, extDeployment, "cat", overlayMountPath)
	if err == nil && strings.TrimSpace(mounted) != "" {
		if d, derr := envelope.ComputeDigestFromWire([]byte(mounted)); derr == nil {
			st.mounted = d
		}
	}

	logs, err := feature.DeploymentLogs(ctx, c, tenantNamespace, extDeployment, "", 200)
	if err == nil {
		st.accepted, st.serving = lastRevisions(logs)
	}
	return st, nil
}

// lastRevisions returns the accepted/serving digests from the most recent ext-proc log
// line that reports them (ANSI colour codes stripped).
func lastRevisions(logs string) (accepted, serving string) {
	for _, line := range strings.Split(logs, "\n") {
		clean := ansiRE.ReplaceAllString(line, "")
		if m := acceptedRevisionRE.FindStringSubmatch(clean); m != nil {
			accepted = m[1]
			if s := servingRevisionRE.FindStringSubmatch(clean); s != nil {
				serving = s[1]
			}
		}
	}
	return accepted, serving
}

// waitOverlay polls until the overlay is converged on wanted and stays so for two
// consecutive samples (the stability gate from wait_overlay).
func waitOverlay(ctx context.Context, c *envconf.Config, wanted string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	stable := 0
	var last overlayState
	for {
		st, err := readOverlayState(ctx, c)
		if err != nil {
			return err
		}
		if st.converged(wanted) && st == last {
			stable++
			if stable >= 2 {
				return nil
			}
		} else {
			stable = 0
		}
		last = st
		if time.Now().After(deadline) {
			return fmt.Errorf("overlay not converged on %q: %+v", wanted, st)
		}
		time.Sleep(2 * time.Second)
	}
}

// overlayIdentity is a stable fingerprint of the routing-overlay ConfigMap (generation,
// resourceVersion, content digest, content) — the semantic-no-op comparison key.
func overlayIdentity(ctx context.Context, c *envconf.Config) (string, error) {
	var cm corev1.ConfigMap
	if err := c.Client().Resources().Get(ctx, overlayConfigMap, tenantNamespace, &cm); err != nil {
		return "", err
	}
	return fmt.Sprintf("gen=%d rv=%s digest=%s data=%s",
		cm.Generation, cm.ResourceVersion, cm.Annotations[contentDigestAnnotation], cm.Data[overlayContentKey]), nil
}

type podIdentity struct {
	uid      string
	restarts int32
}

// extProcIdentity returns the UID and total restart count of the first non-terminating
// ext-proc pod (the identity() helper), to prove a weight switch reconciles in place.
func extProcIdentity(ctx context.Context, c *envconf.Config) (podIdentity, error) {
	var pods corev1.PodList
	if err := c.Client().Resources().WithNamespace(tenantNamespace).List(ctx, &pods,
		resources.WithLabelSelector("app=payload-processing-external-model")); err != nil {
		return podIdentity{}, err
	}
	var live []corev1.Pod
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp == nil {
			live = append(live, pods.Items[i])
		}
	}
	if len(live) == 0 {
		return podIdentity{}, fmt.Errorf("no running ext-proc pod")
	}
	sort.Slice(live, func(i, j int) bool { return live[i].UID < live[j].UID })
	p := live[0]
	var restarts int32
	for _, cs := range p.Status.ContainerStatuses {
		restarts += cs.RestartCount
	}
	return podIdentity{uid: string(p.UID), restarts: restarts}, nil
}

// patchExternalModelWeights sets the provider-a / provider-b weights on the demo-model
// ExternalModel (retry-on-conflict), matching providers by ref name.
func patchExternalModelWeights(ctx context.Context, c *envconf.Config, weightA, weightB int) error {
	r := c.Client().Resources()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var em inferencev1alpha1.ExternalModel
		if err := r.Get(ctx, externalModelName, tenantNamespace, &em); err != nil {
			return err
		}
		for i := range em.Spec.ExternalProviderRefs {
			ref := &em.Spec.ExternalProviderRefs[i]
			var w int
			switch ref.Ref.Name {
			case "provider-a":
				w = weightA
			case "provider-b":
				w = weightB
			default:
				continue
			}
			ref.Weight = &w
		}
		return r.Update(ctx, &em)
	})
}

// touchExternalModel re-applies the ExternalModel unchanged (a no-op Update), so the
// controller reconciles with identical input — proving it does not churn the overlay.
func touchExternalModel(ctx context.Context, c *envconf.Config) error {
	r := c.Client().Resources()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var em inferencev1alpha1.ExternalModel
		if err := r.Get(ctx, externalModelName, tenantNamespace, &em); err != nil {
			return err
		}
		return r.Update(ctx, &em)
	})
}
