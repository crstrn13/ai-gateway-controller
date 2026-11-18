//go:build e2e

package product

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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
// Generic kind names only (match the product's controller defaults).
const (
	tenantsNamespace = "ai-tenants"       // where AITenants live
	infraNamespace   = "ai-gateway-infra" // the tenant's published infra namespace
	fixtureGateway   = "ai-gateway"       // matches the product's default gateway name
	fixtureGatewayNS = "istio-system"     // matches the product's default gateway namespace
)

// The ext-proc EnvoyFilter tells the gateway Envoy to read the CA that signs the
// ext-proc serving certs from an OpenShift service-ca path. On kind we mount our
// published cert-manager CA bundle (ConfigMap e2e-service-ca) into the gateway
// proxy there. The mount is a whole-directory mount (not a subPath file): the
// gateway proxy does not otherwise mount that dir, and a subPath file-mount fails
// with StartError because /var/run is a symlink. istio's userVolume annotations
// are ignored for Gateway API gateways, so MountGatewayCATrust patches the
// istio-provisioned Deployment directly (the patch persists; istio does not revert).
const (
	caTrustVolume    = "service-ca-trust"
	caTrustConfigMap = "e2e-service-ca"
	caTrustDir       = "/var/run/secrets/kubernetes.io/serviceaccount"
	gatewayProxyCtr  = "istio-proxy"
	gatewayDeploy    = "ai-gateway-istio" // istio names it <gateway>-istio
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

		// A permissive (anonymous) Kuadrant AuthPolicy on the gateway: it makes
		// Kuadrant attach the WasmPlugin the ext-proc EnvoyFilters anchor to, and
		// it lets unauthenticated traffic through (no MaaS API key needed on kind).
		authPolicy := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "kuadrant.io/v1",
			"kind":       "AuthPolicy",
			"metadata":   map[string]any{"name": "allow-all", "namespace": fixtureGatewayNS},
			"spec": map[string]any{
				"targetRef": map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": fixtureGateway},
				"rules": map[string]any{
					"authentication": map[string]any{"anonymous": map[string]any{"anonymous": map[string]any{}}},
				},
			},
		}}
		if err := cl.Create(ctx, authPolicy); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create gateway AuthPolicy: %v", err)
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

// ExtProcServingCerts issues the three ext-proc serving-cert secrets a tenant's
// data plane needs, via cert-manager Certificates signed by the run CA issuer
// (e2e-ca-issuer). It stands in for the OpenShift service-ca operator: the
// controller annotates each ext-proc Service with a serving-cert-secret name, and
// these Certificates write exactly those secrets (SANs = the service's cluster
// DNS). The external-model ext-proc is in the tenant namespace; the pre/post
// processing ext-procs are in the gateway namespace.
func ExtProcServingCerts(ns string) features.Func {
	return func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Helper()
		cl := crClient(t, c)
		specs := []struct{ secret, svc, svcNS string }{
			{tenant.PayloadProcessingExternalModelTLSSecretName(ns), tenant.PayloadProcessingExternalModelServiceName(ns), ns},
			{tenant.PayloadProcessingTLSSecretName(ns), tenant.PayloadProcessingServiceName(ns), fixtureGatewayNS},
			{tenant.PayloadPreProcessingTLSSecretName(ns), tenant.PayloadPreProcessingServiceName(ns), fixtureGatewayNS},
		}
		for _, s := range specs {
			cert := certificate(s.secret, s.svcNS, s.secret, []string{
				fmt.Sprintf("%s.%s.svc", s.svc, s.svcNS),
				fmt.Sprintf("%s.%s.svc.cluster.local", s.svc, s.svcNS),
			})
			if err := cl.Create(ctx, cert); err != nil && !apierrors.IsAlreadyExists(err) {
				t.Fatalf("create Certificate %s/%s: %v", s.svcNS, s.secret, err)
			}
		}
		return ctx
	}
}

// MountGatewayCATrust patches the istio-provisioned gateway Deployment to mount
// the published CA bundle (ConfigMap e2e-service-ca) at the service-ca path the
// ext-proc EnvoyFilter reads. It stands in for OpenShift injecting service-ca.crt
// into pods. Idempotent (shared gateway across tests) and waits for the gateway
// Deployment to exist first (istio creates it from the Gateway CR).
func MountGatewayCATrust() features.Func {
	return func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Helper()
		cl := crClient(t, c)
		key := crclient.ObjectKey{Namespace: fixtureGatewayNS, Name: gatewayDeploy}

		var dep appsv1.Deployment
		if err := wait.For(func(context.Context) (bool, error) {
			return cl.Get(ctx, key, &dep) == nil, nil
		}, wait.WithTimeout(2*time.Minute), wait.WithInterval(3*time.Second)); err != nil {
			t.Fatalf("gateway Deployment %s not created: %v", gatewayDeploy, err)
		}

		for _, v := range dep.Spec.Template.Spec.Volumes {
			if v.Name == caTrustVolume {
				return ctx // already patched (shared gateway)
			}
		}
		dep.Spec.Template.Spec.Volumes = append(dep.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: caTrustVolume,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: caTrustConfigMap},
				},
			},
		})
		for i := range dep.Spec.Template.Spec.Containers {
			if dep.Spec.Template.Spec.Containers[i].Name == gatewayProxyCtr {
				dep.Spec.Template.Spec.Containers[i].VolumeMounts = append(
					dep.Spec.Template.Spec.Containers[i].VolumeMounts,
					corev1.VolumeMount{Name: caTrustVolume, MountPath: caTrustDir, ReadOnly: true},
				)
			}
		}
		if err := cl.Update(ctx, &dep); err != nil {
			t.Fatalf("patch gateway Deployment with CA trust mount: %v", err)
		}
		return ctx
	}
}

// certificate builds a cert-manager Certificate (unstructured) signed by the run
// CA ClusterIssuer, writing secretName in namespace ns with the given DNS SANs.
func certificate(name, ns, secretName string, dnsNames []string) *unstructured.Unstructured {
	dns := make([]any, len(dnsNames))
	for i, d := range dnsNames {
		dns[i] = d
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{
			"secretName": secretName,
			"dnsNames":   dns,
			"privateKey": map[string]any{"algorithm": "RSA", "size": int64(2048)},
			"issuerRef":  map[string]any{"name": "e2e-ca-issuer", "kind": "ClusterIssuer", "group": "cert-manager.io"},
		},
	}}
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

// WeightedRef is one weighted provider binding for WeightedExternalModel: the
// ExternalProvider name, the provider-side target model, and the relative weight
// (0 disables the binding; equal positive weights select uniformly, unequal
// positive weights proportionally — Praxis weightedRandom).
type WeightedRef struct {
	Provider    string
	TargetModel string
	Weight      int
}

// WeightedExternalModel maps one client-facing model to several weighted
// providers — the fixture for provider-selection tests. Each ref renders an
// overlay candidate with its traffic_weight.
func WeightedExternalModel(name, ns, modelName string, refs ...WeightedRef) *inferencev1alpha1.ExternalModel {
	prefs := make([]inferencev1alpha1.ExternalProviderRef, len(refs))
	for i, r := range refs {
		w := r.Weight
		prefs[i] = inferencev1alpha1.ExternalProviderRef{
			Ref:         inferencev1alpha1.ExternalProviderReference{Name: r.Provider},
			TargetModel: r.TargetModel,
			APIFormat:   "openai-chat",
			Path:        "/v1/chat/completions",
			Weight:      &w,
		}
	}
	return &inferencev1alpha1.ExternalModel{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: inferencev1alpha1.ExternalModelSpec{
			ModelName:            modelName,
			ExternalProviderRefs: prefs,
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
