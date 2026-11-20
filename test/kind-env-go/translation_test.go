//go:build e2e

package kindenv

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	inferencev1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
	"github.com/opendatahub-io/ai-gateway-controller/test/kind-env-go/contract"
	"github.com/praxis-proxy/dockyard/feature"
	"github.com/praxis-proxy/dockyard/gateway"
	"github.com/praxis-proxy/dockyard/install"
)

// Stage 5: the API-translation qualification from e2e-api-translation.py. Translation is a
// tenant permission (MaasTenantConfig.spec.apiTranslation.enabled) that, once granted,
// lets an ExternalModel bind providers of different API formats (openai-chat, Anthropic
// messages) behind one client model: the controller publishes each provider's translation
// profile (api_format + target_model) into the routing overlay, the ext-proc config turns
// on api_translation_enabled with the body-size limits, and a client's OpenAI request to
// the Anthropic provider is translated to /v1/messages on the wire. This provisions the
// provider-aware translation recorder + three translation providers, proves those three
// contracts, and tears the fixture down.
//
// Prerequisite: the recorder image (provider-recorder:issue28-kind) must be loadable into
// the kind cluster; the test best-effort `kind load`s it and fails clearly if the backend
// cannot start.
const (
	translationBackendDeploy     = "api-translation-provider-aware-mock"
	recorderImage                = "provider-recorder:issue28-kind"
	nativeClusterName            = "dockyard-native"
	openaiTranslationProvider    = "api-translation-openai"
	anthropicTranslationProvider = "api-translation-anthropic"
)

type translationProvider struct {
	name, provider, endpoint, secret string
}

// Only the two providers the assertions exercise are provisioned; the bedrock-mantle
// provider the Python suite also creates is for sub-tests not ported here.
var translationProviders = []translationProvider{
	{openaiTranslationProvider, "openai", "provider-translation-openai-ext.maas-system.svc.cluster.local", "api-translation-openai-credentials"},
	{anthropicTranslationProvider, "anthropic", "provider-translation-ext.maas-system.svc.cluster.local", "api-translation-anthropic-credentials"},
}

func TestAPITranslation(t *testing.T) {
	f := features.New("API translation: tenant permission publishes profiles and translates requests").
		Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			if err := provisionTranslationStack(ctx, t, c); err != nil {
				t.Fatalf("provision translation stack: %v", err)
			}
			return ctx
		}).
		Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			teardownTranslationStack(ctx, t, c)
			return ctx
		}).
		Assess("overlay publishes each provider's translation profile (api_format + target_model)", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			profiles, err := overlayProfiles(ctx, c)
			if err != nil {
				t.Fatalf("read overlay profiles: %v", err)
			}
			want := map[string][2]string{
				"provider-api-translation-openai":    {"openai-chat", "mock-openai-model"},
				"provider-api-translation-anthropic": {"messages", "mock-anthropic-model"},
			}
			for cluster, exp := range want {
				got, ok := profiles[cluster]
				if !ok {
					t.Errorf("overlay missing candidate %q", cluster)
					continue
				}
				if got[0] != exp[0] || got[1] != exp[1] {
					t.Errorf("%s profile = api_format=%s target_model=%s, want %s/%s", cluster, got[0], got[1], exp[0], exp[1])
				}
			}
			return ctx
		}).
		Assess("mounted ExtProc config enables translation with sane body-size limits", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			cfg, err := feature.ExecInDeployment(ctx, c, tenantNamespace, extDeployment, "cat", "/etc/praxis/extproc.yaml")
			if err != nil {
				t.Fatalf("read mounted ExtProc config: %v", err)
			}
			limits := contract.ExtProcTranslationLimits(cfg)
			if !limits.TranslatorFilterPresent {
				t.Error("translator filter absent from mounted config")
			}
			if limits.ServerMaxBodyBytes == nil || *limits.ServerMaxBodyBytes <= 0 {
				t.Errorf("server max_body_bytes = %v, want > 0", limits.ServerMaxBodyBytes)
			}
			if limits.TranslatorMaxRequestBodyBytes == nil || *limits.TranslatorMaxRequestBodyBytes <= 0 {
				t.Errorf("translator max_request_body_bytes = %v, want > 0", limits.TranslatorMaxRequestBodyBytes)
			}
			if limits.TranslatorMaxResponseBodyBytes == nil || *limits.TranslatorMaxResponseBodyBytes <= 0 {
				t.Errorf("translator max_response_body_bytes = %v, want > 0", limits.TranslatorMaxResponseBodyBytes)
			}
			return ctx
		}).
		Assess("an OpenAI client request routed to the Anthropic provider is translated and served", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			// Route everything to the Anthropic (messages) provider so the OpenAI client
			// request must be translated to /v1/messages to succeed.
			if err := setTranslationWeights(ctx, c, 0, 1); err != nil {
				t.Fatalf("route to anthropic: %v", err)
			}
			if err := waitOverlayCandidates(ctx, c, []string{"provider-api-translation-anthropic"}, 3*time.Minute); err != nil {
				t.Fatalf("anthropic-only did not converge: %v", err)
			}

			apiFwd, err := feature.PortForwardService(ctx, c, apiNamespace, maasAPIService, maasAPIHTTPSPort)
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
			gwFwd, err := feature.PortForwardService(ctx, c, apiNamespace, gatewayService, gatewayHTTPPort)
			if err != nil {
				t.Fatalf("port-forward gateway: %v", err)
			}
			defer gwFwd.Close()
			authed := gateway.New(gwFwd.URL("http"), gateway.WithBearer(key), gateway.WithTimeout(30*time.Second))

			// The client always speaks OpenAI chat; the ext-proc translates it to Anthropic
			// Messages for this provider.
			resp, err := retryRequestStatus(ctx, authed, "/"+tenantNamespace+"/demo/v1/chat/completions",
				map[string]any{"model": "demo", "messages": []map[string]any{{"role": "user", "content": "Reply with the word blue."}}},
				http.StatusOK, 2*time.Minute)
			if err != nil {
				t.Fatalf("translated request did not reach 200: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Errorf("translated request status = %d, want 200", resp.StatusCode)
			}
			// The Anthropic provider's response is translated back to an OpenAI
			// chat-completion shape for the client.
			body := resp.String()
			t.Logf("translated response body (first 400): %s", truncate(body, 400))
			if !strings.Contains(body, "\"choices\"") {
				t.Errorf("response not translated to OpenAI shape (no \"choices\"): %s", truncate(body, 300))
			}
			return ctx
		}).
		Assess("a request over the translator input limit is rejected with 413", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			// Still routed to the Anthropic translation provider from the prior assessment.
			apiFwd, err := feature.PortForwardService(ctx, c, apiNamespace, maasAPIService, maasAPIHTTPSPort)
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
			gwFwd, err := feature.PortForwardService(ctx, c, apiNamespace, gatewayService, gatewayHTTPPort)
			if err != nil {
				t.Fatalf("port-forward gateway: %v", err)
			}
			defer gwFwd.Close()
			authed := gateway.New(gwFwd.URL("http"), gateway.WithBearer(key), gateway.WithTimeout(60*time.Second))

			// Build an ~11 MiB request body, over the 10 MiB translator input limit.
			big, err := contract.ChatRequestWithExactJSONSize(11*1024*1024, contract.ExactSizeOptions{})
			if err != nil {
				t.Fatalf("build oversized body: %v", err)
			}
			resp, err := authed.Post(ctx, "/"+tenantNamespace+"/demo/v1/chat/completions", big)
			if err != nil {
				t.Fatalf("oversized request: %v", err)
			}
			if resp.StatusCode != http.StatusRequestEntityTooLarge {
				t.Errorf("oversized request status = %d, want 413", resp.StatusCode)
			}
			return ctx
		}).
		Assess("a streaming OpenAI request to the Anthropic provider yields translated OpenAI SSE chunks", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			// Still routed to the Anthropic translation provider.
			apiFwd, err := feature.PortForwardService(ctx, c, apiNamespace, maasAPIService, maasAPIHTTPSPort)
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
			gwFwd, err := feature.PortForwardService(ctx, c, apiNamespace, gatewayService, gatewayHTTPPort)
			if err != nil {
				t.Fatalf("port-forward gateway: %v", err)
			}
			defer gwFwd.Close()

			// Base the client at the tenant/model prefix so ChatCompletionStream's
			// "/v1/chat/completions" resolves to the gateway route.
			streamClient := gateway.New(gwFwd.URL("http")+"/"+tenantNamespace+"/demo",
				gateway.WithBearer(key), gateway.WithTimeout(60*time.Second))
			stream, err := streamClient.ChatCompletionStream(ctx,
				gateway.ChatRequest{Model: "demo", Messages: gateway.User("Reply with the word blue.")})
			if err != nil {
				t.Fatalf("open translated stream (expected 200): %v", err)
			}
			defer func() { _ = stream.Close() }()

			chunks := 0
			openAIShaped := false
			for {
				data, err := stream.Recv()
				if err != nil {
					break // io.EOF at [DONE] / end
				}
				chunks++
				if strings.Contains(string(data), "chat.completion.chunk") || strings.Contains(string(data), "\"delta\"") {
					openAIShaped = true
				}
			}
			if chunks == 0 {
				t.Error("translated stream yielded no SSE chunks")
			}
			if !openAIShaped {
				t.Error("streamed chunks were not translated to OpenAI SSE shape (no chat.completion.chunk/delta)")
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// --- provisioning ---

func provisionTranslationStack(ctx context.Context, t *testing.T, c *envconf.Config) error {
	// Best-effort load the recorder image into kind (no-op if already present).
	if _, err := install.LoadImage(nativeClusterName, recorderImage, true)(ctx, c); err != nil {
		t.Logf("kind load %s (continuing; image may already be present): %v", recorderImage, err)
	}

	// Backend + provider-auth credential secrets (same token both sides).
	for _, p := range translationProviders {
		token := "translation-test-key-" + p.provider
		if err := ensureSecret(ctx, c, tenantNamespace, p.secret, token); err != nil {
			return err
		}
		if err := ensureSecret(ctx, c, apiNamespace, backendSecretName(p.secret), token); err != nil {
			return err
		}
	}
	// The provider-aware backend manifest always mounts all three backend-credential
	// secrets (openai/anthropic/bedrock), so the bedrock one must exist even though we
	// only wire the openai + anthropic providers.
	if err := ensureSecret(ctx, c, apiNamespace, "api-translation-bedrock-backend-credential", "translation-test-key-bedrock"); err != nil {
		return err
	}

	// Provider-aware recorder backend (Deployment + 3 Services).
	if _, err := install.Manifest("../kind-env/manifests/api-translation-provider-aware-backend.yaml")(ctx, c); err != nil {
		return fmt.Errorf("apply backend manifest: %w", err)
	}
	if err := waitDeploymentRolledOut(ctx, c, apiNamespace, translationBackendDeploy, 3*time.Minute); err != nil {
		return fmt.Errorf("translation backend not ready (is %s loaded into kind?): %w", recorderImage, err)
	}

	// Three translation ExternalProviders.
	for _, p := range translationProviders {
		if err := ensureExternalProvider(ctx, c, p); err != nil {
			return err
		}
	}
	for _, p := range translationProviders {
		if err := waitExternalProviderReady(ctx, c, p.name, 3*time.Minute); err != nil {
			return err
		}
	}

	// Grant the tenant translation permission.
	if err := setTenantTranslation(ctx, c, true); err != nil {
		return err
	}

	// Rewire demo-model to the translation refs (openai + anthropic).
	if err := setTranslationRefs(ctx, c); err != nil {
		return err
	}
	return waitOverlayCandidates(ctx, c, []string{"provider-api-translation-openai", "provider-api-translation-anthropic"}, 3*time.Minute)
}

func teardownTranslationStack(ctx context.Context, t *testing.T, c *envconf.Config) {
	// Restore demo-model to the provider-a baseline the rest of the suite expects.
	if err := restoreDemoModelBaseline(ctx, c); err != nil {
		t.Errorf("restore demo-model: %v", err)
	}
	if err := waitOverlay(ctx, c, "a", 2*time.Minute); err != nil {
		t.Errorf("demo-model did not reconverge on provider-a: %v", err)
	}
	_ = setTenantTranslation(ctx, c, false)
	for _, p := range translationProviders {
		deleteExternalProvider(ctx, c, p.name)
	}
	// NOTE: the provider-auth credential secrets are intentionally NOT deleted. The
	// controller adds their projection to the ext-proc Deployment when translation refs
	// are bound but never prunes it once the model returns to the baseline — so deleting
	// the secrets would wedge the next ext-proc rollout on a missing mount. They are tiny
	// dummy secrets; leaving them keeps the data plane healthy and re-provisioning is
	// idempotent (ensureSecret tolerates AlreadyExists).
	// Delete the backend Deployment + Services.
	_ = c.Client().Resources().Delete(ctx, deploymentObj(apiNamespace, translationBackendDeploy))
	for _, svc := range []string{"provider-translation-ext", "provider-translation-openai-ext", "provider-translation-bedrock-ext"} {
		_ = c.Client().Resources().Delete(ctx, serviceObj(apiNamespace, svc))
	}
	// Ensure the ext-proc is on a single healthy pod before the next test runs.
	_ = waitDeploymentRolledOut(ctx, c, tenantNamespace, extDeployment, 2*time.Minute)
}

func backendSecretName(providerSecret string) string {
	// api-translation-openai-credentials → api-translation-openai-backend-credential
	return providerSecret[:len(providerSecret)-len("-credentials")] + "-backend-credential"
}

func ensureSecret(ctx context.Context, c *envconf.Config, ns, name, token string) error {
	err := c.Client().Resources().Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"api-key": token},
	})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func ensureExternalProvider(ctx context.Context, c *envconf.Config, p translationProvider) error {
	ep := &inferencev1alpha1.ExternalProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name: p.name, Namespace: tenantNamespace,
			Labels: map[string]string{"app.kubernetes.io/managed-by": "api-translation-kind-e2e"},
		},
		Spec: inferencev1alpha1.ExternalProviderSpec{
			Provider: p.provider,
			Endpoint: p.endpoint,
			Auth:     inferencev1alpha1.AuthConfig{Type: "apikey", SecretRef: inferencev1alpha1.NameReference{Name: p.secret}},
			Config:   map[string]string{"tls.caCertificates": providerCAPath},
		},
	}
	err := c.Client().Resources().Create(ctx, ep)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func deleteExternalProvider(ctx context.Context, c *envconf.Config, name string) {
	r := c.Client().Resources()
	ep := &inferencev1alpha1.ExternalProvider{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: tenantNamespace}}
	_ = r.Delete(ctx, ep)
	// Clear the cleanup finalizer if it blocks deletion (the backend is already gone).
	_ = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var live inferencev1alpha1.ExternalProvider
		if err := r.Get(ctx, name, tenantNamespace, &live); err != nil {
			return nil //nolint:nilerr // gone is success
		}
		if len(live.Finalizers) == 0 {
			return nil
		}
		live.Finalizers = nil
		return r.Update(ctx, &live)
	})
}

func waitExternalProviderReady(ctx context.Context, c *envconf.Config, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var ep inferencev1alpha1.ExternalProvider
		if err := c.Client().Resources().Get(ctx, name, tenantNamespace, &ep); err == nil && ep.Status.Phase == "Ready" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ExternalProvider %s not Ready", name)
		}
		time.Sleep(2 * time.Second)
	}
}

func setTenantTranslation(ctx context.Context, c *envconf.Config, enabled bool) error {
	r := c.Client().Resources()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		mtc := &unstructured.Unstructured{}
		mtc.SetGroupVersionKind(maasTenantConfigGVKTest)
		if err := r.Get(ctx, tenantConfigName, tenantNamespace, mtc); err != nil {
			return err
		}
		if err := unstructured.SetNestedField(mtc.Object, enabled, "spec", "apiTranslation", "enabled"); err != nil {
			return err
		}
		return r.Update(ctx, mtc)
	})
}

func setTranslationRefs(ctx context.Context, c *envconf.Config) error {
	return updateDemoModelRefs(ctx, c, []inferencev1alpha1.ExternalProviderRef{
		translationRef(openaiTranslationProvider, "mock-openai-model", "openai-chat", "/v1/chat/completions", 1),
		translationRef(anthropicTranslationProvider, "mock-anthropic-model", "messages", "/v1/messages", 1),
	})
}

// setTranslationWeights sets the openai/anthropic translation ref weights.
func setTranslationWeights(ctx context.Context, c *envconf.Config, openaiW, anthropicW int) error {
	return updateDemoModelRefs(ctx, c, []inferencev1alpha1.ExternalProviderRef{
		translationRef(openaiTranslationProvider, "mock-openai-model", "openai-chat", "/v1/chat/completions", openaiW),
		translationRef(anthropicTranslationProvider, "mock-anthropic-model", "messages", "/v1/messages", anthropicW),
	})
}

func restoreDemoModelBaseline(ctx context.Context, c *envconf.Config) error {
	return updateDemoModelRefs(ctx, c, []inferencev1alpha1.ExternalProviderRef{
		translationRef("provider-a", "demo", "openai-chat", "/v1/chat/completions", 1),
		translationRef("provider-b", "demo", "openai-chat", "/v1/chat/completions", 0),
	})
}

func translationRef(provider, targetModel, apiFormat, path string, weight int) inferencev1alpha1.ExternalProviderRef {
	w := weight
	return inferencev1alpha1.ExternalProviderRef{
		Ref:         inferencev1alpha1.ExternalProviderReference{Name: provider},
		TargetModel: targetModel,
		APIFormat:   apiFormat,
		Path:        path,
		Weight:      &w,
	}
}

func updateDemoModelRefs(ctx context.Context, c *envconf.Config, refs []inferencev1alpha1.ExternalProviderRef) error {
	r := c.Client().Resources()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var em inferencev1alpha1.ExternalModel
		if err := r.Get(ctx, externalModelName, tenantNamespace, &em); err != nil {
			return err
		}
		em.Spec.ExternalProviderRefs = refs
		return r.Update(ctx, &em)
	})
}

// --- overlay inspection ---

// overlayProfiles returns cluster → {api_format, target_model} from the mounted overlay.
func overlayProfiles(ctx context.Context, c *envconf.Config) (map[string][2]string, error) {
	raw, err := feature.ExecInDeployment(ctx, c, tenantNamespace, extDeployment, "cat", overlayMountPath)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Overlay struct {
			Candidates []struct {
				Cluster     string `json:"cluster"`
				APIFormat   string `json:"api_format"`
				TargetModel string `json:"target_model"`
			} `json:"candidates"`
		} `json:"overlay"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, err
	}
	out := map[string][2]string{}
	for _, cand := range doc.Overlay.Candidates {
		out[cand.Cluster] = [2]string{cand.APIFormat, cand.TargetModel}
	}
	return out, nil
}

// waitOverlayCandidates waits until the mounted overlay's candidate clusters exactly match
// want (converged + stable).
func waitOverlayCandidates(ctx context.Context, c *envconf.Config, want []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[w] = true
	}
	for {
		profiles, err := overlayProfiles(ctx, c)
		if err == nil && len(profiles) == len(wantSet) {
			ok := true
			for cluster := range profiles {
				if !wantSet[cluster] {
					ok = false
					break
				}
			}
			if ok {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("overlay candidates did not converge to %v (have %v)", want, keysOf(ctx, c))
		}
		time.Sleep(2 * time.Second)
	}
}

func keysOf(ctx context.Context, c *envconf.Config) []string {
	profiles, _ := overlayProfiles(ctx, c)
	out := make([]string, 0, len(profiles))
	for k := range profiles {
		out = append(out, k)
	}
	return out
}

// waitDeploymentRolledOut blocks until a Deployment has fully rolled onto its current
// template (controller observed the latest generation and every replica is updated+ready).
func waitDeploymentRolledOut(ctx context.Context, c *envconf.Config, namespace, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var dep appsv1.Deployment
		if err := c.Client().Resources().Get(ctx, name, namespace, &dep); err != nil {
			return err
		}
		want := int32(1)
		if dep.Spec.Replicas != nil {
			want = *dep.Spec.Replicas
		}
		st := dep.Status
		if st.ObservedGeneration >= dep.Generation &&
			st.UpdatedReplicas == want && st.ReadyReplicas == want &&
			st.Replicas == want && st.AvailableReplicas == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("deployment %s/%s not rolled out: gen=%d observed=%d updated=%d ready=%d replicas=%d want=%d",
				namespace, name, dep.Generation, st.ObservedGeneration, st.UpdatedReplicas, st.ReadyReplicas, st.Replicas, want)
		}
		time.Sleep(2 * time.Second)
	}
}

func deploymentObj(ns, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("apps/v1")
	u.SetKind("Deployment")
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

func serviceObj(ns, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("v1")
	u.SetKind("Service")
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}
