//go:build e2e

package kindenv

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/praxis-proxy/dockyard/funcs"
	"github.com/praxis-proxy/dockyard/gateway"
)

// Stage 2: the core ExtProc routing qualification ported from e2e-extproc.sh. This
// is the first slice — the gateway-side HTTP status assertions (unauthenticated,
// authorized provider request, unknown model). The deeper assertions (backend
// attribution via katan receipts, overlay-digest convergence, caller-selection
// spoof resistance, envoy-admin cluster health) are follow-on increments.
//
// Access mirrors run.sh: no metallb, so reach the gateway and maas-api via
// port-forward; mint a MaaS API key over the maas-api HTTPS port-forward using the
// run-owned CA, then drive the gateway with it.
//
// NOTE (verify against the live platform): the gateway Service name/port, the
// request model/path, and the subscription name are taken from e2e-extproc.sh and
// have not yet been run end-to-end here.
const (
	tenantNamespace = "models-as-a-service"
	// gatewayService is the istio-provisioned Service for the Gateway (<name>-istio),
	// which e2e-extproc.sh port-forwards on :80 — not the Gateway object's own name.
	gatewayService    = "maas-default-gateway-istio"
	gatewayHTTPPort   = 80
	maasAPIService    = "maas-api"
	maasAPIHTTPSPort  = 8443
	maasAPIServerName = "maas-api.maas-system.svc.cluster.local"
	keySubscription   = "kind-e2e-subscription"
)

func TestExtProcRouting(t *testing.T) {
	f := features.New("ExtProc routing: auth gate, provider request, unknown model").
		Assess("gateway returns 401/200/404 for the core request matrix", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			// Port-forward the maas-api (HTTPS) and mint an ephemeral key.
			apiFwd, err := funcs.PortForwardService(ctx, c, "maas-system", maasAPIService, maasAPIHTTPSPort)
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

			// Port-forward the gateway (HTTP) and drive the request matrix.
			gwFwd, err := funcs.PortForwardService(ctx, c, "maas-system", gatewayService, gatewayHTTPPort)
			if err != nil {
				t.Fatalf("port-forward gateway: %v", err)
			}
			defer gwFwd.Close()

			unauth := gateway.New(gwFwd.URL("http"))
			authed := gateway.New(gwFwd.URL("http"), gateway.WithBearer(key))
			demoPath := "/" + tenantNamespace + "/demo/v1/chat/completions"
			chat := gateway.ChatRequest{Model: "demo", Messages: gateway.User("hello")}

			// 10. No credential → 401.
			assertStatus(t, postChat(ctx, t, unauth, demoPath, chat), http.StatusUnauthorized, "unauthenticated")

			// 11/12. Authorized request → 200, attributed to katan-a (provider A).
			a := postChat(ctx, t, authed, demoPath, chat)
			assertStatus(t, a, http.StatusOK, "provider_a_request")
			assertBackend(t, a, "katan-a-", "katan-b-")

			// 13/14. A caller-supplied routing candidate must be ignored (still katan-a).
			spoof := postChat(ctx, t, authed, demoPath, chat, gateway.Header("x-ai-routing-candidate", "provider-provider-b"))
			assertStatus(t, spoof, http.StatusOK, "caller_selection_spoof")
			assertBackend(t, spoof, "katan-a-", "katan-b-")

			// 16. Unknown model → 404.
			unknown := postChat(ctx, t, authed, "/"+tenantNamespace+"/unknown-model/v1/chat/completions",
				gateway.ChatRequest{Model: "unknown-model", Messages: gateway.User("hello")})
			assertStatus(t, unknown, http.StatusNotFound, "unknown_model")
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// postChat POSTs a chat request to path and returns the response (fatal on transport error).
func postChat(ctx context.Context, t *testing.T, c *gateway.Client, path string, req gateway.ChatRequest, opts ...gateway.RequestOption) *gateway.Response {
	t.Helper()
	resp, err := c.Post(ctx, path, chatPayload(req), opts...)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// assertStatus checks an HTTP status code.
func assertStatus(t *testing.T, r *gateway.Response, want int, name string) {
	t.Helper()
	if r.StatusCode != want {
		t.Errorf("%s: status = %d, want %d (body: %s)", name, r.StatusCode, want, truncate(r.String(), 200))
	}
}

// assertBackend checks the response body was stamped by the expected katan backend
// (wantMarker present, otherMarker absent) — the e2e-extproc.sh attribution contract.
func assertBackend(t *testing.T, r *gateway.Response, wantMarker, otherMarker string) {
	t.Helper()
	body := r.String()
	if !strings.Contains(body, wantMarker) || strings.Contains(body, otherMarker) {
		t.Errorf("attribution: want %q present and %q absent", wantMarker, otherMarker)
	}
}

func chatPayload(r gateway.ChatRequest) map[string]any {
	msgs := make([]map[string]any, len(r.Messages))
	for i, m := range r.Messages {
		msgs[i] = map[string]any{"role": m.Role, "content": m.Content}
	}
	return map[string]any{"model": r.Model, "messages": msgs}
}

// runOwnedCA reads the run-owned CA PEM from the Authorino CA ConfigMap.
func runOwnedCA(ctx context.Context, c *envconf.Config) ([]byte, error) {
	var cm corev1.ConfigMap
	if err := c.Client().Resources().Get(ctx, "authorino-maas-api-ca", "kuadrant-system", &cm); err != nil {
		return nil, err
	}
	ca := cm.Data["ca.crt"]
	if ca == "" {
		return nil, fmt.Errorf("authorino-maas-api-ca has no ca.crt")
	}
	return []byte(ca), nil
}

// mintMaaSKey creates an ephemeral MaaS API key over the maas-api HTTPS port-forward,
// verifying the server cert against the run-owned CA (SNI pinned to the Service DNS).
func mintMaaSKey(ctx context.Context, apiAddr string, caPEM []byte) (string, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return "", fmt.Errorf("invalid CA PEM")
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: maasAPIServerName},
		},
	}
	body := fmt.Sprintf(`{"name":"kind-e2e-go","ephemeral":true,"subscription":%q}`, keySubscription)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+apiAddr+"/v1/api-keys", strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("X-MaaS-Username", "kind-user")
	req.Header.Set("X-MaaS-Group", `["system:authenticated"]`)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create key: status %d", resp.StatusCode)
	}
	var out struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Key == "" {
		return "", fmt.Errorf("key response had no key field")
	}
	return out.Key, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
