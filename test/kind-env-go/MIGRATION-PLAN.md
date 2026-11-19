# kind-env → dockyard (Go) migration plan

A staged rewrite of the `test/kind-env` shell+Python qualification harness
(~30k LOC) into a Go e2e suite built on **dockyard**. This is the roadmap; each
stage is an independently reviewable PR. The old `test/kind-env` suite stays
authoritative until a stage reaches parity and is cut over.

## Why staged (read first)

The harness is not one program — it is a platform provisioner plus several
independent qualification suites, each with real domain logic (statistical
distribution tests, an Anthropic/OpenAI translation matrix, credential-injection
proofs, MaaS source builds + provenance). A big-bang rewrite is unreviewable and
unverifiable: it builds **6 external source repos** (praxis-extproc, praxis-ai,
maas-controller, kuadrant-operator, llm-katan, kserve) that must be present to run.
So we migrate provisioning first (the shared foundation every suite needs), then
port one suite at a time, validating each against a real cluster before the next.

## Source inventory (what we are replacing)

| Source | LOC | Stage |
|--------|-----|-------|
| `run.sh` (provision/destroy, image builds, platform deploy) | 1638 | 1 |
| `e2e.sh` + `e2e-extproc.sh` (core routing/extproc qualification) | ~686 | 2 |
| `e2e-static-provider-weights.py` (weights + chi-square + withdrawal) | 2277 | 3 |
| `e2e-equal-provider-selection.py` (affinity + staged A+B→A) | 2041 | 4 |
| `e2e-api-translation.py` (Anthropic/OpenAI matrix, body-size, SSE) | 7044 | 5 |
| `e2e-ipp-translation-migration.py` (IPP cutover/rollback) | 973 | 6 |
| credential/namespace shell tests + `*contract*.py`/`test_*.py` | ~1300 | 7 |
| `demo.sh`, evidence/provenance plumbing | ~1100 | cross-cutting |

## Layering (where each piece lands)

- **L1 — dockyard.** Already provides: cluster lifecycle, Gateway API,
  cert-manager, istio, kuadrant, CRD/manifest apply, image load, gateway HTTP SDK.
  Likely *new* generic additions this migration justifies:
  - `install.LocalRegistry(...)` — stand up + wire a kind-reachable local registry
    (run.sh needs it because MaaS reconciles with `imagePullPolicy: Always`).
  - `install.Kustomize(dir)` — render+apply a kustomize overlay in-process
    (sigs.k8s.io/kustomize/api) — several MaaS manifests are kustomize.
  - `install.BuildImageFrom(context, dockerfile, tag)` — build with an explicit
    Containerfile path (run.sh builds ext-proc from `praxis-extproc/Containerfile`).
  - a small `gateway`/stats test helper package for goodness-of-fit (chi-square)
    so Stage 3/4 don't reinvent it. (Candidate for L1 `feature` or a `feature/stats`.)
- **L2 — praxis-platform (new, shared later).** The MaaS/Katan/Authorino
  provisioning conventions (service-ca stand-in, run-owned CA, AuthPolicy, the
  `MaasTenantConfig` annotation handoff). Keep in aigc for now; extract once a 2nd
  consumer needs it.
- **L3 — aigc `test/kind-env-go`.** The `MaaSPlatform` Vessel, CR fixtures, the
  per-suite Features, and the source-repo build/provenance logic (inherently
  product-specific).

## Stage 1 — provisioning Vessel (replaces run.sh)

A `MaaSPlatform` dockyard `Vessel` whose `InstallDeps`/`Install` reproduce run.sh's
ordered provisioning. Faithful order (run.sh:578–840+):

1. local registry (+ kind network) → `install.LocalRegistry`
2. cluster create → `Environment.CreateClusterFunc`
3. build images (ai-gateway-controller, katan, provider-recorder, ext-proc,
   maas-api, maas-controller) from the 6 source checkouts → `BuildImageFrom`/Script,
   push to the local registry; record provenance (L3)
4. `kind load` the non-registry images → `install.WithImage`
5. Gateway API → `install.InstallGatewayAPI(GatewayAPIVersion("v1.5.1"))`
6. cert-manager → `install.Helm(HelmChart{…jetstack/cert-manager v1.15.3…})`
7. kuadrant → `install.InstallKuadrant` (already helm-SDK) + operator restart
8. namespaces (maas-system, opendatahub, models-as-a-service, ai-tenant-tenant-b,
   ai-tenant-transition, ai-tenants) → feature/CRD apply
9. database + gateway manifests, MaaS kuadrant networking → `Manifest`
10. KServe CRDs (server-side apply) → `install.InstallCRDs` (needs an SSA option)
11. run-owned certs/CA (openssl today → Go crypto/x509) + TLS secrets + Authorino CA
12. MaaS controller render (kustomize) + apply; MaaS API RBAC (kustomize)
13. tenant fixtures (tenant A/B/transition), `MaasTenantConfig` annotation handoff
    wait (run.sh `wait_for_praxis_handoff`) → `feature` waits
14. ext-proc tenant deployments + Kind security identity patch (uid 65532)

Deliverable: `MaaSPlatform` compiles and provisions a cluster when the source
repos are present. Validation: `--provision` parity vs run.sh (cluster comes up,
same resources exist). Destroy via `Environment` teardown (ownership-checked).

## Stage 2 — core routing / extproc suite (e2e.sh + e2e-extproc.sh)

Port the numbered routing assertions to Go `Features`: transport, routing,
hot-reload, digest/last-known-good, provider status-gate loss/recovery, provider
CA trust via `tls.credentialName`, tenant-B isolation. Reuse the dockyard
`gateway` SDK for requests. This is the base qualification (`e2e.sh`).

## Stage 3 — static provider weights + stats (e2e-static-provider-weights.py)

Three credential-enforcing HTTPS recorders; 50:50 / 50:25:25 / 95:5 traffic
samples (500 req) with Pearson goodness-of-fit at α=0.001; 100:0↔0:100 admin
switch; 2:1:1 ratio-equivalence; all-zero withdrawal scenarios (ready/unready/
missing/healthy-sibling). Needs the chi-square helper (L1 candidate) and the
overlay-convergence gate (published/mounted/accepted/serving digests agree).

## Stage 4 — equal-provider selection + withdrawal (e2e-equal-provider-selection.py)

One-provider routing, two 100-req A/B samples, session affinity, staged A+B→A
transition with drain. Requires clean-`main` ext-proc provenance gates (L3).

## Stage 5 — API translation (e2e-api-translation.py, 7044 LOC)

The largest piece; itself multi-sub-stage: default-off passthrough, OpenAI SSE
route, Anthropic request/response translation matrix, `max_completion_tokens`/
`max_tokens` precedence, image-block conversion, body-size boundary matrix
(1.5/10 MiB + 64 KiB accumulator, HTTP 413 attribution), streaming. Plus the
`test_api_translation_contract.py` mounted-limit parser → Go unit tests.

## Stage 6 — IPP cutover/rollback (e2e-ipp-translation-migration.py)

The explicit-`ipp` `MaasTenantConfig` transition fixture; switch-away cleanup of
the IPP-owned HTTPRoute gated on owner-UID proof.

## Stage 7 — credential / namespace boundary + contracts

`test-provider-credentials.sh`, `test-namespace-boundary.sh`,
`test_run_contracts.sh`, `test_migration_configmap_cleanup.py`,
`api_translation_contract.py` → Go Features and (where cluster-free) Go unit tests.

## Cross-cutting decisions (resolve before/with Stage 1)

- **Evidence/provenance:** keep the JSON evidence contract, or drop it for Go
  test output + artifacts? (Affects every stage.) Recommend: a thin `evidence`
  helper that writes the same JSON shapes, so CI/consumers aren't broken.
- **Source-repo builds:** keep building the 6 repos from checkouts (provenance
  gates), or consume published images only? Recommend: support both, default to
  published images in CI and source builds behind flags (mirrors run.sh modes).
- **Statistics:** port the exact Pearson cutoffs (α=0.001, family-wise ≤0.003);
  a wrong port silently weakens the qualification. Golden-test the helper.
- **What stays shell/Python:** none long-term, but the opt-in *real OpenAI*
  single-paid-request diagnostic (README) is manual and low-value to port early.

## Validation gate (per stage)

A stage is done when its Go Features pass on a freshly `MaaSPlatform`-provisioned
kind cluster and produce the same pass/fail verdict as the shell/Python original
on the same scenario. The old suite is removed only after all stages reach parity.
