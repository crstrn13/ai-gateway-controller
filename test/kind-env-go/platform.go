//go:build e2e

// Package kindenv is the Go/dockyard rewrite of the test/kind-env qualification
// harness (see MIGRATION-PLAN.md). This is Stage 1a — the "strangler bridge": the
// MaaSPlatform vessel provisions by delegating to the existing run.sh, so the Go
// suite runs end to end today; each provisioning phase is then replaced with native
// dockyard installers incrementally, and the Features (Stage 2+) replace the
// shell/Python qualification suites.
package kindenv

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"

	aigatewayv1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/aigateway/v1alpha1"
	inferencev1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
	"github.com/praxis-proxy/dockyard/config"
	"github.com/praxis-proxy/dockyard/funcs"
)

// Namespace label keys applied during provisioning (run.sh:671-673).
const (
	gatewayTenantLabel = "local-env.opendatahub.io/gateway-tenant"
	aiGatewayTenant    = "ai-gateway.opendatahub.io/tenant"
	managedByAITenant  = "maas.opendatahub.io/managed-by-aitenant"
)

const gatewayNamespace = "maas-system"

// katanPullImage is the pinned public llm-katan amd64 digest the fixture backends run
// (run.sh lib/image_refs.sh). Pulled, retagged to a plain local tag, and kind-loaded.
const katanPullImage = "ghcr.io/nerdalert/llm-katan@sha256:a8bf18109e2db641ef4a63efe65f69d4d6554f1128a174053de89a8b81b4284d"

// Infra versions pinned to match run.sh (end-state equivalence, not command parity).
const (
	gatewayAPIVersion = "v1.5.1"
	certManagerVer    = "v1.15.3"
	kuadrantVer       = "1.3.1"
	istioVer          = "1.27.3"
)

// platformImages is the set of images run.sh loads into the kind node. Building
// them (notably ext-proc's Cargo/Containerfile vendoring) stays product-specific
// and out of scope for L1; the native provisioner loads a provided/built set.
type platformImages struct {
	aiController     string
	katan            string
	providerRecorder string
	extproc          string
	maasAPI          string
	maasController   string
}

func platformImagesFromEnv() platformImages {
	return platformImages{
		aiController:     cmp.Or(os.Getenv("AI_CONTROLLER_IMAGE"), "ai-gateway-controller:external-model-two-plane"),
		katan:            cmp.Or(os.Getenv("KATAN_KIND_IMAGE"), "llm-katan:external-model-two-plane"),
		providerRecorder: cmp.Or(os.Getenv("PROVIDER_RECORDER_IMAGE"), "provider-recorder:external-model-two-plane"),
		extproc:          cmp.Or(os.Getenv("EXTPROC_IMAGE"), "praxis-extproc:dev"),
		maasAPI:          cmp.Or(os.Getenv("MAAS_API_IMAGE"), "maas-api:external-model-two-plane"),
		maasController:   cmp.Or(os.Getenv("MAAS_CONTROLLER_IMAGE"), "maas-controller:external-model-two-plane"),
	}
}

// MaaSPlatform provisions the kind-env MaaS platform for the suite. TestMain calls
// its InstallDeps/AddTypesToScheme/Teardown directly (no framework seam). In the
// strangler-bridge stage it owns no in-process control plane: run.sh builds and
// deploys the controller image and the whole MaaS platform. InstallDeps delegates
// to run.sh --provision; Teardown runs run.sh --destroy. As phases are ported, the
// run.sh delegation shrinks and InstallDeps composes dockyard installers instead.
type MaaSPlatform struct {
	// runScript is the absolute path to test/kind-env/run.sh.
	runScript string
	// cluster is the kind cluster name (run.sh's LOCAL_ENV_CLUSTER).
	cluster string
	// evidenceRoot is run.sh's LOCAL_ENV_EVIDENCE_ROOT (optional).
	evidenceRoot string
	// native selects the in-progress native provisioner over the run.sh bridge.
	native bool
	// images is the set loaded into the kind node by native provisioning.
	images platformImages
	// manifestsDir is test/kind-env/manifests (reused by the native provisioner).
	manifestsDir string
	// maasControllerRepo / kserveRepo are the source checkouts run.sh applies
	// manifests/CRDs from (MAAS_CONTROLLER_REPO / KSERVE_REPO).
	maasControllerRepo string
	kserveRepo         string
	// extprocRepo is the praxis-extproc checkout the ext-proc image builds from.
	extprocRepo string
	// aigcRoot is this repo's root (for config/crd and config/self kustomize bases).
	aigcRoot string
	// maasAPIRuntimeImage is the image the MaaS controller reconciles its API
	// Deployment to (imagePullPolicy: Always → run-owned registry ref). Defaults to
	// the loaded maas-api image until the local-registry phase (PG4b) lands.
	maasAPIRuntimeImage string
	// ca is the run-owned CA, retained after provisionCerts to sign the katan
	// provider serving certs and publish the provider-trust ConfigMap in PG5b.
	ca *funcs.CA
}

// NewMaaSPlatform builds the vessel. The source-repo and image environment
// variables run.sh needs (PRAXIS_EXTPROC_REPO, MAAS_CONTROLLER_REPO, KSERVE_REPO,
// KUADRANT_OPERATOR_REPO, EXTPROC_IMAGE, …) are inherited from the process
// environment, exactly as run.sh expects them today.
func NewMaaSPlatform(runScript, cluster, evidenceRoot string, native bool) *MaaSPlatform {
	images := platformImagesFromEnv()
	return &MaaSPlatform{
		runScript:           runScript,
		cluster:             cluster,
		evidenceRoot:        evidenceRoot,
		native:              native,
		images:              images,
		manifestsDir:        filepath.Join(filepath.Dir(runScript), "manifests"),
		maasControllerRepo:  os.Getenv("MAAS_CONTROLLER_REPO"),
		kserveRepo:          os.Getenv("KSERVE_REPO"),
		extprocRepo:         os.Getenv("PRAXIS_EXTPROC_REPO"),
		aigcRoot:            filepath.Clean(filepath.Join(filepath.Dir(runScript), "..", "..")),
		maasAPIRuntimeImage: cmp.Or(os.Getenv("MAAS_API_RUNTIME_IMAGE"), images.maasAPI),
	}
}

// InstallDeps provisions the platform. Default (bridge): delegate the whole
// provisioning to run.sh --provision. Native (-native-provision): compose dockyard
// installers phase-group by phase-group; incomplete until parity, so it stays
// opt-in. Both run with -create-cluster=false (run.sh / native own cluster create).
func (p *MaaSPlatform) InstallDeps(e *config.Environment) []env.Func {
	if p.native {
		return p.provisionNative(e)
	}
	return []env.Func{
		func(ctx context.Context, _ *envconf.Config) (context.Context, error) {
			return ctx, p.run(ctx, "--provision")
		},
	}
}

// provisionNative is the in-progress native (Go/dockyard) provisioner that will
// replace run.sh. Implemented phase-group by phase-group; the remaining groups are
// marked TODO and must land before -native-provision is a working default.
//
// Phase-group 1 (done): load the platform image set into the kind node, then the
// cluster infra (Gateway API, cert-manager, istio, kuadrant). istio is installed
// before kuadrant so kuadrant's operator restart (inside InstallKuadrant)
// rediscovers the Gateway API provider — matching run.sh's ordering.
func (p *MaaSPlatform) provisionNative(e *config.Environment) []env.Func {
	cluster := p.cluster
	isKind := e.ShouldCreateCluster() // native cluster-create is a later phase-group

	deps := funcs.NewDeps().
		// phase-group 0: build images host-side, in parallel. The slow Rust ext-proc
		// build overlaps the Go builds — the big cold-provision win (run.sh:471-575).
		WithParallel(
			funcs.BuildImageFrom(p.aigcRoot, filepath.Join(p.aigcRoot, "Dockerfile"), p.images.aiController),
			funcs.BuildImageFrom(p.extprocRepo, filepath.Join(p.extprocRepo, "Containerfile"), p.images.extproc),
			funcs.BuildImageFrom(filepath.Join(p.maasControllerRepo, "maas-api"), filepath.Join(p.maasControllerRepo, "maas-api", "Dockerfile"), p.images.maasAPI),
			funcs.BuildImageFrom(p.maasControllerRepo, filepath.Join(p.maasControllerRepo, "maas-controller", "Dockerfile"), p.images.maasController),
			p.buildRecorder,
		).
		// phase-group 1a: load the built images + pull/tag/load the public katan digest
		// into the node (independent → parallel; the cluster already exists).
		WithParallel(
			funcs.LoadImage(cluster, p.images.aiController, isKind),
			funcs.LoadImage(cluster, p.images.extproc, isKind),
			funcs.LoadImage(cluster, p.images.maasAPI, isKind),
			funcs.LoadImage(cluster, p.images.maasController, isKind),
			funcs.LoadImage(cluster, p.images.providerRecorder, isKind),
			funcs.EnvFuncs( // pull→tag→load must stay ordered
				funcs.PullImage(katanPullImage),
				funcs.TagImage(katanPullImage, p.images.katan),
				funcs.LoadImage(cluster, p.images.katan, isKind),
			),
		).
		// phase-group 1b: cluster infra. Gateway API + cert-manager are independent
		// (parallel); istio needs the Gateway API CRDs first, and kuadrant is installed
		// after istio so its operator restart rediscovers the Gateway API provider.
		WithParallel(
			funcs.InstallGatewayAPI(funcs.GatewayAPIVersion(gatewayAPIVersion)),
			funcs.InstallCertManager(funcs.CertManagerVersion(certManagerVer)),
		).
		WithIstio(funcs.IstioVersion(istioVer), funcs.IstioInferenceExtension(true)).
		WithKuadrant(funcs.KuadrantVersion(kuadrantVer)).
		// phase-group 2: namespaces + labels (run.sh:666-674)
		WithNamespace(gatewayNamespace, nil).
		WithNamespace("opendatahub", nil).
		WithNamespace("models-as-a-service", map[string]string{gatewayTenantLabel: "models-as-a-service"}).
		WithNamespace("ai-tenant-tenant-b", map[string]string{
			gatewayTenantLabel: "ai-tenant-tenant-b",
			aiGatewayTenant:    "true",
			managedByAITenant:  "true",
		}).
		WithNamespace("ai-tenant-transition", map[string]string{gatewayTenantLabel: "ai-tenant-transition"}).
		WithNamespace("ai-tenants", nil).
		// phase-group 2: shared infra manifests (run.sh:675-682)
		WithManifest(filepath.Join(p.manifestsDir, "05-database.yaml")).
		WithManifest(filepath.Join(p.manifestsDir, "30-gateway.yaml")).
		With(p.ownerRefGatewayService("maas-default-gateway")).
		With(p.ownerRefGatewayService("maas-tenant-b-gateway")).
		With(p.ownerRefGatewayService("maas-transition-gateway")).
		// phase-group 2: MaaS networking + KServe CRDs (run.sh:683-686)
		With(funcs.Manifest(filepath.Join(p.maasControllerRepo, "deployment/base/networking/odh/kuadrant.yaml"))).
		With(funcs.Manifest(filepath.Join(p.kserveRepo, "test/crds/serving.kserve.io_all_crds.yaml"))).
		// phase-group 3: run-owned CA + serving certs + Authorino CA (run.sh:702-782)
		With(p.provisionCerts).
		// phase-group 4c: local registry → push maas-api, set MAAS_API_RUNTIME_IMAGE
		// (run.sh create_run_registry). Runs before the controller step consumes it.
		With(p.provisionRegistry).
		// phase-group 4: MaaS controller + maas-api RBAC (run.sh:719-736, 826-828)
		With(p.provisionMaaS).
		// phase-group 4b: optional translation-schema overlay + Authorino CA wiring
		With(p.applyTranslationSchema).
		With(p.wireAuthorinoCA).
		// phase-group 5a: deploy the ai-gateway-controller (run.sh:838-944)
		With(p.provisionAIGatewayController).
		// phase-group 5b: AITenants + Praxis handoff + provider certs + fixtures
		With(p.provisionTenants)

	// TODO refactor-later: gateway provider-CA volume patch (run.sh:1158+), the
	//   ExtProc uid-65532 security patch, the IPP bootstrap-operand handoff
	//   deletion, and the full evidence/provenance recording.
	return deps.Build()
}

// certValidity matches run.sh's 2-day run-owned certificate lifetime.
const certValidity = 48 * time.Hour

// maasServiceSANs returns the four DNS SANs run.sh uses for a maas-system Service
// serving cert: <base>, <base>.maas-system, .svc, .svc.cluster.local.
func maasServiceSANs(base string) []string {
	return []string{
		base,
		base + ".maas-system",
		base + ".maas-system.svc",
		base + ".maas-system.svc.cluster.local",
	}
}

// provisionCerts mints the run-owned CA and serving certificates kind lacks (no
// OpenShift service-ca) and publishes them as TLS Secrets + the Authorino CA
// ConfigMap (run.sh:702-782). Pure crypto/x509 — no openssl. The Authorino volume/
// env wiring that consumes the CA lands in phase-group 4.
func (p *MaaSPlatform) provisionCerts(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
	ca, err := funcs.GenerateCA("external-model-e2e-ca", certValidity)
	if err != nil {
		return ctx, fmt.Errorf("generate run-owned CA: %w", err)
	}
	p.ca = ca

	// Self-signed controller webhook/metrics cert (run.sh:703-709).
	ctrlCert, ctrlKey, err := funcs.GenerateSelfSignedServingCert(
		"maas-controller-webhook-service.maas-system.svc",
		[]string{
			"maas-controller-webhook-service",
			"maas-controller-webhook-service.maas-system.svc",
			"maas-controller-webhook-service.maas-system.svc.cluster.local",
			"maas-controller-metrics",
			"maas-controller-metrics.maas-system.svc",
			"maas-controller-metrics.maas-system.svc.cluster.local",
		}, certValidity)
	if err != nil {
		return ctx, fmt.Errorf("controller serving cert: %w", err)
	}

	// CA-signed maas-api serving certs for the three tenants (run.sh:751-773).
	apiCert, apiKey, err := ca.SignServingCert("maas-api.maas-system.svc", maasServiceSANs("maas-api"), certValidity)
	if err != nil {
		return ctx, fmt.Errorf("maas-api serving cert: %w", err)
	}
	tbCert, tbKey, err := ca.SignServingCert("maas-api-tenant-b.maas-system.svc", maasServiceSANs("maas-api-tenant-b"), certValidity)
	if err != nil {
		return ctx, fmt.Errorf("maas-api tenant-b serving cert: %w", err)
	}
	trCert, trKey, err := ca.SignServingCert("maas-api-transition.maas-system.svc", maasServiceSANs("maas-api-transition"), certValidity)
	if err != nil {
		return ctx, fmt.Errorf("maas-api transition serving cert: %w", err)
	}

	steps := []env.Func{
		funcs.CreateTLSSecret(gatewayNamespace, "maas-controller-webhook-cert", ctrlCert, ctrlKey),
		funcs.CreateTLSSecret(gatewayNamespace, "maas-controller-metrics-tls", ctrlCert, ctrlKey),
		funcs.CreateTLSSecret(gatewayNamespace, "maas-api-serving-cert", apiCert, apiKey),
		funcs.CreateTLSSecret(gatewayNamespace, "maas-api-tenant-b-serving-cert", tbCert, tbKey),
		funcs.CreateTLSSecret(gatewayNamespace, "maas-api-transition-serving-cert", trCert, trKey),
		funcs.CreateConfigMap("kuadrant-system", "authorino-maas-api-ca", map[string]string{"ca.crt": string(ca.CertPEM)}),
	}
	for _, step := range steps {
		if ctx, err = step(ctx, cfg); err != nil {
			return ctx, err
		}
	}
	return ctx, nil
}

// authorinoGVK is the Authorino CR kuadrant creates in kuadrant-system.
var authorinoGVK = schema.GroupVersionKind{
	Group: "operator.authorino.kuadrant.io", Version: "v1beta1", Kind: "Authorino",
}

// authorinoCAVolumePatch mounts the run-owned MaaS CA ConfigMap into Authorino at
// /etc/ssl/certs/maas-api-serving-ca.crt (run.sh:796).
const authorinoCAVolumePatch = `{"spec":{"volumes":{"defaultMode":420,"items":[{"name":"maas-api-serving-ca","mountPath":"/etc/ssl/certs","configMaps":["authorino-maas-api-ca"],"items":[{"key":"ca.crt","path":"maas-api-serving-ca.crt"}]}]}}}`

// applyTranslationSchema applies the MaaS apiTranslation CRD overlay when the
// selected MaaS controller predates the field (run.sh apply_maas_api_translation_schema).
// Default path (MAAS_API_TRANSLATION_SCHEMA_REPO unset): skip. The full provenance
// comparison (git show + normalized-diff vs main) is a later refinement.
func (p *MaaSPlatform) applyTranslationSchema(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
	schemaFile := os.Getenv("MAAS_API_TRANSLATION_SCHEMA_FILE")
	if schemaFile == "" {
		return ctx, nil // not applied; selected controller already has spec.apiTranslation
	}
	return funcs.Manifest(schemaFile)(ctx, cfg)
}

// wireAuthorinoCA makes Authorino trust the run-owned MaaS API CA (run.sh:787-824):
// wait for the Authorino CR, merge-patch the CA volume mount, wait for the operator
// to roll the Deployment, then point Go's TLS trust env at the mounted CA.
func (p *MaaSPlatform) wireAuthorinoCA(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
	ctx, err := funcs.WaitForResource(authorinoGVK, "kuadrant-system", "authorino", 2*time.Minute)(ctx, cfg)
	if err != nil {
		return ctx, fmt.Errorf("kuadrant did not create the Authorino CR: %w", err)
	}

	r := cfg.Client().Resources()
	au := &unstructured.Unstructured{}
	au.SetGroupVersionKind(authorinoGVK)
	if err := r.Get(ctx, "authorino", "kuadrant-system", au); err != nil {
		return ctx, fmt.Errorf("get Authorino CR: %w", err)
	}
	if err := r.Patch(ctx, au, k8s.Patch{PatchType: types.MergePatchType, Data: []byte(authorinoCAVolumePatch)}); err != nil {
		return ctx, fmt.Errorf("patch Authorino CA volume: %w", err)
	}

	// The operator re-rolls the Deployment to add the projected CA volume.
	// NOTE: run.sh gates on the exact mount being present before continuing; here we
	// wait for Available then set env — tighten to the mount check when runnable.
	steps := []env.Func{
		funcs.WaitForDeploymentAvailable("kuadrant-system", "authorino", 3*time.Minute),
		funcs.SetDeploymentEnv("kuadrant-system", "authorino", "", map[string]string{
			"SSL_CERT_FILE":      "/etc/ssl/certs/maas-api-serving-ca.crt",
			"REQUESTS_CA_BUNDLE": "/etc/ssl/certs/maas-api-serving-ca.crt",
		}),
		funcs.WaitForDeploymentAvailable("kuadrant-system", "authorino", 3*time.Minute),
	}
	for _, step := range steps {
		if ctx, err = step(ctx, cfg); err != nil {
			return ctx, err
		}
	}
	return ctx, nil
}

// maasTenantConfigGVK is the MaaS-owned per-tenant config the handoff annotates.
var maasTenantConfigGVK = schema.GroupVersionKind{
	Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaasTenantConfig",
}

// maasSubscriptionGVK is the tenant subscription whose phase gates readiness.
var maasSubscriptionGVK = schema.GroupVersionKind{
	Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaaSSubscription",
}

const (
	payloadTypeAnnotation   = "maas.opendatahub.io/payload-processing-type"
	payloadStatusAnnotation = "maas.opendatahub.io/payload-processing-status"
)

// katanTLS describes one run-owned HTTPS provider (katan) serving cert.
type katanTLS struct {
	secret  string
	service string
	aliases []string
}

// provisionTenants applies the AITenant fixtures, drives the MaaS/Praxis payload
// handoff, mints the katan provider serving certs + provider-trust ConfigMap from the
// run-owned CA, and applies the model/provider/route fixtures (run.sh:953-1157).
func (p *MaaSPlatform) provisionTenants(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
	if p.ca == nil {
		return ctx, fmt.Errorf("run-owned CA not provisioned before tenant fixtures")
	}
	var err error
	step := func(fn env.Func) bool {
		ctx, err = fn(ctx, cfg)
		return err == nil
	}

	// AITenants first, so MaaS materializes each MaasTenantConfig before the handoff.
	for _, f := range []string{"20-fixtures.yaml", "21-fixtures-tenant-b.yaml", "42-transition-fixtures.yaml"} {
		if !step(funcs.ApplyManifestOnlyKinds(filepath.Join(p.manifestsDir, f), "AITenant")) {
			return ctx, err
		}
	}

	// Praxis opt-in: wait for the MaaS-owned config, select the praxis path, wait steady.
	for _, ns := range []string{"models-as-a-service", "ai-tenant-tenant-b"} {
		if !step(funcs.WaitForResource(maasTenantConfigGVK, ns, "default-tenant", 3*time.Minute)) ||
			!step(funcs.AnnotateResource(maasTenantConfigGVK, ns, "default-tenant", map[string]string{payloadTypeAnnotation: "praxis"})) ||
			!step(funcs.WaitForAnnotation(maasTenantConfigGVK, ns, "default-tenant", payloadStatusAnnotation, "steady", 3*time.Minute)) {
			return ctx, err
		}
	}
	// The transition tenant stays on the MaaS-owned IPP path.
	if !step(funcs.WaitForResource(maasTenantConfigGVK, "ai-tenant-transition", "default-tenant", 3*time.Minute)) ||
		!step(funcs.AnnotateResource(maasTenantConfigGVK, "ai-tenant-transition", "default-tenant", map[string]string{payloadTypeAnnotation: "ipp"})) {
		return ctx, err
	}

	// Run-owned katan provider serving certs, signed by the run CA (run.sh:1106-1114).
	for _, k := range []katanTLS{
		{"provider-a-tls", "provider-a", []string{"provider-a-ext"}},
		{"provider-b-tls", "provider-b", []string{"provider-b-ext"}},
		{"provider-proof-a-tls", "provider-proof-a-ext", nil},
		{"provider-proof-b-tls", "provider-proof-b-ext", nil},
		{"provider-proof-c-tls", "provider-proof-c-ext", nil},
		{"provider-translation-tls", "provider-translation-ext", []string{"provider-translation-openai-ext", "provider-translation-bedrock-ext"}},
		{"provider-a-tenant-b-tls", "provider-a-tenant-b", nil},
		{"provider-b-tenant-b-tls", "provider-b-tenant-b", nil},
		{"provider-transition-tls", "provider-a-legacy", nil},
	} {
		sans := maasServiceSANs(k.service)
		for _, alias := range k.aliases {
			sans = append(sans, maasServiceSANs(alias)...)
		}
		cert, key, signErr := p.ca.SignServingCert(k.service+".maas-system.svc", sans, certValidity)
		if signErr != nil {
			return ctx, fmt.Errorf("katan cert %s: %w", k.secret, signErr)
		}
		if !step(funcs.CreateTLSSecret(gatewayNamespace, k.secret, cert, key)) {
			return ctx, err
		}
	}

	// CA trust bundles the gateway envoy needs. provider-ca (key ca.crt) backs the
	// provider DestinationRules; service-ca (key service-ca.crt) is the OpenShift
	// service-ca stand-in the ext-proc clusters reference. Both are the run CA; the
	// gateway mounts happen after the final gateway apply so istio doesn't revert them.
	if !step(funcs.CreateConfigMap(gatewayNamespace, "external-model-e2e-provider-ca",
		map[string]string{"ca.crt": string(p.ca.CertPEM)})) ||
		!step(funcs.CreateConfigMap(gatewayNamespace, "gateway-service-ca",
			map[string]string{"service-ca.crt": string(p.ca.CertPEM)})) {
		return ctx, err
	}

	// Glob-apply the remaining manifests (backends, route policies, shared infra),
	// skipping the ones applied explicitly in order below (run.sh:1174-1188). The
	// katan backends' pinned public digest is rewritten to the loaded local tag.
	if ctx, err = p.applyFixtureGlob(ctx, cfg); err != nil {
		return ctx, err
	}

	// Tenant-A ExternalProviders from test/external-model (run.sh:1190-1196). These
	// are not in manifests/; they are rendered from a template with the run endpoints.
	if ctx, err = p.applyExternalModelProviders(ctx, cfg); err != nil {
		return ctx, err
	}

	// MaaS model refs/subscriptions, then the ExternalModels (non-AITenant) — in this
	// order so the disabled IPP writer never observes the models first (run.sh:1257-1275).
	for _, f := range []string{"40-maas-fixtures.yaml", "41-maas-fixtures-tenant-b.yaml"} {
		if !step(funcs.Manifest(filepath.Join(p.manifestsDir, f))) {
			return ctx, err
		}
	}
	for _, f := range []string{"42-transition-fixtures.yaml", "20-fixtures.yaml", "21-fixtures-tenant-b.yaml"} {
		if !step(funcs.ApplyManifestExcludingKinds(filepath.Join(p.manifestsDir, f), "AITenant")) {
			return ctx, err
		}
	}

	// Service-ca stand-in: the fixtures just made the controller render the annotated
	// ExtProc Services; mint their serving-cert secrets so the pods can start.
	if ctx, err = p.signExtProcServingCerts(ctx, cfg); err != nil {
		return ctx, err
	}

	// Mount the CA bundles into the istio gateway Deployments so envoy can verify the
	// ext-proc (service-ca path) and provider serving certs. Done last — after the
	// final gateway apply — so istio does not reconcile the mounts away. Each mount
	// rolls the gateway; the subscription-ready gate below waits for it to settle.
	deploymentGVK := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	for _, gw := range []string{"maas-default-gateway-istio", "maas-tenant-b-gateway-istio", "maas-transition-gateway-istio"} {
		if !step(funcs.WaitForResource(deploymentGVK, gatewayNamespace, gw, 3*time.Minute)) ||
			!step(funcs.MountConfigMapVolume(gatewayNamespace, gw, "istio-proxy", "gateway-service-ca", "/var/run/secrets/kubernetes.io/serviceaccount")) ||
			!step(funcs.MountConfigMapVolume(gatewayNamespace, gw, "istio-proxy", "external-model-e2e-provider-ca", "/etc/external-model-e2e/provider-ca")) {
			return ctx, err
		}
	}

	// Readiness gate: wait for the tenant subscriptions to reach Active (their models
	// healthy) before trials — otherwise a key mint 403s with "model unhealthy".
	for _, ns := range []string{"models-as-a-service", "ai-tenant-tenant-b"} {
		if !step(funcs.WaitForPhase(maasSubscriptionGVK, ns, "kind-e2e-subscription", "Active", 5*time.Minute)) {
			return ctx, err
		}
	}
	return ctx, nil
}

// fixtureGlobSkip are the manifests applied explicitly (in order) rather than via the
// glob (run.sh:1176-1177).
var fixtureGlobSkip = map[string]bool{
	"20-fixtures.yaml": true, "21-fixtures-tenant-b.yaml": true,
	"31-maas-api-kind-rbac.yaml": true, "32-maas-api-kind-network.yaml": true,
	"40-maas-fixtures.yaml": true, "41-maas-fixtures-tenant-b.yaml": true,
	"42-transition-fixtures.yaml": true, "provider-proof-external-providers.yaml": true,
	"provider-selection-proof.yaml": true, "shared-translation-backend.yaml": true,
	"shared-translation-routes.yaml": true, "api-translation-provider-aware-backend.yaml": true,
	"45-real-openai-policies.yaml": true, "60-client-kind-patch.yaml": true,
}

// katanIndexDigest is the pinned public katan index digest in 00-backends.yaml; it is
// rewritten to the loaded local tag (run.sh:1184).
const katanIndexDigest = "ghcr.io/nerdalert/llm-katan@sha256:11379a1ec2fd69dc121eada6c544eb423a7c074414507dc1d474f4abba9df75a"

// applyFixtureGlob applies every manifests/*.yaml except fixtureGlobSkip, rewriting
// the katan backend digest to the loaded local tag.
func (p *MaaSPlatform) applyFixtureGlob(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
	entries, err := filepath.Glob(filepath.Join(p.manifestsDir, "*.yaml"))
	if err != nil {
		return ctx, err
	}
	for _, path := range entries {
		base := filepath.Base(path)
		if fixtureGlobSkip[base] {
			continue
		}
		if base == "00-backends.yaml" {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return ctx, readErr
			}
			rewritten := strings.ReplaceAll(string(data), katanIndexDigest, p.images.katan)
			if ctx, err = funcs.ApplyYAMLBytes([]byte(rewritten))(ctx, cfg); err != nil {
				return ctx, err
			}
			continue
		}
		if ctx, err = funcs.Manifest(path)(ctx, cfg); err != nil {
			return ctx, err
		}
	}
	return ctx, nil
}

// applyExternalModelProviders renders test/external-model/providers.yaml.tmpl with the
// run's endpoints (envsubst-equivalent) and applies tenant-A's ExternalProviders.
func (p *MaaSPlatform) applyExternalModelProviders(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
	tmplPath := filepath.Join(p.aigcRoot, "test", "external-model", "providers.yaml.tmpl")
	data, err := os.ReadFile(tmplPath)
	if err != nil {
		return ctx, fmt.Errorf("read external-model providers template: %w", err)
	}
	rendered := os.Expand(string(data), func(v string) string {
		switch v {
		case "EXTERNAL_MODEL_NAMESPACE":
			return "models-as-a-service"
		case "EXTERNAL_MODEL_RUN_ID":
			return p.cluster
		case "EXTERNAL_MODEL_PROVIDER_A_ENDPOINT":
			return "provider-a-ext.maas-system.svc.cluster.local"
		case "EXTERNAL_MODEL_PROVIDER_B_ENDPOINT":
			return "provider-b-ext.maas-system.svc.cluster.local"
		case "EXTERNAL_MODEL_PROVIDER_CA_PATH":
			return "/etc/external-model-e2e/provider-ca/ca.crt"
		default:
			return "${" + v + "}" // leave unknown refs intact (envsubst with explicit vars)
		}
	})
	return funcs.ApplyYAMLBytes([]byte(rendered))(ctx, cfg)
}

// aigcControllerArgs is the ai-gateway-controller arg array run.sh patches in
// (run.sh:927-942), with the ext-proc image appended.
func aigcControllerArgs(extprocImage string) []string {
	return []string{
		"--leader-elect",
		"--health-probe-bind-address=:8081",
		"--gateway-name=maas-default-gateway",
		"--gateway-namespace=maas-system",
		"--known-cluster=provider-provider-a",
		"--known-cluster=provider-provider-b",
		"--known-cluster=provider-provider-proof-a",
		"--known-cluster=provider-provider-proof-b",
		"--known-cluster=provider-provider-proof-c",
		"--known-cluster=provider-api-translation-openai",
		"--known-cluster=provider-api-translation-anthropic",
		"--known-cluster=provider-api-translation-bedrock-mantle",
		"--known-cluster=provider-transition-provider",
		"--image=" + extprocImage,
	}
}

// provisionAIGatewayController deploys this repo's controller: its CRDs, a run-owned
// metrics TLS cert (kind has no service-ca), the self overlay (minus ServiceMonitor),
// and the controller Deployment pinned to the loaded image with the ext-proc image +
// known-cluster args (run.sh:838-944).
func (p *MaaSPlatform) provisionAIGatewayController(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
	crds, err := funcs.RenderKustomize(filepath.Join(p.aigcRoot, "config/crd"))
	if err != nil {
		return ctx, err
	}
	self, err := funcs.RenderKustomize(filepath.Join(p.aigcRoot, "config/self/default"))
	if err != nil {
		return ctx, err
	}
	metricsCert, metricsKey, err := funcs.GenerateSelfSignedServingCert(
		"ai-gateway-controller-metrics.opendatahub.svc",
		[]string{
			"ai-gateway-controller-metrics",
			"ai-gateway-controller-metrics.opendatahub.svc",
			"ai-gateway-controller-metrics.opendatahub.svc.cluster.local",
		}, certValidity)
	if err != nil {
		return ctx, fmt.Errorf("aigc metrics cert: %w", err)
	}

	steps := []env.Func{
		funcs.ApplyYAMLBytes(crds),
		funcs.CreateTLSSecret("opendatahub", "ai-gateway-controller-metrics-tls", metricsCert, metricsKey),
		funcs.ApplyYAMLBytesExcludingKinds(self, "ServiceMonitor"),
		funcs.SetDeploymentImage("opendatahub", "ai-gateway-controller", "manager", p.images.aiController),
		funcs.SetDeploymentImagePullPolicy("opendatahub", "ai-gateway-controller", "manager", corev1.PullNever),
		funcs.SetDeploymentArgs("opendatahub", "ai-gateway-controller", "manager", aigcControllerArgs(p.images.extproc)),
	}
	for _, step := range steps {
		if ctx, err = step(ctx, cfg); err != nil {
			return ctx, err
		}
	}
	return ctx, nil
}

// buildRecorder compiles the provider-recorder binary (its Dockerfile is FROM
// scratch and copies a prebuilt binary) then builds its image (run.sh:566-570).
func (p *MaaSPlatform) buildRecorder(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
	dir := filepath.Join(filepath.Dir(p.runScript), "provider-recorder")
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o",
		filepath.Join(dir, "provider-recorder"), filepath.Join(dir, "main.go"))
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if out, err := cmd.CombinedOutput(); err != nil {
		return ctx, fmt.Errorf("build provider-recorder binary: %w\n%s", err, out)
	}
	return funcs.BuildImageFrom(dir, filepath.Join(dir, "Dockerfile"), p.images.providerRecorder)(ctx, cfg)
}

// provisionRegistry stands up the run-owned local registry, wires the cluster's
// containerd to it, pushes the maas-api image, and sets maasAPIRuntimeImage to the
// registry ref — so the MaaS controller's imagePullPolicy:Always API Deployment
// resolves the source-built image (run.sh create_run_registry). Requires the cluster
// to already exist (native mode: run with -create-cluster=true).
func (p *MaaSPlatform) provisionRegistry(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
	reg := funcs.NewLocalRegistry(p.cluster+"-registry", p.cluster+"-registry-net")
	if err := reg.Start(ctx); err != nil {
		return ctx, err
	}
	if err := reg.WireCluster(ctx, p.cluster); err != nil {
		return ctx, err
	}
	ref := reg.Ref("maas-api:dev")
	if err := reg.Push(ctx, p.images.maasAPI, ref); err != nil {
		return ctx, err
	}
	p.maasAPIRuntimeImage = ref
	return ctx, nil
}

// controllerArgs is the exact maas-controller arg array run.sh patches in (run.sh:731).
var controllerArgs = []string{
	"--leader-elect",
	"--health-probe-bind-address=:8081",
	"--metrics-bind-address=:8080",
	"--metrics-secure=false",
	"--controller-namespace=maas-system",
	"--gateway-name=maas-default-gateway",
	"--gateway-namespace=maas-system",
	"--infra-namespace=AUTO",
	"--maas-subscription-namespace=models-as-a-service",
	"--aitenant-namespace=ai-tenants",
	"--monitoring-namespace=opendatahub",
	"--enable-tenant-namespace-discovery",
	"--metadata-cache-ttl=60",
	"--authz-cache-ttl=60",
	"--log-format=zap",
}

// provisionMaaS renders the MaaS controller bundle with kind image/namespace
// transforms, applies it (minus the kinds kind can't honor), patches the controller
// deployment to run the source-built binary, and installs the maas-api RBAC + kind
// network fixtures (run.sh:719-736, 826-828). The local-registry runtime image, the
// translation-schema overlay and the Authorino CA wiring are phase-group 4b.
func (p *MaaSPlatform) provisionMaaS(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
	rendered, err := funcs.RenderKustomize(filepath.Join(p.maasControllerRepo, "deployment/base/maas-controller/default"))
	if err != nil {
		return ctx, err
	}
	rbac, err := funcs.RenderKustomize(filepath.Join(p.maasControllerRepo, "deployment/base/maas-api/rbac"))
	if err != nil {
		return ctx, err
	}

	steps := []env.Func{
		// controller bundle: transform then apply, dropping the OpenShift-only kinds.
		funcs.ApplyYAMLBytesExcludingKinds(p.transformControllerBundle(rendered),
			"ServiceMonitor", "ValidatingWebhookConfiguration"),
		// run the source-built binary directly against the run-owned config (run.sh:726-731).
		funcs.SetDeploymentEnv(gatewayNamespace, "maas-controller", "",
			map[string]string{"RELATED_IMAGE_ODH_MAAS_API_IMAGE": p.maasAPIRuntimeImage}),
		funcs.SetDeploymentImagePullPolicy(gatewayNamespace, "maas-controller", "", corev1.PullNever),
		funcs.SetDeploymentCommand(gatewayNamespace, "maas-controller", "", []string{"/manager"}),
		funcs.SetDeploymentArgs(gatewayNamespace, "maas-controller", "", controllerArgs),
		funcs.RestartDeployment(gatewayNamespace, "maas-controller"),
		// maas-api RBAC + kind network fixtures.
		funcs.ApplyYAMLBytes([]byte(strings.ReplaceAll(string(rbac), "namespace: opendatahub", "namespace: maas-system"))),
		funcs.Manifest(filepath.Join(p.manifestsDir, "31-maas-api-kind-rbac.yaml")),
		funcs.Manifest(filepath.Join(p.manifestsDir, "32-maas-api-kind-network.yaml")),
	}
	for _, step := range steps {
		if ctx, err = step(ctx, cfg); err != nil {
			return ctx, err
		}
	}
	return ctx, nil
}

// transformControllerBundle applies run.sh's image/namespace substitutions
// (run.sh:719) to the rendered MaaS controller bundle.
func (p *MaaSPlatform) transformControllerBundle(data []byte) []byte {
	s := string(data)
	for _, r := range [][2]string{
		{"quay.io/opendatahub/maas-api:latest", p.maasAPIRuntimeImage},
		{"quay.io/opendatahub/maas-api:odh-stable", p.maasAPIRuntimeImage},
		{"quay.io/opendatahub/maas-controller:latest", p.images.maasController},
		{"quay.io/opendatahub/maas-controller:odh-stable", p.images.maasController},
		{"openshift-ingress", "maas-system"},
		{"namespace: opendatahub", "namespace: maas-system"},
		{"namespace: system", "namespace: maas-system"},
	} {
		s = strings.ReplaceAll(s, r[0], r[1])
	}
	return []byte(s)
}

// signExtProcServingCerts is the service-ca stand-in for the ExtProc data plane: the
// controller annotates each ExtProc Service with a serving-cert secret name, and this
// mints those secrets from the run-owned CA (kind has no service-ca; run.sh never
// stands it in). It scans the gateway namespace (pre/post-processing ExtProcs) and
// each tenant namespace (external-model ExtProc), polling so controller-created
// Services are covered. Uses the generic dockyard helper — no product-specific names.
func (p *MaaSPlatform) signExtProcServingCerts(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
	if p.ca == nil {
		return ctx, fmt.Errorf("run-owned CA not provisioned before ext-proc certs")
	}
	var err error
	for _, ns := range []string{gatewayNamespace, "models-as-a-service", "ai-tenant-tenant-b"} {
		if ctx, err = funcs.SignAnnotatedServiceCerts(p.ca, ns, certValidity, 2*time.Minute)(ctx, cfg); err != nil {
			return ctx, err
		}
	}
	return ctx, nil
}

// ownerRefGatewayService sets a non-controller ownerReference from a Gateway onto
// its auto-provisioned same-named Service (run.sh:677-682), so the Service is
// cleaned up with the Gateway. Waits for the Service to be provisioned first.
func (p *MaaSPlatform) ownerRefGatewayService(name string) env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		r := cfg.Client().Resources()
		_ = clientgoscheme.AddToScheme(r.GetScheme())

		gw := &unstructured.Unstructured{}
		gw.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway",
		})
		if err := r.Get(ctx, name, gatewayNamespace, gw); err != nil {
			return ctx, fmt.Errorf("get Gateway %s/%s: %w", gatewayNamespace, name, err)
		}

		var svc corev1.Service
		if err := wait.For(func(ctx context.Context) (bool, error) {
			if e := r.Get(ctx, name, gatewayNamespace, &svc); e != nil {
				if apierrors.IsNotFound(e) {
					return false, nil
				}
				return false, e
			}
			return true, nil
		}, wait.WithContext(ctx), wait.WithTimeout(2*time.Minute), wait.WithInterval(3*time.Second)); err != nil {
			return ctx, fmt.Errorf("Service %s/%s not provisioned: %w", gatewayNamespace, name, err)
		}

		no := false
		svc.OwnerReferences = []metav1.OwnerReference{{
			APIVersion:         "gateway.networking.k8s.io/v1",
			Kind:               "Gateway",
			Name:               name,
			UID:                gw.GetUID(),
			Controller:         &no,
			BlockOwnerDeletion: &no,
		}}
		if err := r.Update(ctx, &svc); err != nil {
			return ctx, fmt.Errorf("set owner on Service %s/%s: %w", gatewayNamespace, name, err)
		}
		return ctx, nil
	}
}

// AddTypesToScheme registers the inference + aigateway CRD types so Features can use
// typed clients against the provisioned cluster.
func (p *MaaSPlatform) AddTypesToScheme() env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		s := cfg.Client().Resources().GetScheme()
		if err := inferencev1alpha1.AddToScheme(s); err != nil {
			return ctx, err
		}
		if err := aigatewayv1alpha1.AddToScheme(s); err != nil {
			return ctx, err
		}
		_ = clientgoscheme.AddToScheme(s)
		return ctx, nil
	}
}

// Teardown runs run.sh --destroy (ownership-checked in run.sh). Registered on the
// suite's Finish by TestMain.
func (p *MaaSPlatform) Teardown(ctx context.Context, _ *envconf.Config) (context.Context, error) {
	return ctx, p.run(ctx, "--destroy")
}

// run invokes run.sh with one action arg, inheriting the environment and adding the
// cluster/evidence variables run.sh reads.
func (p *MaaSPlatform) run(ctx context.Context, action string) error {
	cmd := exec.CommandContext(ctx, p.runScript, action)
	cmd.Dir = filepath.Dir(p.runScript)
	cmd.Env = append(os.Environ(), "LOCAL_ENV_CLUSTER="+p.cluster)
	if p.evidenceRoot != "" {
		cmd.Env = append(cmd.Env, "LOCAL_ENV_EVIDENCE_ROOT="+p.evidenceRoot)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run.sh %s (cluster %s): %w", action, p.cluster, err)
	}
	return nil
}
