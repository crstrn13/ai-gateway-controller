// Package funcs contains the reusable e2e fixture library, modeled on
// Crossplane's test/e2e/funcs: env.Func builders for environment setup (this
// file) and features.Func builders for per-test actions (feature.go). Logic is
// Go (klient/decoder) rather than shell, except the few dependency installers
// that have no Go path (istioctl, kind, kubectl-from-URL).
package funcs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

// Default versions/settings for the dependency installers. Each installer takes
// functional options; an option left unset falls back to the default here, so a
// caller passing an empty flag value does not clobber the default (see the
// With* option constructors, which ignore empty inputs).
const (
	defaultMetalLBVersion     = "v0.14.9"
	defaultGatewayAPIVersion  = "v1.2.1"
	defaultIstioProfile       = "minimal"
	defaultCertManagerVersion = "v1.15.3"
	defaultKuadrantVersion    = "1.3.1"
)

// kuadrantCR bootstraps Authorino + Limitador once the operator is installed.
const kuadrantCR = `apiVersion: kuadrant.io/v1beta1
kind: Kuadrant
metadata:
  name: kuadrant
  namespace: kuadrant-system
spec: {}
`

// ApplyManifests applies a directory of manifests as a suite-level setup step
// (env.Func) — for shared environment infra (backends, etc.), not per-test CRs.
// Paths are relative to the test package working directory (test/e2e).
func ApplyManifests(dir, pattern string) env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		r, err := resources.New(cfg.Client().RESTConfig())
		if err != nil {
			return ctx, err
		}
		_ = clientgoscheme.AddToScheme(r.GetScheme())
		if err := decoder.ApplyWithManifestDir(ctx, r, dir, pattern, nil); err != nil {
			return ctx, fmt.Errorf("apply manifests %s/%s: %w", dir, pattern, err)
		}
		return ctx, nil
	}
}

// InstallCRDs applies every given CRD directory and waits for each CRD to report
// Established, so the manager's/istio's REST mapper resolves every kind on first
// use (avoids the discovery-race retry storm). Paths are relative to the test
// package working directory.
func InstallCRDs(dirs ...string) env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		r, err := resources.New(cfg.Client().RESTConfig())
		if err != nil {
			return ctx, fmt.Errorf("new resources client: %w", err)
		}
		if err := apiextensionsv1.AddToScheme(r.GetScheme()); err != nil {
			return ctx, fmt.Errorf("add apiextensions scheme: %w", err)
		}
		for _, dir := range dirs {
			if err := decoder.ApplyWithManifestDir(ctx, r, dir, "*.yaml", nil); err != nil {
				return ctx, fmt.Errorf("apply CRDs from %s: %w", dir, err)
			}
		}
		var crds apiextensionsv1.CustomResourceDefinitionList
		if err := r.List(ctx, &crds); err != nil {
			return ctx, fmt.Errorf("list CRDs: %w", err)
		}
		for i := range crds.Items {
			crd := &crds.Items[i]
			err := wait.For(
				conditions.New(r).ResourceMatch(crd, func(obj k8s.Object) bool {
					c, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
					if !ok {
						return false
					}
					for _, cond := range c.Status.Conditions {
						if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
							return true
						}
					}
					return false
				}),
				wait.WithTimeout(60*time.Second), wait.WithInterval(time.Second),
			)
			if err != nil {
				return ctx, fmt.Errorf("CRD %s not Established: %w", crd.Name, err)
			}
		}
		return ctx, nil
	}
}

// WaitForDeploymentAvailable blocks until a Deployment reports Available (env.Func).
func WaitForDeploymentAvailable(namespace, name string, d time.Duration) env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		dep := &appsv1.Deployment{}
		dep.SetName(name)
		dep.SetNamespace(namespace)
		err := wait.For(
			conditions.New(cfg.Client().Resources()).DeploymentConditionMatch(dep, appsv1.DeploymentAvailable, corev1.ConditionTrue),
			wait.WithTimeout(d), wait.WithInterval(3*time.Second),
		)
		if err != nil {
			return ctx, fmt.Errorf("deployment %s/%s not Available: %w", namespace, name, err)
		}
		return ctx, nil
	}
}

// EnvFuncs runs env.Funcs in order, short-circuiting on the first error.
func EnvFuncs(fns ...env.Func) env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		for _, fn := range fns {
			var err error
			if ctx, err = fn(ctx, cfg); err != nil {
				return ctx, err
			}
		}
		return ctx, nil
	}
}

// AsFeaturesFunc adapts an env.Func into a features.Func (fails the test on error).
func AsFeaturesFunc(fn env.Func) features.Func {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		ctx, err := fn(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}
}

// --- dependency installers (functional options) ------------------------------

// MetalLBOption configures InstallMetalLB.
type MetalLBOption func(*metalLBConfig)

type metalLBConfig struct{ version string }

// MetalLBVersion pins the metallb release. Empty is ignored (keeps the default).
func MetalLBVersion(v string) MetalLBOption {
	return func(c *metalLBConfig) {
		if v != "" {
			c.version = v
		}
	}
}

// GatewayAPIOption configures InstallGatewayAPI.
type GatewayAPIOption func(*gatewayAPIConfig)

type gatewayAPIConfig struct{ version string }

// GatewayAPIVersion pins the Gateway API release. Empty is ignored.
func GatewayAPIVersion(v string) GatewayAPIOption {
	return func(c *gatewayAPIConfig) {
		if v != "" {
			c.version = v
		}
	}
}

// IstioOption configures InstallIstio.
type IstioOption func(*istioConfig)

type istioConfig struct {
	profile            string
	inferenceExtension bool
}

// IstioProfile sets the istioctl install profile. Empty is ignored.
func IstioProfile(p string) IstioOption {
	return func(c *istioConfig) {
		if p != "" {
			c.profile = p
		}
	}
}

// IstioInferenceExtension toggles the Gateway API Inference Extension (GIE).
func IstioInferenceExtension(on bool) IstioOption {
	return func(c *istioConfig) { c.inferenceExtension = on }
}

// InstallMetalLB installs metallb and an IPAddressPool carved from the kind
// docker network, so the gateway Service gets a host-routable LB IP
// (the pool is <ipv4-subnet-base>.200-.250; tests resolve the gateway's assigned
// IP from its Service, so the exact base doesn't need to be known ahead of time).
func InstallMetalLB(opts ...MetalLBOption) env.Func {
	o := metalLBConfig{version: defaultMetalLBVersion}
	for _, opt := range opts {
		opt(&o)
	}
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		url := fmt.Sprintf("https://raw.githubusercontent.com/metallb/metallb/%s/config/manifests/metallb-native.yaml", o.version)
		if err := sh(ctx, cfg, "kubectl", "apply", "-f", url); err != nil {
			return ctx, err
		}
		if err := sh(ctx, cfg, "kubectl", "wait", "deployment/controller", "-n", "metallb-system",
			"--for=condition=available", "--timeout=180s"); err != nil {
			return ctx, err
		}
		base, err := kindSubnetBase(ctx)
		if err != nil {
			return ctx, err
		}
		pool := fmt.Sprintf(`apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata: {name: e2e-pool, namespace: metallb-system}
spec: {addresses: ["%s.200-%s.250"]}
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata: {name: e2e-l2, namespace: metallb-system}
`, base, base)
		var lastErr error
		for range 6 { // the validating webhook can lag the controller
			if lastErr = shStdin(ctx, cfg, pool, "kubectl", "apply", "-f", "-"); lastErr == nil {
				return ctx, nil
			}
			time.Sleep(10 * time.Second)
		}
		return ctx, fmt.Errorf("configure metallb pool: %w", lastErr)
	}
}

// InstallGatewayAPI installs the upstream Gateway API CRDs (istio needs them to
// serve Gateway/HTTPRoute, which the suite's manifests are).
func InstallGatewayAPI(opts ...GatewayAPIOption) env.Func {
	o := gatewayAPIConfig{version: defaultGatewayAPIVersion}
	for _, opt := range opts {
		opt(&o)
	}
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		url := fmt.Sprintf("https://github.com/kubernetes-sigs/gateway-api/releases/download/%s/standard-install.yaml", o.version)
		if err := sh(ctx, cfg, "kubectl", "apply", "-f", url); err != nil {
			return ctx, err
		}
		return ctx, sh(ctx, cfg, "kubectl", "wait", "--for=condition=established", "--timeout=60s",
			"crd/gateways.gateway.networking.k8s.io",
			"crd/httproutes.gateway.networking.k8s.io",
			"crd/gatewayclasses.gateway.networking.k8s.io",
		)
	}
}

// InstallIstio installs istio via istioctl (a sanctioned shell-out — there is no
// pure-Go install path). Defaults to the minimal profile with the Gateway API
// Inference Extension on; override with IstioProfile / IstioInferenceExtension.
func InstallIstio(opts ...IstioOption) env.Func {
	o := istioConfig{profile: defaultIstioProfile, inferenceExtension: true}
	for _, opt := range opts {
		opt(&o)
	}
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		args := []string{"install", "-y", "--set", "profile=" + o.profile,
			// Access logs to stdout so request failures (response flags + code
			// details) are visible in the gateway proxy logs during e2e debugging.
			"--set", "meshConfig.accessLogFile=/dev/stdout"}
		if o.inferenceExtension {
			args = append(args, "--set", "values.pilot.env.ENABLE_GATEWAY_API_INFERENCE_EXTENSION=true")
		}
		if err := sh(ctx, cfg, "istioctl", args...); err != nil {
			return ctx, err
		}
		return ctx, sh(ctx, cfg, "kubectl", "rollout", "status", "deployment/istiod", "-n", "istio-system", "--timeout=180s")
	}
}

// BuildImage builds a container image with `docker build -t tag contextDir`
// (Dockerfile at contextDir/Dockerfile). A sanctioned shell-out: building images
// has no pure-Go path. contextDir is relative to the test package working dir.
func BuildImage(contextDir, tag string) env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		return ctx, sh(ctx, cfg, "docker", "build", "-t", tag, contextDir)
	}
}

// CertManagerOption configures InstallCertManager.
type CertManagerOption func(*certManagerConfig)

type certManagerConfig struct{ version string }

// CertManagerVersion pins the cert-manager release. Empty is ignored.
func CertManagerVersion(v string) CertManagerOption {
	return func(c *certManagerConfig) {
		if v != "" {
			c.version = v
		}
	}
}

// InstallCertManager installs cert-manager (the standard kind/k8s way to issue
// TLS) and waits for its CRDs + deployments to be ready, so Issuer/Certificate
// resources can be applied immediately after.
func InstallCertManager(opts ...CertManagerOption) env.Func {
	o := certManagerConfig{version: defaultCertManagerVersion}
	for _, opt := range opts {
		opt(&o)
	}
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		url := fmt.Sprintf("https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml", o.version)
		if err := sh(ctx, cfg, "kubectl", "apply", "-f", url); err != nil {
			return ctx, err
		}
		if err := sh(ctx, cfg, "kubectl", "wait", "--for=condition=established", "--timeout=120s",
			"crd/certificates.cert-manager.io", "crd/issuers.cert-manager.io", "crd/clusterissuers.cert-manager.io"); err != nil {
			return ctx, err
		}
		return ctx, sh(ctx, cfg, "kubectl", "wait", "--for=condition=Available", "deploy", "--all",
			"-n", "cert-manager", "--timeout=180s")
	}
}

// KuadrantOption configures InstallKuadrant.
type KuadrantOption func(*kuadrantConfig)

type kuadrantConfig struct{ version string }

// KuadrantVersion pins the kuadrant-operator chart version. Empty is ignored.
func KuadrantVersion(v string) KuadrantOption {
	return func(c *kuadrantConfig) {
		if v != "" {
			c.version = v
		}
	}
}

// InstallKuadrant installs the kuadrant-operator (Authorino + Limitador) and
// bootstraps it. Kuadrant is what attaches the WasmPlugin to the gateway that the
// ext-proc EnvoyFilters anchor to (INSERT_BEFORE/AFTER the kuadrant wasm filter);
// without it the ext-proc filters are never inserted. Installed after istio, so
// the operator is restarted to pick up istio's gateway provider.
func InstallKuadrant(opts ...KuadrantOption) env.Func {
	o := kuadrantConfig{version: defaultKuadrantVersion}
	for _, opt := range opts {
		opt(&o)
	}
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		if err := sh(ctx, cfg, "helm", "repo", "add", "--force-update", "kuadrant", "https://kuadrant.io/helm-charts/"); err != nil {
			return ctx, err
		}
		_ = sh(ctx, cfg, "helm", "repo", "update", "kuadrant")
		if err := sh(ctx, cfg, "helm", "upgrade", "--install", "kuadrant-operator", "kuadrant/kuadrant-operator",
			"--namespace", "kuadrant-system", "--create-namespace", "--version", o.version, "--wait", "--timeout", "350s"); err != nil {
			return ctx, err
		}
		// Restart so the operator rediscovers istio (installed before this step).
		_ = sh(ctx, cfg, "kubectl", "-n", "kuadrant-system", "rollout", "restart", "deployment/kuadrant-operator-controller-manager")
		if err := sh(ctx, cfg, "kubectl", "-n", "kuadrant-system", "rollout", "status",
			"deployment/kuadrant-operator-controller-manager", "--timeout=120s"); err != nil {
			return ctx, err
		}
		if err := shStdin(ctx, cfg, kuadrantCR, "kubectl", "apply", "-f", "-"); err != nil {
			return ctx, err
		}
		// The operator creates authorino + limitador from the CR; wait for both.
		return ctx, sh(ctx, cfg, "bash", "-c",
			"until kubectl -n kuadrant-system get deploy authorino limitador-limitador >/dev/null 2>&1; do sleep 5; done; "+
				"kubectl -n kuadrant-system wait --for=condition=Available deploy --all --timeout=180s")
	}
}

// KubectlApply applies a manifest path, retrying to ride out the cert-manager
// webhook becoming ready (it can reject the first Issuer/Certificate writes).
// Path is relative to the test package working directory.
func KubectlApply(path string) env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		var lastErr error
		for range 6 {
			if lastErr = sh(ctx, cfg, "kubectl", "apply", "-f", path); lastErr == nil {
				return ctx, nil
			}
			time.Sleep(10 * time.Second)
		}
		return ctx, fmt.Errorf("kubectl apply %s: %w", path, lastErr)
	}
}

// PublishCABundle copies the ca.crt from a cert-manager CA secret into a ConfigMap
// (key service-ca.crt) in another namespace — the CA the gateway mounts to trust
// the ext-proc serving certs. Waits for the CA secret to be issued first.
func PublishCABundle(caSecretNS, caSecretName, targetNS, targetCM string) env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		r := cfg.Client().Resources()
		var secret corev1.Secret
		err := wait.For(func(context.Context) (bool, error) {
			if e := r.Get(ctx, caSecretName, caSecretNS, &secret); e != nil {
				return false, nil
			}
			return len(secret.Data["ca.crt"]) > 0, nil
		}, wait.WithTimeout(120*time.Second), wait.WithInterval(3*time.Second))
		if err != nil {
			return ctx, fmt.Errorf("CA secret %s/%s not issued: %w", caSecretNS, caSecretName, err)
		}
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: targetCM, Namespace: targetNS},
			Data:       map[string]string{"service-ca.crt": string(secret.Data["ca.crt"])},
		}
		if err := r.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctx, fmt.Errorf("publish CA configmap %s/%s: %w", targetNS, targetCM, err)
		}
		return ctx, nil
	}
}

// LoadImage loads the ext-proc image into the kind cluster (no-op when we don't
// own the cluster).
func LoadImage(clusterName, image string, isKind bool) env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		if !isKind {
			return ctx, nil
		}
		return ctx, sh(ctx, cfg, "kind", "load", "docker-image", image, "--name", clusterName)
	}
}

// --- shell plumbing (dep installs only) --------------------------------------

func sh(ctx context.Context, cfg *envconf.Config, name string, args ...string) error {
	return shStdin(ctx, cfg, "", name, args...)
}

func shStdin(ctx context.Context, cfg *envconf.Config, stdin, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+cfg.KubeconfigFile())
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}
	return nil
}

func kindSubnetBase(ctx context.Context) (string, error) {
	// The kind network is dual-stack; list all subnets and pick the IPv4 one
	// (metallb pool must be IPv4 to match the host-routable LB IP).
	out, err := exec.CommandContext(ctx, "docker", "network", "inspect", "kind",
		"-f", "{{range .IPAM.Config}}{{.Subnet}} {{end}}").Output()
	if err != nil {
		return "", fmt.Errorf("docker network inspect kind: %w", err)
	}
	for _, subnet := range strings.Fields(string(out)) {
		ip, _, _ := strings.Cut(subnet, "/")
		if strings.Contains(ip, ".") && !strings.Contains(ip, ":") { // IPv4
			if octets := strings.Split(ip, "."); len(octets) >= 3 {
				return strings.Join(octets[:3], "."), nil
			}
		}
	}
	return "", fmt.Errorf("no IPv4 subnet on the kind network (got %q)", strings.TrimSpace(string(out)))
}
