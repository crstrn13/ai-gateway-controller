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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	inferencev1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
	"github.com/praxis-proxy/dockyard/feature"
	"github.com/praxis-proxy/dockyard/gateway"
)

// Stage 2 (increment 9): the provider-CA trust assertions ported from e2e-extproc.sh
// (38-41). A provider's transport CA can be supplied two ways: tls.caCertificates (a
// file mounted into the gateway) or tls.credentialName (a Secret in the gateway
// namespace delivered over Istio SDS). The credentialName path must fail closed with a
// generic message when the Secret is absent (38), verify the provider once the Secret
// exists and reflect it in the DestinationRule (39), fail TLS verification when no CA is
// configured at all (40), and recover when the file CA is restored (41).
const (
	externalProviderName = "provider-a"
	providerCASecret     = "external-model-e2e-provider-ca"
	providerCAConfigMap  = "external-model-e2e-provider-ca"
	providerCAPath       = "/etc/external-model-e2e/provider-ca/ca.crt"
	destinationRuleName  = "provider-provider-a"
)

func TestProviderCATrust(t *testing.T) {
	f := features.New("Provider CA trust: tls.credentialName (SDS) vs tls.caCertificates (file)").
		Assess("missing credentialName fails closed generically, present verifies and serves, absent CA fails TLS, file CA recovers", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
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
			authed := gateway.New(gwFwd.URL("http"), gateway.WithBearer(key))
			demoPath := "/" + tenantNamespace + "/demo/v1/chat/completions"
			payload := map[string]any{"model": "demo", "messages": []map[string]any{{"role": "user", "content": "hello"}}}

			// Establish the provider-a baseline deterministically: the attribution checks
			// below require provider-a to be the sole active route, independent of whatever
			// weight state a previously-run test may still be converging out of.
			if err := patchExternalModelWeights(ctx, c, 1, 0); err != nil {
				t.Fatalf("set provider-a baseline: %v", err)
			}
			if err := waitOverlay(ctx, c, "a", 2*time.Minute); err != nil {
				t.Fatalf("provider-a baseline did not converge: %v", err)
			}

			// Always restore the file-based CA and remove the SDS secret (assertion 41).
			defer func() {
				if err := setProviderConfig(ctx, c, map[string]string{"tls.caCertificates": providerCAPath}); err != nil {
					t.Errorf("restore file CA config: %v", err)
				}
				_ = deleteSecret(ctx, c, apiNamespace, providerCASecret)
				if err := waitProviderPhase(ctx, c, "Ready", 2*time.Minute); err != nil {
					t.Errorf("provider did not recover to Ready: %v", err)
				}
				if _, err := retryRequestStatus(ctx, authed, demoPath, payload, http.StatusOK, 2*time.Minute); err != nil {
					t.Errorf("request did not recover to 200: %v", err)
				}
			}()

			// 38. credentialName set but the Secret is absent → fail closed, generically.
			_ = deleteSecret(ctx, c, apiNamespace, providerCASecret)
			if err := setProviderConfig(ctx, c, map[string]string{"tls.credentialName": providerCASecret}); err != nil {
				t.Fatalf("set credentialName config: %v", err)
			}
			if err := waitProviderPhase(ctx, c, "Failed", 2*time.Minute); err != nil {
				t.Fatalf("provider did not fail on missing credentialName secret: %v", err)
			}
			msg, err := providerReadyMessage(ctx, c)
			if err != nil {
				t.Fatalf("read provider Ready condition: %v", err)
			}
			if !strings.Contains(msg, "tls.credentialName must name a Secret in the Gateway namespace") {
				t.Errorf("missing-credential message not generic enough: %q", msg)
			}
			if strings.Contains(msg, apiNamespace+"/") {
				t.Errorf("missing-credential message leaks the Secret namespace: %q", msg)
			}

			// 39. Create the Secret from the provider-CA ConfigMap → provider verifies,
			// the DestinationRule names the credential (no file caCertificates), serves 200.
			if err := createCASecretFromConfigMap(ctx, c); err != nil {
				t.Fatalf("create provider-CA secret: %v", err)
			}
			if err := waitProviderPhase(ctx, c, "Ready", 2*time.Minute); err != nil {
				t.Fatalf("provider did not become Ready with credentialName: %v", err)
			}
			tls, err := destinationRuleTLS(ctx, c)
			if err != nil {
				t.Fatalf("read DestinationRule tls: %v", err)
			}
			if tls["credentialName"] != providerCASecret {
				t.Errorf("DestinationRule credentialName = %v, want %q", tls["credentialName"], providerCASecret)
			}
			if _, has := tls["caCertificates"]; has {
				t.Errorf("DestinationRule still has caCertificates under credentialName mode: %v", tls["caCertificates"])
			}
			ok, err := retryRequestStatus(ctx, authed, demoPath, payload, http.StatusOK, 2*time.Minute)
			if err != nil {
				t.Fatalf("credentialName request did not reach 200: %v", err)
			}
			assertBackend(t, ok, "katan-a-", "katan-b-")

			// 40. Remove the CA entirely → the gateway verifies against system roots and
			// the private cert fails verification (503 CERTIFICATE_VERIFY_FAILED).
			if err := setProviderConfig(ctx, c, map[string]string{}); err != nil {
				t.Fatalf("clear provider CA config: %v", err)
			}
			if err := waitRequestCertVerifyFailed(ctx, authed, demoPath, payload, 2*time.Minute); err != nil {
				t.Fatalf("request did not fail TLS verification without a CA: %v", err)
			}
			noCATLS, err := destinationRuleTLS(ctx, c)
			if err != nil {
				t.Fatalf("read DestinationRule tls (no CA): %v", err)
			}
			if _, has := noCATLS["credentialName"]; has {
				t.Errorf("DestinationRule still names a credential with no CA configured: %v", noCATLS)
			}
			if _, has := noCATLS["caCertificates"]; has {
				t.Errorf("DestinationRule still has caCertificates with no CA configured: %v", noCATLS)
			}

			// 41 asserted by the deferred restore (file CA, provider Ready, request 200).
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// setProviderConfig replaces the ExternalProvider's spec.config wholesale (retry-on-conflict).
func setProviderConfig(ctx context.Context, c *envconf.Config, config map[string]string) error {
	r := c.Client().Resources()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ep inferencev1alpha1.ExternalProvider
		if err := r.Get(ctx, externalProviderName, tenantNamespace, &ep); err != nil {
			return err
		}
		ep.Spec.Config = config
		return r.Update(ctx, &ep)
	})
}

// waitProviderPhase polls the ExternalProvider status.phase until it equals wanted.
func waitProviderPhase(ctx context.Context, c *envconf.Config, wanted string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var ep inferencev1alpha1.ExternalProvider
		if err := c.Client().Resources().Get(ctx, externalProviderName, tenantNamespace, &ep); err != nil {
			return err
		}
		if ep.Status.Phase == wanted {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("provider phase = %q, want %q", ep.Status.Phase, wanted)
		}
		time.Sleep(2 * time.Second)
	}
}

// providerReadyMessage returns the message of the ExternalProvider's Ready condition.
func providerReadyMessage(ctx context.Context, c *envconf.Config) (string, error) {
	var ep inferencev1alpha1.ExternalProvider
	if err := c.Client().Resources().Get(ctx, externalProviderName, tenantNamespace, &ep); err != nil {
		return "", err
	}
	for _, cond := range ep.Status.Conditions {
		if cond.Type == "Ready" {
			return cond.Message, nil
		}
	}
	return "", nil
}

// createCASecretFromConfigMap mints the SDS provider-CA Secret in the gateway namespace
// from the provider-CA ConfigMap's ca.crt (key "cacert", as Istio expects).
func createCASecretFromConfigMap(ctx context.Context, c *envconf.Config) error {
	r := c.Client().Resources()
	var cm corev1.ConfigMap
	if err := r.Get(ctx, providerCAConfigMap, apiNamespace, &cm); err != nil {
		return err
	}
	ca := cm.Data["ca.crt"]
	if ca == "" {
		return fmt.Errorf("provider-CA configmap has no ca.crt")
	}
	err := r.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: providerCASecret, Namespace: apiNamespace},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"cacert": ca},
	})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// destinationRuleTLS reads the DestinationRule's trafficPolicy.tls as a plain map.
func destinationRuleTLS(ctx context.Context, c *envconf.Config) (map[string]any, error) {
	dr := &unstructured.Unstructured{}
	dr.SetGroupVersionKind(schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1", Kind: "DestinationRule"})
	if err := c.Client().Resources().Get(ctx, destinationRuleName, apiNamespace, dr); err != nil {
		return nil, err
	}
	tls, found, err := unstructured.NestedMap(dr.Object, "spec", "trafficPolicy", "tls")
	if err != nil || !found {
		return map[string]any{}, err
	}
	return tls, nil
}

// retryRequestStatus retries the gateway request until it returns wantStatus or timeout.
func retryRequestStatus(ctx context.Context, client *gateway.Client, path string, payload any, wantStatus int, timeout time.Duration) (*gateway.Response, error) {
	deadline := time.Now().Add(timeout)
	var last *gateway.Response
	for {
		resp, err := client.Post(ctx, path, payload)
		if err == nil {
			last = resp
			if resp.StatusCode == wantStatus {
				return resp, nil
			}
		}
		if time.Now().After(deadline) {
			code := -1
			if last != nil {
				code = last.StatusCode
			}
			return last, fmt.Errorf("status never reached %d (last %d)", wantStatus, code)
		}
		time.Sleep(2 * time.Second)
	}
}

// waitRequestCertVerifyFailed retries until the gateway returns 503 with a TLS
// certificate-verification failure in the body.
func waitRequestCertVerifyFailed(ctx context.Context, client *gateway.Client, path string, payload any, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		resp, err := client.Post(ctx, path, payload)
		if err == nil && resp.StatusCode == http.StatusServiceUnavailable &&
			strings.Contains(resp.String(), "CERTIFICATE_VERIFY_FAILED") {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("never saw 503 CERTIFICATE_VERIFY_FAILED")
		}
		time.Sleep(2 * time.Second)
	}
}
