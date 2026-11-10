package funcs

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/opendatahub-io/ai-gateway-controller/test/e2e/gateway"
)

// DefaultPollInterval is the poll cadence for the Within helpers.
const DefaultPollInterval = 3 * time.Second

// AllOf runs several fixtures as one step (Crossplane's combinator).
func AllOf(fns ...features.Func) features.Func {
	return func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		for _, fn := range fns {
			ctx = fn(ctx, t, c)
		}
		return ctx
	}
}

// ApplyResources applies every manifest matching pattern under dir (relative to
// the test package's working directory — i.e. test/e2e) via the decoder.
func ApplyResources(dir, pattern string) features.Func {
	return func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Helper()
		if err := decoder.DecodeEachFile(ctx, os.DirFS(dir), pattern,
			decoder.CreateHandler(c.Client().Resources())); err != nil {
			t.Fatalf("apply %s/%s: %v", dir, pattern, err)
		}
		return ctx
	}
}

// DeleteResources deletes every manifest matching pattern under dir (best-effort).
func DeleteResources(dir, pattern string) features.Func {
	return func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		_ = decoder.DecodeEachFile(ctx, os.DirFS(dir), pattern,
			decoder.DeleteHandler(c.Client().Resources()))
		return ctx
	}
}

// Create applies typed/unstructured objects built in Go (code fixtures).
func Create(objs ...k8s.Object) features.Func {
	return func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Helper()
		r := c.Client().Resources()
		for _, o := range objs {
			if err := r.Create(ctx, o); err != nil {
				t.Fatalf("create %T %s/%s: %v", o, o.GetNamespace(), o.GetName(), err)
			}
		}
		return ctx
	}
}

// Delete removes Go-built objects (best-effort), the counterpart to Create.
func Delete(objs ...k8s.Object) features.Func {
	return func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		r := c.Client().Resources()
		for _, o := range objs {
			_ = r.Delete(ctx, o)
		}
		return ctx
	}
}

// ResourceCreatedWithin waits until obj can be fetched (created by a controller).
func ResourceCreatedWithin(d time.Duration, obj k8s.Object) features.Func {
	return func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Helper()
		r := c.Client().Resources()
		err := wait.For(func(context.Context) (bool, error) {
			e := r.Get(ctx, obj.GetName(), obj.GetNamespace(), obj)
			if apierrors.IsNotFound(e) {
				return false, nil
			}
			return e == nil, e
		}, wait.WithTimeout(d), wait.WithInterval(DefaultPollInterval))
		if err != nil {
			t.Fatalf("resource %s/%s not created within %s: %v", obj.GetNamespace(), obj.GetName(), d, err)
		}
		return ctx
	}
}

// DeploymentBecomesAvailableWithin waits for a Deployment's Available condition.
func DeploymentBecomesAvailableWithin(d time.Duration, namespace, name string) features.Func {
	return func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Helper()
		dep := &appsv1.Deployment{}
		dep.SetName(name)
		dep.SetNamespace(namespace)
		err := wait.For(
			conditions.New(c.Client().Resources()).DeploymentConditionMatch(dep, appsv1.DeploymentAvailable, corev1.ConditionTrue),
			wait.WithTimeout(d), wait.WithInterval(DefaultPollInterval),
		)
		if err != nil {
			t.Fatalf("deployment %s/%s not Available within %s: %v", namespace, name, d, err)
		}
		return ctx
	}
}

// GatewayClient resolves the gateway's assigned LB IP and returns a gateway SDK
// client for it. Errors if the Service has no IP yet — call it inside an Assess,
// where the gateway is already up. Driving requests + asserting is the Assess's
// own inline job (convention), so there is no chat/assertion helper here.
func GatewayClient(ctx context.Context, c *envconf.Config, gatewayName, ns string, opts ...gateway.Option) (*gateway.Client, error) {
	ip, err := gatewayLBIP(ctx, c.Client().Resources(), gatewayName, ns)
	if err != nil {
		return nil, err
	}
	if ip == "" {
		return nil, fmt.Errorf("gateway %s/%s has no LoadBalancer IP yet", ns, gatewayName)
	}
	return gateway.New("http://"+ip, opts...), nil
}

// gatewayLBIP returns the external IP metallb assigned to the istio gateway's
// auto-provisioned Service (named <gatewayName>-istio).
func gatewayLBIP(ctx context.Context, r *resources.Resources, gatewayName, ns string) (string, error) {
	var svc corev1.Service
	if err := r.Get(ctx, gatewayName+"-istio", ns, &svc); err != nil {
		return "", err
	}
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			return ing.IP, nil
		}
	}
	return "", nil
}
