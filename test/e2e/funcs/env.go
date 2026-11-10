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
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	aigatewayv1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/aigateway/v1alpha1"
	inferencev1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
)

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

// AddTypesToScheme registers the controller's CRD types on the suite client's
// scheme, so typed Create/Get work for inference + aigateway kinds.
func AddTypesToScheme() env.Func {
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

// InstallMetalLB installs metallb and an IPAddressPool carved from the kind
// docker network, so the gateway Service gets a host-routable LB IP
// (the pool is <ipv4-subnet-base>.200-.250; tests resolve the gateway's assigned
// IP from its Service, so the exact base doesn't need to be known ahead of time).
func InstallMetalLB(version string) env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		url := fmt.Sprintf("https://raw.githubusercontent.com/metallb/metallb/%s/config/manifests/metallb-native.yaml", version)
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
func InstallGatewayAPI(version string) env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		url := fmt.Sprintf("https://github.com/kubernetes-sigs/gateway-api/releases/download/%s/standard-install.yaml", version)
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

// InstallIstio installs istio (minimal + inference extension). istioctl is a
// sanctioned shell-out — there is no pure-Go install path.
func InstallIstio() env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		if err := sh(ctx, cfg, "istioctl", "install", "-y",
			"--set", "profile=minimal",
			"--set", "values.pilot.env.ENABLE_GATEWAY_API_INFERENCE_EXTENSION=true",
		); err != nil {
			return ctx, err
		}
		return ctx, sh(ctx, cfg, "kubectl", "rollout", "status", "deployment/istiod", "-n", "istio-system", "--timeout=180s")
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
