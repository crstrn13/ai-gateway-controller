package funcs

import (
	"time"

	"sigs.k8s.io/e2e-framework/pkg/env"
)

// Deps builds the ordered dependency stack a product installs before its tests
// run. Each With* appends one install step from the generic installers in this
// package; Build returns them in call order. This is the builder the Product's
// InstallDeps composes — the generic layer owns the building blocks, the product
// owns the order (e.g. the GIE CRD must land before istio):
//
//	funcs.NewDeps().
//	    WithMetalLB(funcs.MetalLBVersion(v)).
//	    WithGatewayAPI(funcs.GatewayAPIVersion(v)).
//	    WithCRDs("manifests/gie").
//	    WithIstio(funcs.IstioInferenceExtension(true)).
//	    WithImage(clusterName, image, isKind).
//	    WithManifests("manifests/backends", "*.yaml").
//	    WithDeploymentAvailable("llm", "internal-model", 3*time.Minute).
//	    Build()
type Deps struct{ fns []env.Func }

// NewDeps starts an empty dependency stack.
func NewDeps() *Deps { return &Deps{} }

// With appends an arbitrary env.Func, for a step with no dedicated builder method.
func (d *Deps) With(fn env.Func) *Deps {
	d.fns = append(d.fns, fn)
	return d
}

// WithMetalLB appends the metallb install (LoadBalancer IPs for the gateway).
func (d *Deps) WithMetalLB(opts ...MetalLBOption) *Deps { return d.With(InstallMetalLB(opts...)) }

// WithGatewayAPI appends the upstream Gateway API CRD install.
func (d *Deps) WithGatewayAPI(opts ...GatewayAPIOption) *Deps {
	return d.With(InstallGatewayAPI(opts...))
}

// WithIstio appends the istio install.
func (d *Deps) WithIstio(opts ...IstioOption) *Deps { return d.With(InstallIstio(opts...)) }

// WithCertManager appends the cert-manager install (TLS issuance for kind).
func (d *Deps) WithCertManager(opts ...CertManagerOption) *Deps {
	return d.With(InstallCertManager(opts...))
}

// WithKuadrant appends the kuadrant-operator install (the ext-proc filters anchor
// to the WasmPlugin Kuadrant attaches to the gateway).
func (d *Deps) WithKuadrant(opts ...KuadrantOption) *Deps {
	return d.With(InstallKuadrant(opts...))
}

// WithKubectlApply appends a retrying `kubectl apply -f path` step.
func (d *Deps) WithKubectlApply(path string) *Deps { return d.With(KubectlApply(path)) }

// WithCABundle appends a step that publishes a cert-manager CA secret's ca.crt as
// a ConfigMap (key service-ca.crt) for the gateway to mount.
func (d *Deps) WithCABundle(caSecretNS, caSecretName, targetNS, targetCM string) *Deps {
	return d.With(PublishCABundle(caSecretNS, caSecretName, targetNS, targetCM))
}

// WithCRDs appends a CRD-apply-and-wait-Established step for the given dirs.
func (d *Deps) WithCRDs(dirs ...string) *Deps { return d.With(InstallCRDs(dirs...)) }

// WithImage appends a kind image-load step (no-op when the cluster isn't kind).
func (d *Deps) WithImage(clusterName, image string, isKind bool) *Deps {
	return d.With(LoadImage(clusterName, image, isKind))
}

// WithBuiltImage appends a docker build of contextDir, then loads the resulting
// tag into the kind cluster — for sim backends the harness builds from source.
func (d *Deps) WithBuiltImage(contextDir, tag, clusterName string, isKind bool) *Deps {
	return d.With(BuildImage(contextDir, tag)).With(LoadImage(clusterName, tag, isKind))
}

// WithManifests appends a suite-level manifest-apply step (shared infra).
func (d *Deps) WithManifests(dir, pattern string) *Deps { return d.With(ApplyManifests(dir, pattern)) }

// WithDeploymentAvailable appends a wait-for-Deployment-Available step.
func (d *Deps) WithDeploymentAvailable(namespace, name string, timeout time.Duration) *Deps {
	return d.With(WaitForDeploymentAvailable(namespace, name, timeout))
}

// Build returns the accumulated steps in call order.
func (d *Deps) Build() []env.Func { return d.fns }
