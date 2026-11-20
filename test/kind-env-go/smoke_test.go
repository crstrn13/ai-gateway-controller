//go:build e2e

package kindenv

import (
	"context"
	"testing"
	"time"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/praxis-proxy/dockyard/funcs"
)

// TestPlatformSmoke is the bridge's first Feature: it proves run.sh provisioning
// stood up the platform far enough that the Go suite can reach the cluster and the
// istio control plane is serving. Stage 2 replaces this with the ported routing /
// extproc qualification (e2e.sh + e2e-extproc.sh).
func TestPlatformSmoke(t *testing.T) {
	f := features.New("kind-env platform is provisioned and istiod is Available").
		Assess("istiod Deployment becomes Available", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			return funcs.DeploymentBecomesAvailableWithin(3*time.Minute, "istio-system", "istiod")(ctx, t, c)
		}).
		Feature()
	testenv.Test(t, f)
}
