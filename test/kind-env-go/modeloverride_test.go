//go:build e2e

package kindenv

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	inferencev1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
	"github.com/praxis-proxy/dockyard/feature"
	"github.com/praxis-proxy/dockyard/gateway"
)

// Stage 2 (increment 8): the model-level credential override assertions ported from
// e2e-extproc.sh (28-31). A per-model Auth override must flow all the way to the data
// plane — the overlay candidate's credential secretRef, the ext-proc plugins ConfigMap,
// and the Deployment's projected volume all name the override Secret — and serve
// requests (28). Deleting the Secret fails the model closed without republishing a
// broken overlay (29); recreating it recovers (30); removing the override restores the
// provider default (31).
const (
	overrideSecret   = "provider-model-override-credentials"
	extPluginsCM     = "payload-processing-external-model-plugins"
	providerACluster = "provider-provider-a"
)

func TestModelCredentialOverride(t *testing.T) {
	f := features.New("Model credential override: per-model Auth reaches the data plane, fails closed, recovers").
		Assess("override secret reaches overlay/config/volume and serves, deletion fails closed, recreation recovers, removal restores default", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			apiFwd, err := feature.PortForwardService(ctx, c, apiNamespace, maasAPIService, maasAPIHTTPSPort)
			if err != nil {
				t.Fatalf("port-forward maas-api: %v", err)
			}
			defer apiFwd.Close()
			caPEM, err := runOwnedCA(ctx, c)
			if err != nil {
				t.Fatalf("read run-owned CA: %v", err)
			}
			key, err := mintMaaSKey(ctx, apiFwd.Addr(), caPEM)
			if err != nil {
				t.Fatalf("mint MaaS key: %v", err)
			}
			gwFwd, err := feature.PortForwardService(ctx, c, apiNamespace, gatewayService, gatewayHTTPPort)
			if err != nil {
				t.Fatalf("port-forward gateway: %v", err)
			}
			defer gwFwd.Close()
			authed := gateway.New(gwFwd.URL("http"), gateway.WithBearer(key))
			demoPath := "/" + tenantNamespace + "/demo/v1/chat/completions"
			chat := gateway.ChatRequest{Model: "demo", Messages: gateway.User("hello")}

			if err := waitModelPhase(ctx, c, "Ready", 2*time.Minute); err != nil {
				t.Fatalf("precondition model Ready: %v", err)
			}

			// 31 cleanup always: drop the override and the Secret, restore the default.
			defer func() {
				if err := clearProviderAuth(ctx, c); err != nil {
					t.Errorf("clear provider auth: %v", err)
				}
				_ = deleteSecret(ctx, c, tenantNamespace, overrideSecret)
				if err := waitModelPhase(ctx, c, "Ready", 2*time.Minute); err != nil {
					t.Errorf("restore model Ready: %v", err)
				}
				if err := waitOverlay(ctx, c, "a", 2*time.Minute); err != nil {
					t.Errorf("restore overlay on provider-a: %v", err)
				}
			}()

			// Create the override Secret and point provider-a's per-model Auth at it.
			if err := applyOverrideSecret(ctx, c); err != nil {
				t.Fatalf("create override secret: %v", err)
			}
			if err := setProviderAuth(ctx, c, overrideSecret); err != nil {
				t.Fatalf("set provider auth override: %v", err)
			}
			if err := waitModelPhase(ctx, c, "Ready", 2*time.Minute); err != nil {
				t.Fatalf("model Ready after override: %v", err)
			}
			if err := waitOverlay(ctx, c, "a", 2*time.Minute); err != nil {
				t.Fatalf("overlay converge after override: %v", err)
			}

			// 28 preconditions: the override must reach overlay, config, and volume.
			ref, err := mountedCredentialSecretRef(ctx, c, providerACluster)
			if err != nil {
				t.Fatalf("read mounted credential secretRef: %v", err)
			}
			if ref != overrideSecret {
				t.Errorf("overlay candidate credential secretRef = %q, want %q", ref, overrideSecret)
			}
			inConfig, err := extConfigReferences(ctx, c, overrideSecret)
			if err != nil {
				t.Fatalf("read ext plugins configmap: %v", err)
			}
			if !inConfig {
				t.Errorf("ext-proc plugins config does not reference %q", overrideSecret)
			}
			projected, err := deploymentProjectsSecret(ctx, c, overrideSecret)
			if err != nil {
				t.Fatalf("read ext deployment volumes: %v", err)
			}
			if !projected {
				t.Errorf("ext-proc Deployment does not project secret %q", overrideSecret)
			}

			// 28. Request served under the override.
			assertStatus(t, postChat(ctx, t, authed, demoPath, chat), http.StatusOK, "model_level_credential_override")

			// 29. Delete the Secret and force a reconcile: the model fails closed and the
			// serving overlay digest is unchanged (no broken overlay published).
			beforeDigest, err := servingDigest(ctx, c)
			if err != nil {
				t.Fatalf("serving digest (before delete): %v", err)
			}
			if err := deleteSecret(ctx, c, tenantNamespace, overrideSecret); err != nil {
				t.Fatalf("delete override secret: %v", err)
			}
			if err := annotateModel(ctx, c, "external-model-e2e/override-probe", fmt.Sprintf("%d", time.Now().Unix())); err != nil {
				t.Fatalf("annotate model to force reconcile: %v", err)
			}
			if err := waitModelPhase(ctx, c, "Failed", 2*time.Minute); err != nil {
				t.Fatalf("model did not fail closed on missing override secret: %v", err)
			}
			afterDigest, err := servingDigest(ctx, c)
			if err != nil {
				t.Fatalf("serving digest (after delete): %v", err)
			}
			if afterDigest != beforeDigest {
				t.Errorf("serving digest churned on missing override: before=%s after=%s", beforeDigest, afterDigest)
			}

			// 30. Recreate the Secret: the model recovers and serves again.
			if err := applyOverrideSecret(ctx, c); err != nil {
				t.Fatalf("recreate override secret: %v", err)
			}
			if err := waitModelPhase(ctx, c, "Ready", 2*time.Minute); err != nil {
				t.Fatalf("model did not recover after recreating secret: %v", err)
			}
			if err := waitOverlay(ctx, c, "a", 2*time.Minute); err != nil {
				t.Fatalf("overlay converge after recovery: %v", err)
			}
			assertStatus(t, postChat(ctx, t, authed, demoPath, chat), http.StatusOK, "model_override_recovery")

			// 31 asserted by the deferred cleanup (default restored, model Ready).
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// servingDigest returns the currently published overlay content digest (the cm annotation).
func servingDigest(ctx context.Context, c *envconf.Config) (string, error) {
	st, err := readOverlayState(ctx, c)
	if err != nil {
		return "", err
	}
	return st.declared, nil
}

// applyOverrideSecret creates-or-updates the opaque override Secret (data api-key=dummy).
func applyOverrideSecret(ctx context.Context, c *envconf.Config) error {
	r := c.Client().Resources()
	err := r.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: overrideSecret, Namespace: tenantNamespace},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"api-key": "kind-only-dummy"},
	})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func deleteSecret(ctx context.Context, c *envconf.Config, namespace, name string) error {
	err := c.Client().Resources().Delete(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// setProviderAuth sets the per-model Auth override on the first provider ref.
func setProviderAuth(ctx context.Context, c *envconf.Config, secretName string) error {
	r := c.Client().Resources()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var em inferencev1alpha1.ExternalModel
		if err := r.Get(ctx, externalModelName, tenantNamespace, &em); err != nil {
			return err
		}
		em.Spec.ExternalProviderRefs[0].Auth = &inferencev1alpha1.AuthConfig{
			Type:      "apikey",
			SecretRef: inferencev1alpha1.NameReference{Name: secretName},
		}
		return r.Update(ctx, &em)
	})
}

func clearProviderAuth(ctx context.Context, c *envconf.Config) error {
	r := c.Client().Resources()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var em inferencev1alpha1.ExternalModel
		if err := r.Get(ctx, externalModelName, tenantNamespace, &em); err != nil {
			return err
		}
		em.Spec.ExternalProviderRefs[0].Auth = nil
		return r.Update(ctx, &em)
	})
}

func annotateModel(ctx context.Context, c *envconf.Config, key, value string) error {
	r := c.Client().Resources()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var em inferencev1alpha1.ExternalModel
		if err := r.Get(ctx, externalModelName, tenantNamespace, &em); err != nil {
			return err
		}
		if em.Annotations == nil {
			em.Annotations = map[string]string{}
		}
		em.Annotations[key] = value
		return r.Update(ctx, &em)
	})
}

// overlayCandidate is the subset of a mounted-overlay candidate we assert on.
type overlayCandidate struct {
	Cluster    string `json:"cluster"`
	Credential struct {
		SecretRef struct {
			Name string `json:"name"`
		} `json:"secretRef"`
	} `json:"credential"`
}

// mountedCredentialSecretRef reads the mounted overlay and returns the credential
// secretRef name for the given candidate cluster.
func mountedCredentialSecretRef(ctx context.Context, c *envconf.Config, cluster string) (string, error) {
	raw, err := feature.ExecInDeployment(ctx, c, tenantNamespace, extDeployment, "cat", overlayMountPath)
	if err != nil {
		return "", err
	}
	var doc struct {
		Overlay struct {
			Candidates []overlayCandidate `json:"candidates"`
		} `json:"overlay"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return "", fmt.Errorf("parse mounted overlay: %w", err)
	}
	for _, cand := range doc.Overlay.Candidates {
		if cand.Cluster == cluster {
			return cand.Credential.SecretRef.Name, nil
		}
	}
	return "", fmt.Errorf("candidate %q not found in mounted overlay", cluster)
}

// extConfigReferences reports whether the ext-proc plugins ConfigMap references needle.
func extConfigReferences(ctx context.Context, c *envconf.Config, needle string) (bool, error) {
	var cm corev1.ConfigMap
	if err := c.Client().Resources().Get(ctx, extPluginsCM, tenantNamespace, &cm); err != nil {
		return false, err
	}
	for _, v := range cm.Data {
		if strings.Contains(v, needle) {
			return true, nil
		}
	}
	return false, nil
}

// deploymentProjectsSecret reports whether the ext-proc Deployment projects secretName
// as a volume source (projected or plain secret volume).
func deploymentProjectsSecret(ctx context.Context, c *envconf.Config, secretName string) (bool, error) {
	pod, err := feature.DeploymentPod(ctx, c, tenantNamespace, extDeployment)
	if err != nil {
		return false, err
	}
	for _, vol := range pod.Spec.Volumes {
		if vol.Secret != nil && vol.Secret.SecretName == secretName {
			return true, nil
		}
		if vol.Projected != nil {
			for _, src := range vol.Projected.Sources {
				if src.Secret != nil && src.Secret.Name == secretName {
					return true, nil
				}
			}
		}
	}
	return false, nil
}
