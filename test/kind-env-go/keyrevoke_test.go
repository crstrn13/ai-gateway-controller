//go:build e2e

package kindenv

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/praxis-proxy/dockyard/funcs"
)

// Stage 2 (increment 10): the key-revocation assertion ported from e2e-extproc.sh (37).
// An admin can revoke a minted MaaS API key via DELETE /v1/api-keys/{id}; the endpoint
// must accept the revocation (200/204, or 404 if already gone).
func TestMaaSKeyRevocation(t *testing.T) {
	f := features.New("MaaS key revocation: an admin can revoke a minted key").
		Assess("mint then revoke returns a success status", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			apiFwd, err := funcs.PortForwardService(ctx, c, apiNamespace, maasAPIService, maasAPIHTTPSPort)
			if err != nil {
				t.Fatalf("port-forward maas-api: %v", err)
			}
			defer apiFwd.Close()
			caPEM, err := runOwnedCA(ctx, c)
			if err != nil {
				t.Fatalf("read run-owned CA: %v", err)
			}
			client, err := maasAPIClient(caPEM)
			if err != nil {
				t.Fatalf("build maas-api client: %v", err)
			}

			id, _, err := createMaaSKey(ctx, client, apiFwd.Addr())
			if err != nil {
				t.Fatalf("create key: %v", err)
			}
			if id == "" {
				t.Fatal("create key returned no id to revoke")
			}
			status, err := revokeMaaSKey(ctx, client, apiFwd.Addr(), id)
			if err != nil {
				t.Fatalf("revoke key: %v", err)
			}
			if status != http.StatusOK && status != http.StatusNoContent && status != http.StatusNotFound {
				t.Errorf("revoke status = %d, want 200/204/404", status)
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

// maasAPIClient builds an HTTPS client that trusts the run-owned CA with SNI pinned to
// the maas-api Service DNS.
func maasAPIClient(caPEM []byte) (*http.Client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("invalid CA PEM")
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: maasAPIServerName},
		},
	}, nil
}

// createMaaSKey mints an ephemeral key as admin and returns its id and value.
func createMaaSKey(ctx context.Context, client *http.Client, apiAddr string) (id, key string, err error) {
	body := fmt.Sprintf(`{"name":"kind-e2e-revoke","ephemeral":true,"subscription":%q}`, keySubscription)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+apiAddr+"/v1/api-keys", strings.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("X-MaaS-Username", "kind-user")
	req.Header.Set("X-MaaS-Group", `["system:authenticated"]`)
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return "", "", fmt.Errorf("create key: status %d", resp.StatusCode)
	}
	var out struct {
		ID    string `json:"id"`
		KeyID string `json:"keyId"`
		Key   string `json:"key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	id = out.ID
	if id == "" {
		id = out.KeyID
	}
	return id, out.Key, nil
}

// revokeMaaSKey deletes a key by id as admin and returns the HTTP status.
func revokeMaaSKey(ctx context.Context, client *http.Client, apiAddr, id string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "https://"+apiAddr+"/v1/api-keys/"+id, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-MaaS-Username", "kind-user")
	req.Header.Set("X-MaaS-Group", `["system:authenticated"]`)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}
