package funcs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
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

// HTTPPostStatusWithin drives a data-plane request, retrying until the response
// status equals want or the deadline passes (routes/endpoints propagate lazily).
func HTTPPostStatusWithin(d time.Duration, url, body string, headers map[string]string, want int) features.Func {
	return func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Helper()
		var last int
		var lastBody string
		err := wait.For(func(context.Context) (bool, error) {
			st, b, e := httpPost(ctx, url, body, headers)
			if e != nil {
				return false, nil
			}
			last, lastBody = st, b
			return st == want, nil
		}, wait.WithTimeout(d), wait.WithInterval(5*time.Second))
		if err != nil {
			t.Fatalf("POST %s: want status %d, last=%d\nbody: %s", url, want, last, lastBody)
		}
		return ctx
	}
}

// ChatThroughGatewayWithin resolves the metallb LB IP of the istio gateway's
// Service (<gatewayName>-istio), then POSTs path until the response status equals
// want or the deadline passes. Resolving the IP from the Service avoids assuming
// the kind subnet / pool ordering.
func ChatThroughGatewayWithin(d time.Duration, gatewayName, gwNamespace, path, body string, headers map[string]string, want int) features.Func {
	return func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Helper()
		r := c.Client().Resources()
		var last int
		var lastBody, lastErr string
		err := wait.For(func(context.Context) (bool, error) {
			ip, e := gatewayLBIP(ctx, r, gatewayName, gwNamespace)
			if e != nil || ip == "" {
				lastErr = "gateway LB IP not assigned yet"
				return false, nil
			}
			st, b, e := httpPost(ctx, "http://"+ip+path, body, headers)
			if e != nil {
				lastErr = e.Error()
				return false, nil
			}
			last, lastBody = st, b
			return st == want, nil
		}, wait.WithTimeout(d), wait.WithInterval(5*time.Second))
		if err != nil {
			t.Fatalf("gateway %s/%s POST %s: want %d, last=%d (%s)\nbody: %s",
				gwNamespace, gatewayName, path, want, last, lastErr, lastBody)
		}
		return ctx
	}
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

func httpPost(ctx context.Context, url, body string, headers map[string]string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

// ChatBody builds a minimal OpenAI chat-completions body whose only routing
// signal is the model field — the BBR input.
func ChatBody(model, content string) string {
	return fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}]}`, model, content)
}
