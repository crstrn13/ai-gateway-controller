# Dockyard — extraction plan

A shared, cluster-agnostic Go e2e framework for the org: it provisions a real
platform (kind or an existing kubeconfig), docks your product into it, and runs
sea trials. Built on `sigs.k8s.io/e2e-framework`. Extracted from the
ai-gateway-controller `test/e2e` harness, which is its first consumer.

Metaphor (naming system): the framework = the **dockyard**; each repo's
`Product` = a **vessel** fitting out; the installers / `Deps` = **fitting-out**;
the tests = **sea trials**.

## Scope — the L1 / L2 / L3 boundary (read first)

The single rule that decides whether Dockyard ages well: **L1 stays generic and
small; it delegates product provisioning instead of reimplementing it.**

- **L1 — Dockyard (this new repo).** Generic, no org/product-specific knowledge.
  Cluster lifecycle, the `Product` seam, the `Deps` builder + generic infra
  installers (metallb, Gateway API, istio, cert-manager, kuadrant), image
  build/load, manifest/CRD apply + waits, the gateway HTTP SDK, the recorder sim.
  Already import-clean today (`config`/`funcs`/`gateway`/`sim` have zero aigc
  imports — that was the point of the Product seam).
- **L2 — praxis platform (stays in aigc for now).** Service-ca stand-in
  (`ExtProcServingCerts`, `MountGatewayCATrust`), the self-signed CA issuer, the
  kuadrant gateway `AuthPolicy`, the HTTPS `ProviderBackend`. These encode
  praxis/OpenShift conventions (service-ca secret names, the kuadrant wasm
  anchor, the provider-HTTPS contract). Do NOT move them into L1 yet — revisit a
  shared `praxis-platform` layer only once a 2nd consumer (grid / gateway-
  controller) confirms the common shape.
- **L3 — per repo.** The `Product` implementation (controller install + CRD
  types), CR fixtures, and the test files. Stays in each repo.

## Repo creation checklist (do this now)

1. Create repo **`dockyard`**. Decide the org/module path:
   - `github.com/opendatahub-io/dockyard` (home of the first consumer), or
   - `github.com/praxis-proxy/dockyard` (neutral across praxis repos).
   Recommendation: pick the org that will own long-term maintenance.
2. `go mod init github.com/<org>/dockyard`; Go version matching the consumers
   (aigc is on go 1.26). Apache-2.0 license + headers (matches aigc).
3. Minimal CI: `go build ./...`, `go vet ./...`, `gofmt -l`. No cluster in unit
   CI — e2e runs live in consumers.
4. A registry namespace for the recorder image (see "Recorder" below), e.g.
   `ghcr.io/<org>/dockyard-recorder`.

## Module layout (L1 only)

Rename the current `config`/`funcs` packages to clearer public names during the
move (one-time mechanical churn):

```
dockyard/
  go.mod                  module github.com/<org>/dockyard
  dockyard.go             (pkg dockyard) Environment, Product interface, suites, labels
  install/                component installers + the Deps builder
    deps.go               Deps: NewDeps().WithMetalLB()/WithGatewayAPI()/WithIstio()
                          /WithCertManager()/WithKuadrant()/WithBuiltImage()/...
    metallb.go istio.go cert_manager.go kuadrant.go gatewayapi.go image.go crds.go
    command.go            delegation primitives: Helm(), Kubectl(), Script() (see below)
  feature/                feature helpers
    feature.go            AllOf, ApplyResources, Create/Delete, waits, GatewayClient
  gateway/                HTTP SDK (unchanged)
  sim/recorder/           the recorder sim (its own module; published as an image)
```

Source mapping from today:
- `test/e2e/config/{environment.go,product.go}` → `dockyard/dockyard.go` (pkg `dockyard`)
- `test/e2e/funcs/{env.go,deps.go}` → `dockyard/install/`
- `test/e2e/funcs/feature.go` → `dockyard/feature/`
- `test/e2e/gateway/` → `dockyard/gateway/`
- `test/e2e/sim/recorder/` → `dockyard/sim/recorder/`

## The Product contract (the seam)

Dockyard guarantees cluster lifecycle + suites/labels + the generic installers;
a consumer supplies a `Product`:

```go
type Product interface {
    InstallDeps(e *Environment) []env.Func  // compose via install.NewDeps()
    AddTypesToScheme() env.Func             // register the repo's CRD types
    Enabled() bool                          // run the control plane?
    Install() (setup []env.Func, teardown env.Func) // controller etc.
}
```

Write this contract down as the first thing in the new repo's README, and
sanity-check it against the hardest consumer (grid/#189: KServe + two serving
pools + grid operator) before over-investing. If grid needs something, add it to
the interface, don't special-case it in L1.

## Recorder: publish an image, don't build per-consumer

Today `Deps.WithBuiltImage` does a `docker build` of the recorder per run. For a
shared lib that's wrong — each consumer would need the recorder source. Instead:
- Dockyard CI builds + publishes `ghcr.io/<org>/dockyard-recorder:<tag>`.
- `install.WithRecorder(opts...)` references that published image (overridable
  tag/registry). No local build in consumers.
- Keep `WithBuiltImage` as a generic primitive for a consumer's own sims.

## Delegation primitive (avoid reimplementing product deploys)

So a consumer deploying a real product (MaaS via its `deploy.sh`, operators on
OpenShift) doesn't force Dockyard to re-encode it, L1 ships generic escape
hatches the `Deps` builder can compose:
- `install.Helm(release, chart, opts...)`
- `install.Kubectl(args...)` / `install.Manifest(path)`
- `install.Script(path, env...)`   # e.g. MaaS deploy.sh

This is the "two modes" safety valve: **lightweight** (Dockyard installs infra +
in-process controller) vs **full platform** (a product's own installer stands up
everything; Dockyard just drives trials). A consumer picks per `Deps` chain;
Dockyard never double-installs because the consumer composes it.

## Porting steps (mechanical, keep aigc green throughout)

1. `git mv` the L1 packages into the Dockyard repo; rename packages per layout;
   fix internal import paths.
2. `go build ./... && go vet ./...` in Dockyard. Tag `v0.0.1` (or just use a
   pseudo-version via replace).
3. In aigc `go.mod`: `require github.com/<org>/dockyard vX` and, during active
   co-development, `replace github.com/<org>/dockyard => ../../../dockyard`.
4. In aigc, update imports: `.../test/e2e/config` → `.../dockyard`,
   `.../funcs` → `.../dockyard/install` + `.../dockyard/feature`,
   `.../gateway` → `.../dockyard/gateway`, recorder image → published tag.
5. aigc keeps: `product/` (L2+L3), `*_test.go`, `consts.go`, `main_test.go`,
   `manifests/` (crds, cert-manager issuer, per-area YAML).
6. The L2 helpers in `product/` now call Dockyard's `install`/`feature` instead
   of the local `funcs`.

## Validation gate

The extraction is correct iff the aigc e2e suite is still green against Dockyard:

```
go test -tags=e2e -run 'TestBBR|TestProviderSelectionWeighted' \
  -destroy-cluster=false -cluster-name=aigc-debug -args -v=2   # expect: ok
```

Same tests, same fixtures, now importing Dockyard. No behavior change — pure move.

## After the first port

- **grid / praxis-extproc #189** implements a `Product` + its L2 (KServe,
  serving pools, grid operator) and its trials. Its needs stress-test the
  `Product` interface — adjust L1 only if genuinely generic.
- **gateway-controller** (praxis-proxy) adopts Dockyard from day one.
- Revisit extracting a shared **L2 `praxis-platform`** module once 2+ consumers
  share the same provisioning (service-ca stand-in, provider-HTTPS, kuadrant).
- OpenShift support = additional installer variants behind the same `Deps` API
  (detect platform; skip what the cluster already provides, e.g. service-ca).

## Open decisions

- Org / module path (`opendatahub-io` vs `praxis-proxy`).
- Recorder image registry + tag scheme.
- Public package names (this plan proposes `dockyard` / `install` / `feature` /
  `gateway` / `sim`).
- Whether L2 extracts now or waits for the 2nd consumer (recommend: wait).
