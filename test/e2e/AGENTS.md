# test/e2e — agent & contributor guide

Kind-backed end-to-end suite on `sigs.k8s.io/e2e-framework`, arranged after
Crossplane's `test/e2e`. Read this before changing the suite so the structure
stays consistent.

## Layout

```
test/e2e/
  config/
    environment.go   UNTAGGED. Environment (embeds env.Environment): flags,
                     test suites, labels, cluster lifecycle, InstallDeps.
    controller.go    //go:build e2e. The "install the product" layer:
                     InstallController, InstallCRDs, ControlPlane (in-process
                     reconcilers). Imports controller-runtime + pkg/{controller,tenant}.
  funcs/             UNTAGGED. The fixture library (reusable features.Func / env.Func):
    env.go           EnvFuncs, AsFeaturesFunc, AddTypesToScheme, InstallMetalLB/
                     GatewayAPI/Istio, LoadImage, InstallCRDs, ApplyManifests +
                     WaitForDeploymentAvailable (suite-level infra, env.Func).
    feature.go       AllOf, ApplyResources/DeleteResources, Create/Delete,
                     DeploymentBecomesAvailableWithin, ResourceCreatedWithin,
                     GatewayClient (build an SDK client for the gateway's LB IP).
    fixtures.go      Code fixtures: ReadyTenant, ExternalProvider/Model, ProviderSecret.
  gateway/           UNTAGGED. A small HTTP SDK for the gateway: gateway.New(baseURL),
                     Client.ChatCompletion / ChatCompletionStream / Post, Response.
                     Pure HTTP — no Kubernetes; funcs.GatewayClient resolves the IP.
  manifests/
    backends/        shared sim backend (applied once in TestMain, not per-test).
    gie/             Gateway API Inference Extension InferencePool CRD (InstallDeps).
    llmd/            llm-d stack for area=llm-d: EPP + InferencePool + vLLM-vcr
                     members + the GIE HTTPRoute (vendored from the praxis demo).
    <area>/*.yaml    per-area YAML fixtures (applied in a feature via funcs.ApplyResources).
    crds/            MaaS CRD stubs the in-process controller watches.
  consts.go          //go:build e2e, package e2e. Label keys/values, FieldManager.
  main_test.go       //go:build e2e. TestMain: flags -> env, suites, setup/finish.
  <area>_test.go     //go:build e2e. One Feature per area (e.g. bbr_test.go).
  kind/              A separate control-plane-only lane (reuses config.ControlPlane).
```

## The build-tag rule (do not break `go build ./...`)

- `config/environment.go` and everything in `funcs/` are **untagged** libraries.
- Files that import controller-runtime or the reconcilers (`config/controller.go`)
  and all `package e2e` files (`main_test.go`, `consts.go`, `*_test.go`) are
  **`//go:build e2e`**.
- A package must keep **at least one untagged file**, or `go build ./...` fails
  with "build constraints exclude all Go files". That is why `config` is split:
  `environment.go` (untagged) + `controller.go` (e2e). Methods of `Environment`
  may live in either file.

Verify any change with both:
```
go build ./test/e2e/config/ ./test/e2e/funcs/      # untagged must pass
go vet -tags=e2e ./test/e2e/ ./test/e2e/kind/      # tagged must pass
```

## Fixtures: two kinds, pick per object

- **YAML** (`manifests/<area>/*.yaml` via `funcs.ApplyResources(dir, pattern)`) for
  the opaque static stack (backends, Gateway, EnvoyFilter). Paths are relative to
  the `test/e2e` package dir — `go test` sets cwd there (no `go:embed`).
- **Code** (`funcs` builders + `funcs.Create(...)`) for the CRs a test
  parameterizes (ExternalProvider/Model, tenants). Add builders to `funcs/fixtures.go`.

## Test shape (convention)

- **Setup / Teardown = fixtures.** Compose reusable `funcs`/`fixtures` helpers
  (`ApplyResources`, `ReadyTenant`, `Create`, `DeploymentBecomesAvailableWithin`,
  `Delete`, …) via `funcs.AllOf(...)`. Keep them declarative; no assertions here
  beyond "it came up".
- **Assess = inline code.** Write the assertion as a raw closure in the test, not
  behind a helper — the check should be readable where it runs:

  ```go
  Assess("body `model` routes to the backend", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
      gw, err := funcs.GatewayClient(ctx, c, "ai-gateway", "istio-system")
      if err != nil {
          t.Fatal(err)
      }
      resp, err := gw.ChatCompletion(ctx, gateway.ChatRequest{Model: "granite-8b", Messages: gateway.User("hello")})
      if err != nil {
          t.Fatal(err)
      }
      if resp.StatusCode != http.StatusOK {
          t.Errorf("want %d, got %d: %s", http.StatusOK, resp.StatusCode, resp.String())
      }
      return ctx
  }),
  ```

  Rules for the closure: named params `(ctx, t, c)`, it MUST `return ctx`, use
  stdlib `t.Fatal`/`t.Errorf` (not testify), and `http.Status*` constants (not bare
  200/404). Drive traffic through the `gateway` SDK (`funcs.GatewayClient` →
  `gw.ChatCompletion`/`ChatCompletionStream`). **No assertion/chat helper in
  `funcs`** — the assertion lives inline. If the check needs eventual-consistency
  retry, write a `wait.For` loop inline too (don't hide it behind a helper). The
  only `funcs` thing an Assess may call is the `GatewayClient` constructor.

## Adding an area (e.g. intelligent routing)

1. `manifests/<area>/*.yaml` for its static stack.
2. `<area>_test.go` (`//go:build e2e`, package `e2e`) with one
   `features.NewWithDescription(...).WithLabel(LabelArea, ...).WithLabel(config.LabelTestSuite, config.TestSuiteDefault)...Feature()`.
3. Reuse `funcs`/fixtures; add a helper there only if genuinely new.
4. No changes to `TestMain`.

Every Feature MUST set `config.LabelTestSuite` — `TestMain`'s `BeforeEachFeature`
fails the run otherwise.

## Lifecycle (owned by config.Environment)

`CreateClusterFunc` → `InstallDeps` (metallb → Gateway API → istio → load image)
→ shared backend (`ApplyManifests manifests/backends` + wait) → `InstallController`
(installs the controller CRDs + runs the reconcilers in-process with the real
ext-proc image) → `DestroyClusterFunc`. Shared infra belongs here; per-test CRs
belong in a feature's `WithSetup`.

## Conventions

- **Generic kind names only** — `ai-gateway` / `istio-system`, never OpenShift
  names (`maas-default-gateway`, `openshift-ingress`). Those live only in the
  pre-existing `scripts/` OpenShift harness and the vendored MaaS CRD stubs.
- **Image** comes from config (`-extproc-image`, default `praxis-extproc:dev`),
  never a `quay.io/example/...:test` placeholder.
- **Do NOT call `klog.InitFlags`** — `envconf.NewFromFlags()` already registers
  klog's flags; a second call panics ("flag redefined"). `-v=N` works via the
  framework.
- Logic in Go (klient/decoder), not shell — except the few dependency installers
  with no Go path (istioctl, kind, kubectl-from-URL for CRDs).

## Running

```
make test-e2e                              # all areas, fresh kind cluster
make test-e2e-bbr                          # area=bbr
make test-e2e E2E_LABELS=area=bbr
go test -tags=e2e ./test/e2e/ -create-cluster=false -install-deps=false   # live cluster
```

All tests run by default (no skip gates). Filter with `--labels area=<area>` or
`-run <name>`. A test fails (not skips) if its prerequisites — e.g. the ext-proc
image being loadable, or llm-d image egress — aren't there; that's intentional.

Flags: `-create-cluster`, `-destroy-cluster`, `-install-deps`, `-run-controller`,
`-cluster-name`, `-extproc-image`, `-test-suite`, framework `--labels`.

## Tests today

- `TestBBR` (bbr_test.go) — controller-driven body-based routing: CRs →
  controller renders the ext-proc → body `model` routes. Needs the ext-proc image
  loaded (baseline does this) + the provider transport.
- `TestLLMD` (llmd_test.go) — self-hosted / llm-d via GIE: istio routes to an
  InferencePool, the EPP picks a vLLM pod. Pure data-plane (NOT controller-driven —
  no llm-d reconciliation exists here; RHAIGW-468 groundwork). EPP + pool are
  vendored from the praxis demo; the HTTPRoute→InferencePool (istio-GIE) is the
  unvalidated frontier.
