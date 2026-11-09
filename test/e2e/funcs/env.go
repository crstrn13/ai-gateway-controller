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

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	aigatewayv1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/aigateway/v1alpha1"
	inferencev1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
)

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
