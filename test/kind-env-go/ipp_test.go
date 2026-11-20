//go:build e2e

package kindenv

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/praxis-proxy/dockyard/funcs"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

// Stage 6: the IPP→Praxis cutover/rollback from e2e-ipp-translation-migration.py. A
// tenant's MaasTenantConfig carries a payload-processing-type; flipping the
// transition tenant from the MaaS-owned IPP path to Praxis must cut over cleanly —
// the handoff status passes through cleanup-complete (the IPP resources are torn down)
// and settles at steady — and flipping back must roll the tenant onto IPP again while
// staying Ready.
const (
	transitionTenantNS = "ai-tenant-transition"
	tenantConfigName   = "default-tenant"
	payloadTypeAnno    = "maas.opendatahub.io/payload-processing-type"
	payloadStatusAnno  = "maas.opendatahub.io/payload-processing-status"
)

var maasTenantConfigGVKTest = schema.GroupVersionKind{Group: "maas.opendatahub.io", Version: "v1alpha1", Kind: "MaasTenantConfig"}

func TestIPPCutoverRollback(t *testing.T) {
	f := features.New("IPP cutover/rollback: transition tenant moves IPP↔Praxis cleanly").
		Assess("cutover to Praxis cleans up IPP and reaches steady, rollback returns to IPP Ready", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			// Baseline: the transition tenant is on the IPP path and Ready.
			typ, err := mtcAnnotation(ctx, c, payloadTypeAnno)
			if err != nil {
				t.Fatalf("read transition tenant: %v", err)
			}
			if typ != "ipp" {
				t.Fatalf("precondition: transition tenant type = %q, want ipp", typ)
			}

			// Always restore the IPP baseline.
			defer func() {
				if err := setMTCType(ctx, c, "ipp"); err != nil {
					t.Errorf("restore IPP type: %v", err)
					return
				}
				if err := waitMTC(ctx, c, "ipp", "", true, 3*time.Minute, nil); err != nil {
					t.Errorf("restore to IPP Ready: %v", err)
				}
			}()

			// Cutover: IPP → Praxis. The handoff must reach steady, passing through the
			// cleanup-complete phase where the IPP-owned resources are torn down.
			if err := setMTCType(ctx, c, "praxis"); err != nil {
				t.Fatalf("cutover to praxis: %v", err)
			}
			sawCleanup := false
			if err := waitMTC(ctx, c, "praxis", "steady", true, 3*time.Minute, func(status string) {
				if status == "cleanup-complete" {
					sawCleanup = true
				}
			}); err != nil {
				t.Fatalf("cutover did not reach praxis/steady: %v", err)
			}
			t.Logf("cutover reached steady (observed cleanup-complete=%v)", sawCleanup)

			// Rollback: Praxis → IPP. The tenant must return to the IPP path and stay Ready.
			if err := setMTCType(ctx, c, "ipp"); err != nil {
				t.Fatalf("rollback to ipp: %v", err)
			}
			if err := waitMTC(ctx, c, "ipp", "", true, 3*time.Minute, nil); err != nil {
				t.Fatalf("rollback did not return to ipp Ready: %v", err)
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// transitionTenantConfig fetches the transition tenant's MaasTenantConfig.
func transitionTenantConfig(ctx context.Context, c *envconf.Config) (*unstructured.Unstructured, error) {
	mtc := &unstructured.Unstructured{}
	mtc.SetGroupVersionKind(maasTenantConfigGVKTest)
	if err := c.Client().Resources().Get(ctx, tenantConfigName, transitionTenantNS, mtc); err != nil {
		return nil, err
	}
	return mtc, nil
}

// mtcAnnotation reads one annotation from the transition tenant's MaasTenantConfig.
func mtcAnnotation(ctx context.Context, c *envconf.Config, key string) (string, error) {
	mtc, err := transitionTenantConfig(ctx, c)
	if err != nil {
		return "", err
	}
	return mtc.GetAnnotations()[key], nil
}

// setMTCType sets the payload-processing-type annotation (retry-on-conflict).
func setMTCType(ctx context.Context, c *envconf.Config, value string) error {
	r := c.Client().Resources()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		mtc, err := transitionTenantConfig(ctx, c)
		if err != nil {
			return err
		}
		anns := mtc.GetAnnotations()
		if anns == nil {
			anns = map[string]string{}
		}
		anns[payloadTypeAnno] = value
		mtc.SetAnnotations(anns)
		return r.Update(ctx, mtc)
	})
}

// waitMTC polls the transition tenant until its type matches wantType, its status matches
// wantStatus (ignored when empty), and (when wantReady) its Ready condition is True. The
// optional onStatus callback observes each status value seen (e.g. to catch a transient).
func waitMTC(ctx context.Context, c *envconf.Config, wantType, wantStatus string, wantReady bool, timeout time.Duration, onStatus func(string)) error {
	return funcs.Retry(ctx, timeout, 2*time.Second, "MaasTenantConfig convergence", func(ctx context.Context) error {
		mtc, err := transitionTenantConfig(ctx, c)
		if err != nil {
			return err
		}
		anns := mtc.GetAnnotations()
		typ := anns[payloadTypeAnno]
		status := anns[payloadStatusAnno]
		if onStatus != nil && status != "" {
			onStatus(status)
		}
		ready := mtcReady(mtc)
		if typ == wantType && (wantStatus == "" || status == wantStatus) && (!wantReady || ready) {
			return nil
		}
		return fmt.Errorf("type=%q status=%q ready=%v (want type=%q status=%q ready=%v)",
			typ, status, ready, wantType, wantStatus, wantReady)
	})
}

// mtcReady reports whether the MaasTenantConfig's Ready condition is True.
func mtcReady(mtc *unstructured.Unstructured) bool {
	conds, found, _ := unstructured.NestedSlice(mtc.Object, "status", "conditions")
	if !found {
		return false
	}
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cm["type"] == "Ready" && cm["status"] == "True" {
			return true
		}
	}
	return false
}
