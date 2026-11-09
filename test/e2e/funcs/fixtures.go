package funcs

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	inferencev1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
	"github.com/opendatahub-io/ai-gateway-controller/pkg/tenant"
)

// This file holds AI-Gateway code fixtures: a Ready Praxis tenant and the
// ExternalProvider/ExternalModel CRs the reconcilers turn into the data plane.
// Generic kind names only (match config's controller defaults).
const (
	tenantsNamespace = "ai-tenants"       // where AITenants live
	infraNamespace   = "ai-gateway-infra" // the tenant's published infra namespace
	fixtureGateway   = "ai-gateway"       // matches config's default gateway name
	fixtureGatewayNS = "istio-system"     // matches config's default gateway namespace
)

// crClient builds a controller-runtime client with the inference types registered,
// for creating CRs + writing their status (status subresource writes are awkward
// through the e2e-framework client).
func crClient(t *testing.T, c *envconf.Config) crclient.Client {
	t.Helper()
	s := clientgoscheme.Scheme
	if err := inferencev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add inference scheme: %v", err)
	}
	cl, err := crclient.New(c.Client().RESTConfig(), crclient.Options{Scheme: s})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	return cl
}

// ReadyTenant establishes a Ready Praxis tenant the reconcilers render off: a
// Gateway, an AITenant (Active + Ready, gatewayRef set), and a MaaSTenantConfig
// opted into Praxis with status.infraNamespace published. Ported from the envtest
// maasFixture, adapted to generic kind names.
func ReadyTenant(ns string) features.Func {
	return func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Helper()
		cl := crClient(t, c)
		mustCreateNS(ctx, t, cl, ns)
		mustCreateNS(ctx, t, cl, tenantsNamespace)

		gateway := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "Gateway",
			"metadata":   map[string]any{"name": fixtureGateway, "namespace": fixtureGatewayNS},
			"spec": map[string]any{
				"gatewayClassName": "istio",
				"listeners": []any{map[string]any{
					"name": "http", "protocol": "HTTP", "port": int64(80),
					"allowedRoutes": map[string]any{"namespaces": map[string]any{"from": "All"}},
				}},
			},
		}}
		if err := cl.Create(ctx, gateway); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create Gateway: %v", err)
		}

		ait := tenant.NewAITenant()
		ait.SetName(ns)
		ait.SetNamespace(tenantsNamespace)
		ait.SetAnnotations(map[string]string{tenant.AnnotationPayloadProcessingType: tenant.PayloadProcessingBackendPraxis})
		ait.Object["spec"] = map[string]any{}
		if err := cl.Create(ctx, ait); err != nil {
			t.Fatalf("create AITenant: %v", err)
		}
		ait.Object["status"] = map[string]any{
			"phase":           tenant.AITenantPhaseActive,
			"tenantNamespace": ns,
			"gatewayRef":      map[string]any{"name": fixtureGateway, "namespace": fixtureGatewayNS},
			"conditions": []any{map[string]any{
				"type": tenant.AITenantConditionReady, "status": "True",
				"reason": "Reconciled", "message": "bootstrap resources reconciled",
				"observedGeneration": ait.GetGeneration(),
				"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
			}},
		}
		if err := cl.Status().Update(ctx, ait); err != nil {
			t.Fatalf("set AITenant status: %v", err)
		}

		mtc := tenant.NewMaasTenantConfig()
		mtc.SetName(tenant.MaasTenantConfigInstanceName)
		mtc.SetNamespace(ns)
		mtc.SetLabels(map[string]string{tenant.LabelManagedByAITenant: "true", tenant.LabelTenantName: ns})
		mtc.SetAnnotations(map[string]string{
			tenant.AnnotationPayloadProcessingType:   tenant.PayloadProcessingBackendPraxis,
			tenant.AnnotationPayloadProcessingStatus: tenant.PayloadProcessingStatusCleanupComplete,
			tenant.AnnotationAITenantName:            ns,
			tenant.AnnotationAITenantNamespace:       tenantsNamespace,
		})
		mtc.Object["spec"] = map[string]any{}
		if err := cl.Create(ctx, mtc); err != nil {
			t.Fatalf("create MaaSTenantConfig: %v", err)
		}
		// The reconciler adds its finalizer between create and this status write, so
		// retry on conflict with the latest object.
		err := wait.For(func(context.Context) (bool, error) {
			latest := tenant.NewMaasTenantConfig()
			if e := cl.Get(ctx, crclient.ObjectKeyFromObject(mtc), latest); e != nil {
				return false, nil
			}
			latest.Object["status"] = map[string]any{"infraNamespace": infraNamespace}
			return cl.Status().Update(ctx, latest) == nil, nil
		}, wait.WithTimeout(30*time.Second), wait.WithInterval(time.Second))
		if err != nil {
			t.Fatalf("publish MaaSTenantConfig status.infraNamespace: %v", err)
		}
		return ctx
	}
}

func mustCreateNS(ctx context.Context, t *testing.T, cl crclient.Client, name string) {
	t.Helper()
	err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace %s: %v", name, err)
	}
}

// --- CR builders -------------------------------------------------------------

// ProviderSecret is the apikey Secret an ExternalProvider references.
func ProviderSecret(name, ns string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string][]byte{"api-key": []byte("sim-key")},
	}
}

// ExternalProvider points the data plane at an OpenAI-compatible endpoint.
func ExternalProvider(name, ns, endpoint string) *inferencev1alpha1.ExternalProvider {
	return &inferencev1alpha1.ExternalProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: inferencev1alpha1.ExternalProviderSpec{
			Provider: "openai",
			Endpoint: endpoint,
			Auth: inferencev1alpha1.AuthConfig{
				Type:      "apikey",
				SecretRef: inferencev1alpha1.NameReference{Name: name},
			},
		},
	}
}

// ExternalModel maps a client-facing model name to a provider + target model.
func ExternalModel(name, ns, modelName, provider, targetModel string) *inferencev1alpha1.ExternalModel {
	return &inferencev1alpha1.ExternalModel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: inferencev1alpha1.ExternalModelSpec{
			ModelName: modelName,
			ExternalProviderRefs: []inferencev1alpha1.ExternalProviderRef{{
				Ref:         inferencev1alpha1.ExternalProviderReference{Name: provider},
				TargetModel: targetModel,
				APIFormat:   "openai-chat",
				Path:        "/v1/chat/completions",
			}},
		},
	}
}
