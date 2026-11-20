//go:build e2e

package kindenv

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/praxis-proxy/dockyard/funcs"
	"github.com/praxis-proxy/dockyard/gateway"
)

// Stage 2 (increment 3): the streaming assertion ported from e2e-extproc.sh (27). A
// stream=true chat completion must come back 200 with at least two SSE data chunks
// that actually arrive at different times — proving the gateway streams incrementally
// rather than buffering the whole response and flushing it at once.
func TestExtProcStreaming(t *testing.T) {
	f := features.New("ExtProc streaming: SSE chunks arrive incrementally").
		Assess("stream=true yields >=2 data chunks with a measurable inter-chunk delay", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			apiFwd, err := funcs.PortForwardService(ctx, c, "maas-system", maasAPIService, maasAPIHTTPSPort)
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
			gwFwd, err := funcs.PortForwardService(ctx, c, "maas-system", gatewayService, gatewayHTTPPort)
			if err != nil {
				t.Fatalf("port-forward gateway: %v", err)
			}
			defer gwFwd.Close()

			// ChatCompletionStream posts to "<baseURL>/v1/chat/completions"; point the
			// base at the tenant/model prefix so the full path matches the gateway route.
			streamClient := gateway.New(gwFwd.URL("http")+"/"+tenantNamespace+"/demo",
				gateway.WithBearer(key), gateway.WithTimeout(45*time.Second))

			stream, err := streamClient.ChatCompletionStream(ctx,
				gateway.ChatRequest{Model: "demo", Messages: gateway.User("stream")})
			if err != nil {
				t.Fatalf("open stream (expected 200): %v", err)
			}
			defer func() { _ = stream.Close() }()

			start := time.Now()
			var chunks int
			var firstMS, lastMS int64
			for {
				_, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("recv chunk %d: %v", chunks+1, err)
				}
				elapsed := time.Since(start).Milliseconds()
				chunks++
				if chunks == 1 {
					firstMS = elapsed
				}
				lastMS = elapsed
			}

			if chunks < 2 {
				t.Errorf("streaming: got %d data chunks, want >=2", chunks)
			}
			if lastMS <= firstMS {
				t.Errorf("streaming: chunks not delayed (first=%dms last=%dms); response looks buffered", firstMS, lastMS)
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}
