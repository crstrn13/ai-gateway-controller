//go:build e2e

package kindenv

import (
	"context"
	"fmt"
	"testing"
	"time"

	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	inferencev1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
	"github.com/praxis-proxy/dockyard/funcs"
)

// Stage 2 (increment 5): the duplicate active-provider binding assertions ported from
// e2e-extproc.sh (32/33). Binding the same provider twice as an active (non-zero
// weight) candidate is ambiguous routing, so the controller must reject the spec —
// move the ExternalModel to Failed and leave the serving overlay digest unchanged
// (fail closed, do not publish a broken overlay) — then recover to Ready once the
// duplicate is removed.
func TestDuplicateProviderBinding(t *testing.T) {
	f := features.New("Duplicate provider binding: rejected to Failed, serving digest unchanged, recovers").
		Assess("adding a duplicate active provider ref fails the model without churning the overlay, removal recovers", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			// Baseline: provider-a active, overlay converged, model Ready.
			if err := waitModelPhase(ctx, c, "Ready", 2*time.Minute); err != nil {
				t.Fatalf("precondition model Ready: %v", err)
			}
			before, err := readOverlayState(ctx, c)
			if err != nil {
				t.Fatalf("overlay state (before): %v", err)
			}

			// Always strip the duplicate and restore Ready, whatever happens.
			defer func() {
				if err := removeDuplicateProviderRef(ctx, c); err != nil {
					t.Errorf("remove duplicate provider ref: %v", err)
					return
				}
				if err := waitModelPhase(ctx, c, "Ready", 2*time.Minute); err != nil {
					t.Errorf("recover model Ready: %v", err)
				}
				if err := waitOverlay(ctx, c, "a", 2*time.Minute); err != nil {
					t.Errorf("recover overlay on provider-a: %v", err)
				}
			}()

			// 32. Duplicate the first provider ref as an active (weight 1) candidate.
			if err := addDuplicateProviderRef(ctx, c); err != nil {
				t.Fatalf("add duplicate provider ref: %v", err)
			}
			if err := waitModelPhase(ctx, c, "Failed", 2*time.Minute); err != nil {
				t.Fatalf("model did not move to Failed on duplicate binding: %v", err)
			}
			after, err := readOverlayState(ctx, c)
			if err != nil {
				t.Fatalf("overlay state (after): %v", err)
			}
			if after.declared != before.declared {
				t.Errorf("serving overlay digest churned on rejected spec: before=%s after=%s", before.declared, after.declared)
			}
			// 33 is asserted by the deferred recovery (model Ready + overlay on A).
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// waitModelPhase polls the ExternalModel's status.phase until it equals wanted.
func waitModelPhase(ctx context.Context, c *envconf.Config, wanted string, timeout time.Duration) error {
	return funcs.Retry(ctx, timeout, 2*time.Second, "model phase "+wanted, func(ctx context.Context) error {
		var em inferencev1alpha1.ExternalModel
		if err := c.Client().Resources().Get(ctx, externalModelName, tenantNamespace, &em); err != nil {
			return err
		}
		if em.Status.Phase == wanted {
			return nil
		}
		return fmt.Errorf("phase = %q, want %q", em.Status.Phase, wanted)
	})
}

// addDuplicateProviderRef appends a copy of the first provider ref with weight 1, so the
// same provider is bound twice as an active candidate.
func addDuplicateProviderRef(ctx context.Context, c *envconf.Config) error {
	r := c.Client().Resources()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var em inferencev1alpha1.ExternalModel
		if err := r.Get(ctx, externalModelName, tenantNamespace, &em); err != nil {
			return err
		}
		if len(em.Spec.ExternalProviderRefs) == 0 {
			return fmt.Errorf("externalModel has no provider refs")
		}
		dup := *em.Spec.ExternalProviderRefs[0].DeepCopy()
		one := 1
		dup.Weight = &one
		em.Spec.ExternalProviderRefs = append(em.Spec.ExternalProviderRefs, dup)
		return r.Update(ctx, &em)
	})
}

// removeDuplicateProviderRef trims the ExternalModel back to its first two provider refs
// (provider-a, provider-b), dropping any appended duplicate.
func removeDuplicateProviderRef(ctx context.Context, c *envconf.Config) error {
	r := c.Client().Resources()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var em inferencev1alpha1.ExternalModel
		if err := r.Get(ctx, externalModelName, tenantNamespace, &em); err != nil {
			return err
		}
		if len(em.Spec.ExternalProviderRefs) <= 2 {
			return nil
		}
		em.Spec.ExternalProviderRefs = em.Spec.ExternalProviderRefs[:2]
		return r.Update(ctx, &em)
	})
}
