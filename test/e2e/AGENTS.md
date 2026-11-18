# test/e2e — agent & contributor guide

Kind-backed end-to-end suite on `sigs.k8s.io/e2e-framework`, arranged after
Crossplane's `test/e2e`. Read this before changing the suite so the structure
stays consistent.

## Layout

The harness is split into a **generic layer** (reusable across control-plane repos
— the convergence target) and a **product layer** (this repo's specifics). The
generic layer knows nothing about the AI Gateway controller; the product supplies
it via the `config.Product` seam.

```
test/e2e/
  config/            UNTAGGED, GENERIC. The environment scaffold.
    environment.go   Environment (embeds env.Environment): flags, test suites,
                     labels, cluster lifecycle. Delegates product concerns via
                     WithProduct. No controller/aigc imports.
    product.go       The Product interface: InstallDeps / AddTypesToScheme /
                     Enabled / Install. The per-repo seam.
  funcs/             UNTAGGED, GENERIC. The reusable fixture library:
    env.go           EnvFuncs, AsFeaturesFunc, InstallMetalLB/GatewayAPI/Istio
                     (functional options), LoadImage, InstallCRDs, ApplyManifests,
                     WaitForDeploymentAvailable.
    deps.go          Deps builder: NewDeps().WithMetalLB(...).WithGatewayAPI(...)...
                     .Build() — composes an ordered []env.Func dep stack.
    feature.go       AllOf, ApplyResources/DeleteResources, Create/Delete,
                     DeploymentBecomesAvailableWithin, ResourceCreatedWithin,
                     GatewayClient (build an SDK client for the gateway's LB IP).
  gateway/           UNTAGGED, GENERIC. A small HTTP SDK for the gateway:
                     gateway.New(baseURL), Client.ChatCompletion /
                     ChatCompletionStream / Post, Response. Pure HTTP — no
                     Kubernetes; funcs.GatewayClient resolves the IP.
  sim/               Standalone sim backends, each its OWN Go module (so
                     `go build ./...` at the repo root ignores them) built into an
                     image by the dep stack (funcs.Deps.WithBuiltImage):
    recorder/        A self-identifying OpenAI-compatible backend: stamps its
                     PROVIDER_NAME into every response's "provider" field and logs
                     requests at /__test/observations. The attribution signal for
                     provider-selection. Serves HTTPS:8443 when TLS_DIR holds certs
                     (providers are SIMPLE-TLS HTTPS; see product.ProviderBackend),
                     else HTTP:8080. Credential-free.
  product/           //go:build e2e, PRODUCT-SPECIFIC. This repo's config.Product.
    aigateway.go     AIGateway (implements config.Product): flags (-extproc-image,
                     -run-controller), InstallDeps (the Deps builder), AddTypesToScheme,
                     ControlPlane (in-process reconcilers). Imports controller-runtime
                     + pkg/{controller,tenant}.
    fixtures.go      Code fixtures: ReadyTenant, ExternalProvider/Model, ProviderSecret.
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

- The **generic layer** (`config/`, `funcs/`, `gateway/`) is **untagged** and has
  **no aigc/controller-runtime imports** — that is what keeps it reusable and
  importable on a plain `go build ./...`.
- The **product layer** (`product/`) and all `package e2e` files (`main_test.go`,
  `consts.go`, `*_test.go`) import controller-runtime + the reconcilers, so they
  are **`//go:build e2e`**.
- Anything that imports the controller, the reconcilers, or the CRD type packages
  belongs in `product/` (or a test file), never in the generic layer. If you reach
  for an aigc import inside `config`/`funcs`/`gateway`, that's the signal it belongs
  on the `Product` instead.

Verify any change with both:
```
go build ./test/e2e/config/ ./test/e2e/funcs/ ./test/e2e/gateway/   # generic must pass
go vet -tags=e2e ./test/e2e/ ./test/e2e/kind/ ./test/e2e/product/   # tagged must pass
```

## Fixtures: two kinds, pick per object

- **YAML** (`manifests/<area>/*.yaml` via `funcs.ApplyResources(dir, pattern)`) for
  the opaque static stack (backends, Gateway, EnvoyFilter). Paths are relative to
  the `test/e2e` package dir — `go test` sets cwd there (no `go:embed`).
- **Code** (`product` builders + `funcs.Create(...)`) for the CRs a test
  parameterizes (ExternalProvider/Model, tenants). `funcs.Create` is the generic
  applier; the CR builders are aigc-specific, so add them to `product/fixtures.go`.

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

## Lifecycle (generic Environment + the Product seam)

`TestMain` wires a `config.Environment` (generic: cluster lifecycle, suites,
labels) to a `product.AIGateway` via `environment.WithProduct(...)`. The order:

`CreateClusterFunc` (generic) → `Product.InstallDeps` (the product's ordered
`funcs.NewDeps()` stack: metallb → Gateway API → GIE CRD → istio → load image →
shared backend + wait) → suite-specific setup → `Product.AddTypesToScheme` →
`Product.Install` (controller CRDs + reconcilers in-process, real ext-proc image,
gated on `-run-controller`) → `DestroyClusterFunc` (generic).

Adding a reusable **generic** installer → `funcs/` (+ a `Deps.WithX` method).
Adding anything **aigc-specific** (a dep, a CR type, control-plane wiring) → the
`product` package, behind the `Product` interface. Per-test CRs belong in a
feature's `WithSetup`.

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
- `TestProviderSelectionWeighted` (provider_selection_test.go) — one client model
  bound to two weighted providers (4:1); samples 200 requests and checks the served
  distribution against the 80/20 split with a chi-square goodness-of-fit test
  (df=1, alpha=0.001). Ported from kind-env's e2e-static-provider-weights.py. Two
  recorder sims (product.ProviderBackend, HTTPS) stamp their identity
  into each response; attribution reads the "provider" field (providerFromResponse).
- `TestLLMD` (llmd_test.go) — self-hosted / llm-d via GIE: istio routes to an
  InferencePool, the EPP picks a vLLM pod. Pure data-plane (NOT controller-driven —
  no llm-d reconciliation exists here; RHAIGW-468 groundwork). EPP + pool are
  vendored from the praxis demo; the HTTPRoute→InferencePool (istio-GIE) is the
  unvalidated frontier.
