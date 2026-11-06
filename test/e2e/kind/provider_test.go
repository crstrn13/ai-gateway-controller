//go:build e2e

package kind

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	inferencev1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
)

// TestProviderTenantNotReady is the first genuinely meaningful e2e assertion:
// it proves this repo's controller is really running and reconciling against a
// real (kind) cluster. We create an ExternalProvider in a namespace that has no
// MaaSTenantConfig; the controller must report status.phase=Pending with
// reason=TenantNotReady, because it cannot resolve a tenant. No MaaS/Istio
// operators are involved — only the CRDs and the controller.
func TestProviderTenantNotReady(t *testing.T) {
	const (
		ns           = "e2e-no-tenant"
		providerName = "provider-a"
	)

	feature := features.New("ExternalProvider reports TenantNotReady without a tenant").
		Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			r := c.Client().Resources()
			if err := inferencev1alpha1.AddToScheme(r.GetScheme()); err != nil {
				t.Fatalf("add scheme: %v", err)
			}
			if err := r.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			provider := &inferencev1alpha1.ExternalProvider{
				ObjectMeta: metav1.ObjectMeta{Name: providerName, Namespace: ns},
				Spec: inferencev1alpha1.ExternalProviderSpec{
					Provider: "openai",
					Endpoint: "api.example.com",
					Auth: inferencev1alpha1.AuthConfig{
						Type:      "apikey",
						SecretRef: inferencev1alpha1.NameReference{Name: "provider-a-key"},
					},
				},
			}
			if err := r.Create(ctx, provider); err != nil {
				t.Fatalf("create ExternalProvider: %v", err)
			}
			return ctx
		}).
		Assess("status becomes Pending / TenantNotReady", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			r := c.Client().Resources()
			provider := &inferencev1alpha1.ExternalProvider{
				ObjectMeta: metav1.ObjectMeta{Name: providerName, Namespace: ns},
			}
			err := wait.For(
				conditions.New(r).ResourceMatch(provider, func(obj k8s.Object) bool {
					p, ok := obj.(*inferencev1alpha1.ExternalProvider)
					if !ok || p.Status.Phase != "Pending" {
						return false
					}
					ready := meta.FindStatusCondition(p.Status.Conditions, "Ready")
					return ready != nil && ready.Reason == "TenantNotReady"
				}),
				wait.WithTimeout(2*time.Minute),
				wait.WithInterval(2*time.Second),
			)
			if err != nil {
				t.Fatalf("provider never reached Pending/TenantNotReady (phase=%q conditions=%+v): %v",
					provider.Status.Phase, provider.Status.Conditions, err)
			}
			ready := meta.FindStatusCondition(provider.Status.Conditions, "Ready")
			t.Logf("provider status: phase=%s reason=%s message=%q",
				provider.Status.Phase, ready.Reason, ready.Message)
			return ctx
		}).
		Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			_ = c.Client().Resources().Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
			return ctx
		}).
		Feature()

	testenv.Test(t, feature)
}
