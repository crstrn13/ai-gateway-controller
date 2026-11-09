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
                     GatewayAPI/Istio, LoadImage.
    feature.go       AllOf, ApplyResources/DeleteResources, Create/Delete,
                     DeploymentBecomesAvailableWithin, ResourceCreatedWithin,
                     HTTPPostStatusWithin, ChatBody.
    fixtures.go      Code fixtures: ReadyTenant, ExternalProvider/Model, ProviderSecret.
  manifests/
    <area>/*.yaml    YAML fixtures per area (applied with funcs.ApplyResources).
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
→ `InstallController` (installs the controller CRDs + runs the reconcilers
in-process with the real ext-proc image) → `DestroyClusterFunc`.

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
AIGW_EXTPROC_READY=1 ...                    # also runs the controller-driven BBR test
```

Flags: `-create-cluster`, `-destroy-cluster`, `-install-deps`, `-run-controller`,
`-cluster-name`, `-extproc-image`, `-test-suite`, framework `--labels`.

## Tests today

- `TestBBR` (bbr_test.go) — controller-driven body-based routing: CRs →
  controller renders the ext-proc → body `model` routes. Gated behind
  `AIGW_EXTPROC_READY=1` (needs the real image loaded; the provider-transport → 200
  is the part to validate on a live cluster).
