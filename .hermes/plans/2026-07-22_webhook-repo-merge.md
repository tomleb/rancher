# Merge rancher/webhook into rancher/rancher — Implementation Plan

> **For Hermes:** Use subagent-driven-development skill to implement this plan task-by-task.
> **Context:** This plan is a hand-off; the implementing agent has none of the prior
> discussion. This is Option A ("full merge") from a design discussion that also considered
> Option B (split: keep a thin framework library in `rancher/webhook`, move only
> `pkg/resources` validators/mutators into `rancher/rancher`) and rejected it — see
> "Why full merge, not a framework/impl split" below. Do not silently switch to Option B
> without flagging it back to a human; the two are materially different amounts of work and
> different long-term maintenance shapes.
>
> **Relationship to the other plan in this directory
> (`2026-07-22_webhook-embedded-chart.md`):** That plan changes *how the webhook Helm chart
> is deployed* (bundled into the rancher/rancher-agent images, no ClusterRepo). This plan
> changes *where webhook's source code and release process live* (merged into
> `rancher/rancher`, one go.mod, one release cadence). They are independent and can land in
> either order, but landing this one FIRST makes the embedded-chart plan's Task 8 (Docker
> build stage that clones and packages the webhook chart from an external repo) unnecessary
> — if webhook's `charts/rancher-webhook` source already lives inside `rancher/rancher`,
recommend re-reading the embedded-chart plan's Task 8 once this merge lands, since the
> cross-repo `git clone` step it describes becomes a local `helm package` on an in-tree
> directory instead. Flag this simplification to a human rather than silently rewriting the
> other plan.

**Goal:** Eliminate the `rancher/webhook` repository and Go module as an independent
release/dependency unit. Move all of its code (`pkg/resources` validators/mutators, the
admission-webhook framework, generated controllers, and the binary entrypoint) into
`rancher/rancher`, so that CVE/dependency bumps and API-type changes affecting webhook logic
happen in a single PR, in a single go.mod, on a single release cadence — directly addressing
https://github.com/rancher/rancher/issues/56127.

**Architecture:** `rancher/webhook`'s `pkg/*` tree is imported into `rancher/rancher` under
a new `pkg/webhook/` namespace (avoiding a name collision with rancher's existing
`pkg/auth`, `pkg/server`, `pkg/resources`, etc. — see Task 2). The webhook binary becomes a
new `cmd/webhook/main.go` entrypoint built into (or alongside) the existing rancher images,
so the `rancher-webhook` Kubernetes Deployment can still run as a distinct process/image
without a separate repository, module, or release pipeline behind it. All 111 files in
webhook that today import `rancher/rancher/pkg/apis/...` across a module boundary switch to
plain in-repo imports — the entire reason this fixes the dependency-sync problem.

**Tech Stack:** Go 1.26, Docker multi-stage build, `git subtree` (recommended for history
preservation) or plain file copy for the migration itself.

---

## Why full merge, not a framework/impl split (Option B)

Measured against `rancher/webhook` @ `upstream/main`, July 2026 (non-test Go LOC):

| Area | LOC | Imports `rancher/rancher`? |
|---|---|---|
| `pkg/resources/*` (validators + mutators) | 9,555 | Yes (111 files) |
| `pkg/generated/*` (wrangler-generated controllers/objects) | 4,960 | No (generated from CRDs) |
| Framework: `pkg/admission`, `pkg/server`, `pkg/auth`, `pkg/resolvers`, `pkg/podsecurityadmission`, `pkg/clients`, `pkg/health`, `pkg/patch`, `pkg/mocks` | ~2,097 | No |

The framework is genuinely decoupled today — zero imports of `rancher/rancher` — and has a
single, clean registration seam: `pkg/server/handlers.go` is the only file in the framework
that imports `pkg/resources/*` (one big import list + registration function handing
validators/mutators to the framework's `Validators`/`Mutators` slices).

Despite that clean seam, full merge was chosen over a framework/library split because:

1. **The framework has exactly one consumer.** Splitting it into a real "library" means
   committing to a stable public API (the `Validator`/`Mutator`/registration interfaces),
   its own versioning, and its own release process — real ongoing cost — for a boundary
   nobody outside `rancher/rancher` currently needs. That's speculative infrastructure for a
   theoretical second consumer that doesn't exist.
2. **The dependency-sync pain the issue describes isn't solved by a partial split.** As long
   as *any* piece of webhook lives in a separate go.mod, some cross-repo version-bump PR
   dance survives — smaller in volume (framework code changes rarely, per your own
   observation), but the release-week ceremony (tag, cross-repo bump PR, wait for CI, merge)
   is a fixed cost per release regardless of how much code triggers it. Full merge removes
   the ceremony entirely, not just shrinks its trigger frequency.
3. **The CI-time worry (long rancher CI, webhook framework rarely changes) is a
   CI-configuration problem, not an argument for a repo boundary.** Go's build/test caching
   already avoids re-testing packages whose dependency graph didn't change; if this turns
   out to matter in practice post-merge, it's solvable with path-based CI job filtering
   inside one repo (see Task 8's note on this), without paying the cross-repo-module tax
   for it.

If, after reading this, the decision changes to Option B, this plan does NOT apply as
written — flag that back to a human for a fresh plan rather than improvising a partial
version of these tasks.

---

## IMPORTANT UPDATE (post-write verification): webhook's pkg/generated is ~70% redundant

Verified directly against both repos' `pkg/generated/controllers/` trees (file-by-file
`comm -23` diff plus byte-level `diff` on sampled files):

| webhook's `pkg/generated/` subtree | LOC | Status |
|---|---|---|
| `controllers/{management,provisioning,catalog,rke}.cattle.io/...` | 3,456 | **Fully redundant.** `comm -23` (files in webhook not in rancher) returned EMPTY for all four groups — every filename webhook generates already exists in `rancher/rancher/pkg/generated/controllers/...`. Sampled `cluster.go`, `setting.go`, `feature.go` under `management.cattle.io/v3`: byte-identical except the `// Code generated by codegen` vs `// Code generated by main` header comment left by each repo's own codegen tool invocation. Rancher's generated tree is a strict superset (it covers 10+ additional API groups webhook never touches, e.g. `fleet.cattle.io`, `cluster.x-k8s.io`, `upgrade.cattle.io`). |
| `objects/*` (`core`, `provisioning.cattle.io`, `rbac.authorization.k8s.io`, `autoscaling`, `management.cattle.io`, `auditlog.cattle.io`, `rke.cattle.io`, `catalog.cattle.io`) | 1,504 | **NOT redundant — genuinely webhook-specific, must move.** This is not wrangler controller codegen at all; it's typed helpers for extracting old/new objects out of `admissionv1.AdmissionRequest` (e.g. `UnstructuredOldAndNewFromRequest`). Rancher has no equivalent directory or concept. 36 internal call sites in webhook depend on it. |

**Action this implies, overriding the generic Task 2/3/6 instructions below wherever they
conflict:**
- Do **NOT** copy `pkg/generated/controllers/{management,provisioning,catalog,rke}.cattle.io`
  into `pkg/webhook/generated/controllers/...` at all. Skip them entirely in the Task 3
  move.
- In the ~90 files under `pkg/webhook/resources/...` (and any elsewhere) that import
  `github.com/rancher/webhook/pkg/generated/controllers/{management,provisioning,catalog,rke}.cattle.io/...`,
  rewrite those imports to point at
  `github.com/rancher/rancher/pkg/generated/controllers/{management,provisioning,catalog,rke}.cattle.io/...`
  (rancher's EXISTING tree) instead of creating a `pkg/webhook/generated/...` equivalent.
  This is a different rewrite target than the generic sed pattern in Task 3 Step 2 — add a
  dedicated sed pass for these four specific import prefixes pointing at rancher's own
  `pkg/generated`, not `pkg/webhook/generated`.
- Only `pkg/generated/objects/...` actually moves, to `pkg/webhook/generated/objects/...`.
- Confirm before executing whether any OTHER webhook controller group not covered by these
  4 (unlikely, given webhook's `pkg/generated/controllers` only had these 4 subdirectories
  total per the investigation) needs the same treatment — re-run the `comm -23` check
  against whatever the current state of both repos is at execution time, since both may
  have drifted since this plan was written.
- This reduces Task 3's move scope by ~3,456 LOC and Task 6's codegen-coexistence scope
  correspondingly (webhook's own codegen no longer needs to regenerate those 4 groups at
  all post-merge — only whatever produces `objects/*` needs to keep running, and even that
  should be checked: 8 files / 1,504 LOC has the shape of hand-maintained helpers as much
  as true generated output despite living under `pkg/generated/` and carrying a "DO NOT
  EDIT" header — confirm what actually (re)generates it, if anything, before assuming a
  live codegen dependency needs porting for this subtree too).

---

## Key structural findings (traced directly, not assumed)

- **`rancher/rancher` already has top-level packages that collide by name** with several of
  webhook's: `pkg/auth` (rancher's is large — accessor, providers, tokens, requests, etc.),
  `pkg/server`, `pkg/resources`, `pkg/health`, `pkg/patch`, `pkg/clients`. Webhook's code
  CANNOT land at `rancher/rancher/pkg/<same-name>` — it needs its own namespace. This plan
  uses `pkg/webhook/...` as that namespace (e.g. `pkg/webhook/resources`,
  `pkg/webhook/admission`, `pkg/webhook/server`, `pkg/webhook/auth`, ...). Confirm this
  namespace choice doesn't collide with anything already under `pkg/webhook/` in
  `rancher/rancher` (none found as of this writing — `search_files` for `pkg/webhook`
  target=files turned up nothing pre-merge).
- **`rancher/rancher` is architected as (at least) two binaries from one module today**:
  root `main.go` (the rancher server) and `cmd/agent/main.go` (the downstream cluster
  agent). This is the existing precedent for "multiple entrypoints, one go.mod, one
  release" — exactly the shape webhook needs to fit into. Webhook becomes a third
  entrypoint: `cmd/webhook/main.go`, a near-verbatim copy of webhook's current `main.go`
  (which is only 43 lines, trivial to port) but importing `pkg/webhook/server` instead of a
  separate module's `pkg/server`.
- **Webhook's Dockerfile builds a genuinely separate, minimal image** (`FROM scratch AS
  binary`, `COPY --from=webhook-build /dist/webhook /webhook`) — the `rancher-webhook`
  Kubernetes Deployment runs this dedicated image, not the full rancher-server image. This
  needs to be preserved or deliberately changed (see Task 7) — the merge does not need to
  mean "webhook now runs inside the rancher-server pod"; it can still ship as its own
  container image built from the same monorepo, same way `cmd/agent` produces a separate
  `rancher-agent` image from the same `rancher/rancher` source tree today.
- **`main.go`'s `//go:generate` directives** (`go run pkg/codegen/cleanup/main.go`, `go run
  ./pkg/codegen`) drive webhook's own CRD-type codegen (`pkg/generated/*`). Rancher has its
  own `pkg/codegen` already (per `scripts/go-generate` referenced in the
  `rancher-webhook-systemcharts` skill notes) — these two codegen setups need to coexist
  without collision (separate `//go:generate` invocations targeting separate output trees
  is fine; just don't merge the codegen configs themselves in this pass).

---

## Files/directories likely to change or be created

- **New:** `pkg/webhook/resources/...` (from webhook's `pkg/resources/`)
- **New:** `pkg/webhook/admission/...`, `pkg/webhook/server/...`, `pkg/webhook/auth/...`,
  `pkg/webhook/resolvers/...`, `pkg/webhook/podsecurityadmission/...`,
  `pkg/webhook/clients/...`, `pkg/webhook/health/...`, `pkg/webhook/patch/...`,
  `pkg/webhook/mocks/...` (from webhook's corresponding top-level dirs)
- **New:** `pkg/webhook/generated/...` (from webhook's `pkg/generated/`)
- **New:** `pkg/webhook/codegen/...` (from webhook's `pkg/codegen/`, kept separate from
  rancher's own `pkg/codegen`)
- **New:** `cmd/webhook/main.go` (ported from webhook's `main.go`)
- **New:** `charts/rancher-webhook/` (webhook's `charts/rancher-webhook`, moved in — feeds
  the embedded-chart plan)
- **Modify:** `go.mod`/`go.sum` — add webhook's non-`rancher/rancher` dependencies (e.g.
  `github.com/go-ldap/ldap/v3`, `github.com/robfig/cron`, `sigs.k8s.io/cluster-api-provider-aws/v2`
  — cross-check the full list per Task 3), remove the `github.com/rancher/webhook` and
  `github.com/rancher/rancher/pkg/apis`/`pkg/plan` replace/require entries that existed
  purely to let webhook build against rancher (webhook's own go.mod required
  `rancher/rancher/pkg/apis` and `rancher/rancher/pkg/plan` — those become plain in-repo
  imports and vanish from go.mod entirely).
- **Modify:** `package/Dockerfile` — new build stage/target producing the webhook binary
  image (see Task 7).
- **Modify:** `pkg/settings/setting.go`, `pkg/buildconfig/constants.go` (generated),
  `scripts/export-config`, `build.yaml` — remove/retire `RancherWebhookVersion` and
  `CATTLE_RANCHER_WEBHOOK_VERSION` plumbing IF webhook's binary version now simply equals
  rancher's own version (open question, flagged in Task 9 — do not silently rip this out
  without confirming the systemcharts controller's version-pinning logic doesn't still need
  an independent value for chart-version comparison purposes, particularly if the
  embedded-chart plan's `system.Manager.install()` version-check logic is still in play).
- **Remove (end state, not this plan's job):** the `rancher/webhook` repository itself stops
  receiving new code — out of scope here to actually archive/deprecate it; that's a
  post-merge decision for the team once the cutover is validated across one full release
  cycle.

---

## Step-by-step plan

### Task 1: Inventory webhook's full dependency list against rancher's go.mod

**Objective:** Before moving any code, know exactly which of webhook's `go.mod` requires
are already satisfied by `rancher/rancher`'s go.mod (same or compatible version) and which
are net-new additions.

**Files:** none changed yet — this is an analysis task producing a checklist for Task 3.

**Step 1: Dump both go.mod require lists**

Run (in the `webhook-migration` worktree, both repos checked out):
```sh
cd webhook && go list -m all > /tmp/webhook-deps.txt
cd ../rancher && go list -m all > /tmp/rancher-deps.txt
diff <(sort /tmp/webhook-deps.txt) <(sort /tmp/rancher-deps.txt) > /tmp/dep-diff.txt
```

**Step 2: Review `/tmp/dep-diff.txt`** and produce a short list (paste into a scratch note
or the PR description later) of:
- Deps webhook needs that rancher doesn't have at all (net-new additions to rancher's
  go.mod) — from the go.mod read earlier in this investigation, at minimum expect:
  `github.com/go-ldap/ldap/v3`, `github.com/robfig/cron`,
  `sigs.k8s.io/cluster-api-provider-aws/v2`, `github.com/rancher/jsonpath`,
  `github.com/rancher/dynamiclistener` (check if rancher already has this one — likely
  yes, given rancher itself uses dynamiclistener for its own TLS listener setup — verify,
  don't assume).
- Deps both need but at different versions — flag any k8s.io/* version mismatches
  specifically, since both repos pin `k8s.io/* v0.36.2` per the go.mod's already read in
  this conversation; a mismatch here is the most likely source of build breakage.

**Step 3: No commit** — this task produces analysis notes only, carry them into Task 3.

---

### Task 2: Decide and document the `pkg/webhook/*` namespace mapping

**Objective:** Produce an explicit old-path → new-path mapping so the code move (Task 3) is
mechanical, not improvised.

**Files:** Create `docs/webhook-merge-path-mapping.md` (or embed the table directly in the
PR description — implementer's choice, but it must exist somewhere reviewable).

**Step 1: Write the mapping table**

| webhook path (old) | rancher/rancher path (new) |
|---|---|
| `pkg/resources/` | `pkg/webhook/resources/` |
| `pkg/admission/` | `pkg/webhook/admission/` |
| `pkg/server/` | `pkg/webhook/server/` |
| `pkg/auth/` | `pkg/webhook/auth/` |
| `pkg/resolvers/` | `pkg/webhook/resolvers/` |
| `pkg/podsecurityadmission/` | `pkg/webhook/podsecurityadmission/` |
| `pkg/clients/` | `pkg/webhook/clients/` |
| `pkg/health/` | `pkg/webhook/health/` |
| `pkg/patch/` | `pkg/webhook/patch/` |
| `pkg/mocks/` | `pkg/webhook/mocks/` |
| `pkg/generated/` | `pkg/webhook/generated/` |
| `pkg/codegen/` | `pkg/webhook/codegen/` |
| `main.go` | `cmd/webhook/main.go` |
| `charts/rancher-webhook/` | `charts/rancher-webhook/` (top-level, unchanged — no
  collision, rancher's own chart is at `chart/` singular, not `charts/`) |
| `tests/integration/` | `pkg/webhook/tests/integration/` (or `tests/webhook/integration/`
  if rancher's own `tests/` layout expects a specific structure — check
  `rancher/rancher/tests/` conventions before deciding) |

**Step 2: Confirm no collisions** by running, in the target rancher worktree:
```sh
for p in resources admission server auth resolvers podsecurityadmission clients health patch mocks generated codegen; do
  test -d "pkg/webhook/$p" && echo "COLLISION: pkg/webhook/$p already exists" || true
done
```
Expected: no output (no collisions), since `pkg/webhook/` shouldn't exist yet pre-merge.

**Step 3: Commit the mapping doc**

```bash
git add docs/webhook-merge-path-mapping.md
git commit -s -m "docs: add webhook->rancher path mapping for repo merge"
```

---

### Task 3: Move webhook's source tree into rancher, rewrite imports

**Objective:** Physically copy webhook's `pkg/*` (per Task 2's mapping) into
`rancher/rancher`, and rewrite every `import` referencing the old module path.

**Files:** All files listed in Task 2's mapping table (as new files under `pkg/webhook/...`
and `cmd/webhook/main.go`).

**Step 1: Preserve history with `git subtree` (recommended over a plain copy)**

From the `rancher/rancher` worktree, with the webhook repo added as a remote:
```sh
git remote add webhook-src /srv/docker/volumes/hermes/hermes.workspace/.bare/webhook.git
git fetch webhook-src upstream/main
git subtree add --prefix=pkg/webhook-import webhook-src/upstream/main --squash
```
This lands webhook's entire tree (including `main.go`, `charts/`, `tests/`, everything) at
`pkg/webhook-import/` in one commit, preserving a reference to webhook's history in the
commit message. Then use `git mv` to redistribute files per Task 2's mapping table (e.g.
`git mv pkg/webhook-import/pkg/resources pkg/webhook/resources`, `git mv
pkg/webhook-import/main.go cmd/webhook/main.go`, `git mv pkg/webhook-import/charts
charts-webhook-tmp && git mv charts-webhook-tmp/rancher-webhook charts/rancher-webhook &&
rm -rf charts-webhook-tmp`), then remove the now-empty `pkg/webhook-import/` directory.

If `git subtree` is unavailable or the history-preservation isn't valued, a plain
`cp -r` + `git add` accomplishes the same file layout without the history — acceptable
fallback, note the choice in the PR description either way.

**Step 2: Rewrite import paths across the moved files**

Every moved `.go` file needs `github.com/rancher/webhook/pkg/X` rewritten to
`github.com/rancher/rancher/pkg/webhook/X` (per the Task 2 mapping). Use a scripted
find-and-replace rather than manual editing, given the file count:

```sh
cd rancher  # repo root
find pkg/webhook cmd/webhook -name "*.go" -print0 | xargs -0 sed -i \
  -e 's#github.com/rancher/webhook/pkg/resources#github.com/rancher/rancher/pkg/webhook/resources#g' \
  -e 's#github.com/rancher/webhook/pkg/admission#github.com/rancher/rancher/pkg/webhook/admission#g' \
  -e 's#github.com/rancher/webhook/pkg/server#github.com/rancher/rancher/pkg/webhook/server#g' \
  -e 's#github.com/rancher/webhook/pkg/auth#github.com/rancher/rancher/pkg/webhook/auth#g' \
  -e 's#github.com/rancher/webhook/pkg/resolvers#github.com/rancher/rancher/pkg/webhook/resolvers#g' \
  -e 's#github.com/rancher/webhook/pkg/podsecurityadmission#github.com/rancher/rancher/pkg/webhook/podsecurityadmission#g' \
  -e 's#github.com/rancher/webhook/pkg/clients#github.com/rancher/rancher/pkg/webhook/clients#g' \
  -e 's#github.com/rancher/webhook/pkg/health#github.com/rancher/rancher/pkg/webhook/health#g' \
  -e 's#github.com/rancher/webhook/pkg/patch#github.com/rancher/rancher/pkg/webhook/patch#g' \
  -e 's#github.com/rancher/webhook/pkg/mocks#github.com/rancher/rancher/pkg/webhook/mocks#g' \
  -e 's#github.com/rancher/webhook/pkg/generated#github.com/rancher/rancher/pkg/webhook/generated#g' \
  -e 's#github.com/rancher/webhook/pkg/codegen#github.com/rancher/rancher/pkg/webhook/codegen#g'
```

Note: the two `//go:generate` lines that were at the top of webhook's `main.go` (`go run
pkg/codegen/cleanup/main.go`, `go run ./pkg/codegen`) need their paths updated too when
ported to `cmd/webhook/main.go` — change to `go run
github.com/rancher/rancher/pkg/webhook/codegen/cleanup` and `go run
./pkg/webhook/codegen` respectively (verify exact invocation style against what actually
works once the files are in place — `go generate` path resolution is relative to the file
containing the directive, so `./pkg/webhook/codegen` needs correcting if `cmd/webhook/`
is now main.go's location, likely becoming `go run ../../pkg/webhook/codegen` or an
absolute-module-path invocation — test this explicitly rather than guessing, see Step 4).

**Step 3: Rewrite the ~111 files that imported `rancher/rancher/pkg/apis/...` and
`rancher/rancher/pkg/plan`** — these already point at the correct final module
(`github.com/rancher/rancher/...`), so NO rewrite is needed for these specific imports;
confirm this with:
```sh
grep -rl "github.com/rancher/rancher/pkg/apis\|github.com/rancher/rancher/pkg/plan" pkg/webhook/ | wc -l
```
Expected: ~111 (matches the count found during the earlier investigation). These lines
require zero changes — this is the entire point of the merge (they were already
"pre-written" as if webhook were part of rancher; now they simply resolve within the same
module instead of across a module boundary).

**Step 4: Attempt a build**

Run: `cd rancher && go build ./pkg/webhook/... ./cmd/webhook/...`
Expected: FAILS on the first pass — resolve iteratively:
- Missing go.mod deps (from Task 1's inventory) → `go get` each, or manually add to
  `require` block, then `go mod tidy`.
- Any remaining `github.com/rancher/webhook/...` import strings not covered by Step 2's
  sed list (e.g. test files importing internal test helpers by full path) → repeat the
  sed pass scoped to `_test.go` files too if Step 2 excluded them (it doesn't exclude
  them as written above, but double check).
- Package name collisions if any two moved packages both declared `package resources` (or
  similar) at different nesting depths clashing with something already in
  `pkg/webhook/...` — shouldn't occur since `pkg/webhook/` is entirely new, but verify.

Iterate until `go build ./pkg/webhook/... ./cmd/webhook/...` succeeds with no errors.

**Step 5: Run webhook's existing unit tests in their new location**

Run: `go test ./pkg/webhook/... -v 2>&1 | tail -100`
Expected: same pass/fail profile as running `go test ./...` inside the original
`rancher/webhook` repo pre-merge (run that once beforehand as a baseline to diff against —
do this as part of Task 1 if not already captured). Investigate and fix any NEW failures
introduced by the move (e.g. hardcoded relative paths in tests assuming the old repo root).

**Step 6: Commit**

```bash
git add pkg/webhook/ cmd/webhook/ charts/rancher-webhook/ go.mod go.sum
git commit -s -m "webhook: merge rancher/webhook source tree into rancher/rancher under pkg/webhook"
```
(A single large commit is acceptable here given this is fundamentally one atomic move: use
`git commit -s` with a detailed body listing the source repo/commit merged from, for
traceability — reference the exact webhook commit SHA that was merged.)

---

### Task 4: Wire `cmd/webhook/main.go` and verify it builds as a standalone binary

**Objective:** Confirm the ported entrypoint actually produces a working `webhook` binary
from the merged monorepo, exactly as `rancher/webhook`'s own `main.go` did before.

**Files:** `cmd/webhook/main.go` (already created/moved in Task 3 — this task is
verification + build wiring, not new code, unless Task 3 Step 4 surfaced issues specific to
this file).

**Step 1: Confirm `cmd/webhook/main.go` content matches webhook's original `main.go`**
except for import path rewrites (Task 3, Step 2/3). It should still be ~43-59 lines,
importing `github.com/rancher/rancher/pkg/webhook/server` and calling
`server.ListenAndServe(ctx, cfg, os.Getenv("ENABLE_MCM") != "false")`.

**Step 2: Build the binary**

Run: `cd rancher && go build -o /tmp/webhook-test-binary ./cmd/webhook`
Expected: success, produces an executable.

**Step 3: Smoke-test the binary starts** (it will fail past the k8s connection step without
a real cluster, which is fine — confirm it fails at the *expected* point, not on an import
or panic):
```sh
KUBECONFIG=/nonexistent /tmp/webhook-test-binary
```
Expected output: an error about failing to load kubeconfig / connect to a cluster — NOT a
Go panic, NOT a "package not found" style error. This confirms the binary's internal wiring
(flag parsing, logging setup, server construction up to the point of needing a real
cluster) is intact post-merge.

**Step 4: Commit** (only if Step 1 required any fixes; otherwise this task produces no new
diff beyond what Task 3 already committed — skip commit if nothing changed).

---

### Task 5: Update rancher's root Go module metadata and dependency list

**Objective:** Finalize `go.mod`/`go.sum` so the merged module builds cleanly end-to-end,
and remove now-dead references to `github.com/rancher/webhook` as an external dependency.

**Files:** `go.mod`, `go.sum`

**Step 1: Remove the (now nonexistent) external webhook dependency, if rancher's go.mod
ever referenced `github.com/rancher/webhook` directly** (unlikely — the investigation found
no evidence rancher imports webhook's code — but check):
```sh
grep -n "rancher/webhook" go.mod go.sum
```
Expected: no matches (rancher never depended on webhook — only the reverse). If matches
exist, understand why before removing (could be an indirect dependency via some other
module — investigate, don't blindly delete).

**Step 2: Run `go mod tidy`**

Run: `go mod tidy`
Expected: cleanly resolves; review the diff to `go.mod`/`go.sum` for anything unexpected
(large unrelated version bumps signal a real conflict from Task 3's dependency merge, not
just added lines for webhook's own deps).

**Step 3: Full module build**

Run: `go build ./...`
Expected: success across the entire `rancher/rancher` module, not just the `pkg/webhook`
subtree — this is the real integration test that nothing else broke.

**Step 4: Commit**

```bash
git add go.mod go.sum
git commit -s -m "go.mod: finalize dependencies after webhook merge"
```

---

### Task 6: Fold webhook's `objects/*` generator into rancher's existing codegen

**Objective:** Rancher's repo-root `generate.go` already drives a single codegen pipeline:
```go
//go:generate go run pkg/codegen/buildconfig/writer.go pkg/codegen/buildconfig/chart_writer.go pkg/codegen/buildconfig/main.go
//go:generate go run pkg/codegen/generator/cleanup/main.go
//go:generate go run pkg/codegen/main.go
//go:generate scripts/build-crds
```
`pkg/codegen/main.go` calls `controllergen.Run(...)` once (covering all 14 of rancher's API
groups, including the 4 webhook also needs) plus a series of `generator.Generate*` calls for
norman/schema-based types. Webhook's `pkg/codegen/main.go` does the equivalent
`controllergen.Run(...)` call (now dead — see the earlier "pkg/generated is ~70% redundant"
section, that output is discarded) PLUS a second, genuinely unique step:
`generateObjectsFromRequest("pkg/generated/objects", groups)`, driven by a hand-written
generator (`pkg/codegen/template.go`'s `objectsFromRequestTemplate` + the
`generateObjectsFromRequest` function in `pkg/codegen/main.go`) that emits
`<Type>OldAndNewFromRequest(...)` helper functions per configured API type. THIS is what
needs to be ported — nothing else in webhook's codegen setup.

**PITFALL — do not port webhook's cleanup step as-is.** Webhook's
`pkg/codegen/cleanup/main.go` does:
```go
func main() {
	if err := os.RemoveAll("./pkg/generated"); err != nil { ... }
	...
}
```
This wipes webhook's entire (small, webhook-only) `pkg/generated` tree before every
regen — safe in webhook's own repo, because that's ALL that directory contains there. In the
merged monorepo, `pkg/generated` is rancher's own shared, much larger tree (clientset,
compose, norman, openapi, controllers for 14 groups). Reusing this cleanup step unmodified
would delete all of rancher's generated code on every `go generate` run. Rancher's own
`pkg/codegen/generator/cleanup/main.go` (already wired into `generate.go`'s second
`//go:generate` line) presumably already scopes its cleanup correctly for rancher's own
tree — READ that file before writing anything here, and either (a) confirm it already
covers whatever narrow subdirectory `objects/*` will live in and needs no changes, or
(b) extend it narrowly to also clean `pkg/webhook/generated/objects` specifically — never
reintroduce a bare `os.RemoveAll("./pkg/generated")`.

**Files:**
- Modify: `pkg/codegen/main.go` (rancher's, not webhook's) — add the
  `generateObjectsFromRequest` call and its supporting template.
- Create or modify: a new file alongside it (e.g. `pkg/codegen/objects_from_request.go`)
  carrying the `generateObjectsFromRequest` function + `objectsFromRequestTemplate` moved
  verbatim from webhook's `pkg/codegen/template.go`.
- Read (do not blindly edit without understanding first): `pkg/codegen/generator/cleanup/main.go`

**Step 1: Read rancher's existing cleanup generator** to understand its current scope:
```sh
read_file pkg/codegen/generator/cleanup/main.go
```
Confirm what it currently deletes/regenerates and whether extending it to also cover
`pkg/webhook/generated/objects` is a small, safe addition or requires a structural change.
Do not proceed to Step 2 until this is understood.

**Step 2: Port `generateObjectsFromRequest` + its template** into rancher's `pkg/codegen/`
as a new file (do not touch webhook's version — it will cease to exist once
`pkg/webhook/codegen/` is deleted per the earlier redundancy findings, since the
`controllergen.Run` half of webhook's codegen is discarded and this is the only remaining
piece). Call it from rancher's existing `pkg/codegen/main.go`'s `main()` function, after the
existing `controllergen.Run(...)` block, with output directory changed to
`pkg/webhook/generated/objects` and the exact same `groups` map webhook used (8 groups:
`catalog.cattle.io`, `management.cattle.io`, `provisioning.cattle.io`, `core`,
`autoscaling`, `rbac.authorization.k8s.io`, `auditlog.cattle.io`, `rke.cattle.io` — copy the
type list verbatim from webhook's `pkg/codegen/main.go` lines ~80-136, already captured in
full during this investigation). The API types referenced (`v3.Cluster`,
`catalogv1.ClusterRepo`, etc.) are the SAME types rancher's own `controllergen.Run` block
already imports from `github.com/rancher/rancher/pkg/apis/...` — reuse those imports rather
than re-importing.

**Step 3: Regenerate and diff against the pre-merge output**

Run: `go generate ./...` (or whatever the actual top-level invocation is — confirm via
`generate.go`'s directives, likely `go generate .` at repo root given the directives live
in `generate.go` at the root).
Then: `git diff --stat pkg/webhook/generated/objects/`
Expected: no diff (or only the boilerplate header differing, e.g. package path in a
generated comment) — confirms the ported generator produces byte-equivalent output to what
was moved in Task 3.

**Step 4: Confirm rancher's OWN generated output is untouched**

Run: `git status pkg/generated/` (rancher's original tree, NOT `pkg/webhook/generated`)
Expected: no changes. If anything under rancher's pre-existing `pkg/generated/` shows as
modified/deleted, STOP — this means Step 1's cleanup-scoping investigation was incomplete
and the cleanup generator is still too broad. Fix before proceeding.

**Step 5: Commit**

```bash
git add pkg/codegen/main.go pkg/codegen/objects_from_request.go pkg/webhook/generated/objects pkg/codegen/generator/cleanup/main.go
git commit -s -m "codegen: port webhook's objects/* generator into rancher's codegen pipeline"
```

---

### Task 7: Produce a `rancher-webhook` image from the merged monorepo

**Objective:** The `rancher-webhook` Kubernetes Deployment needs a container image built
from `cmd/webhook`. Decide and implement how that image gets built now that the source
lives in `rancher/rancher` instead of its own repo with its own Dockerfile/CI.

**Files:** `package/Dockerfile` (add a new stage/target), and whichever CI workflow builds
and pushes rancher's images (identify via `.github/workflows/` — not yet inspected in this
plan; the implementer should locate it before starting this task).

**Step 1: Add a webhook-binary build stage to `package/Dockerfile`**, modeled on webhook's
own former Dockerfile (`FROM ... AS build` → `COPY pkg/ ; COPY main.go` → `go build` →
`FROM scratch AS binary`), adapted to build from the now-shared source tree:

```dockerfile
FROM go-builder AS webhook-build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION
ARG COMMIT
COPY . .
RUN --mount=type=cache,target=/root/.cache,id=rancher \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags "-X main.Version=${VERSION} -X main.GitCommit=${COMMIT} -extldflags -static -s" \
    -o /dist/webhook ./cmd/webhook

FROM scratch AS webhook-binary
COPY --from=webhook-build /dist/webhook /webhook
```

(`go-builder` here refers to the existing `registry.suse.com/bci/golang:1.26.5 AS
go-builder` stage already present in `package/Dockerfile` — reuse it rather than
duplicating a build-base stage.)

Decide whether the final `rancher-webhook` image is:
- (a) a genuinely separate minimal image built via `docker build --target webhook-binary`
  and then wrapped in its own small final-stage Dockerfile (closest to webhook's original
  packaging — smallest image, cleanest separation), or
- (b) folded into the existing `rancher`/`rancher-agent` final images with the webhook
  binary just present alongside the others (simpler build graph, larger image size for
  images that don't need the webhook binary).

Recommendation: (a), to avoid bloating the main rancher/agent images with a binary most
of their runtime instances don't execute — but this is a real tradeoff to confirm with the
team, not a foregone conclusion; note it explicitly in the PR description.

**Step 2: Update whichever CI/release automation currently builds+pushes
`rancher/rancher-webhook`'s image** (was previously triggered by `rancher/webhook`'s own
`.github/workflows/release.yaml` — this workflow's responsibilities need to move into
`rancher/rancher`'s CI/release automation). Locate rancher's existing image-publishing
workflow(s) and add a job/step producing and pushing `rancher/rancher-webhook:<tag>` from
the new Dockerfile target, tagged in lockstep with rancher's own release tags (this is
explicitly the desired end state per the original issue — webhook releases in lockstep with
rancher, no separate tag scheme).

**Step 3: Local validation build**

Run: `docker build --target webhook-binary -t rancher-webhook-test -f package/Dockerfile .`
Expected: succeeds, produces a runnable image; smoke-test with
`docker run --rm rancher-webhook-test` and confirm it fails at the "no kubeconfig" point,
same as Task 4 Step 3's binary-level check.

**Step 4: Commit**

```bash
git add package/Dockerfile .github/workflows/  # or wherever CI config lives
git commit -s -m "package: build rancher-webhook image from merged monorepo source"
```

---

### Task 8: Decide fate of `RancherWebhookVersion`/`CATTLE_RANCHER_WEBHOOK_VERSION` plumbing

**Objective:** Once webhook's binary version is inherently tied to rancher's own version
(same repo, same tag), the entire independent-version-tracking plumbing
(`pkg/settings/setting.go`'s `RancherWebhookVersion`, `build.yaml`'s `webhookVersion:`
field, `CATTLE_RANCHER_WEBHOOK_VERSION` env var chain) may become partially or fully
redundant. This task is explicitly a DECISION point, not a mechanical task — do not delete
this plumbing without confirming downstream consumers.

**Files:** `pkg/settings/setting.go`, `build.yaml`, `scripts/export-config`,
`pkg/controllers/dashboard/systemcharts/controller.go` (its `watchedSettings` map watches
`settings.RancherWebhookVersion.Name`), `pkg/buildconfig/constants.go` (generated).

**Step 1: Determine whether `RancherWebhookVersion` is still needed as an independently
settable value.** If the embedded-chart plan (`2026-07-22_webhook-embedded-chart.md`) has
also landed by this point, the systemcharts controller's `install()` still does a
version-comparison against a chart index (now the bundled/embedded one) — that comparison
still needs *some* version string. The simplest post-merge answer is likely: "webhook's
chart version now always equals rancher's own release version" (no independent bump
possible or needed), which would let `RancherWebhookVersion`'s value collapse to always
mirror `settings.ServerVersion` — but confirm this doesn't break any user-facing behavior
(e.g. if any tooling/support workflows read `rancher-webhook-version` expecting a
webhook-specific semver like `0.11.0-rc.24` rather than rancher's own version string like
`v2.16.0`).

**Step 2: If keeping a separate value makes sense, at minimum simplify the plumbing** —
`CATTLE_RANCHER_WEBHOOK_VERSION`'s env-var round-trip through `scripts/export-config` →
Docker build-arg → `pkg/settings` runtime env lookup existed specifically to bridge two
separate build processes (rancher's and webhook's). With one build process, this can likely
become a plain build-time constant baked via `pkg/buildconfig` (the same mechanism already
used for `WebhookVersion` in `pkg/buildconfig/constants.go`, though note from the earlier
investigation: that generated constant currently has NO `setting.go` consumer — this would
be the first real one, or the decision could be "keep the env var machinery as-is, it still
works, don't touch it" if there's no clear win to simplifying it).

**Step 3: Whatever is decided, update the `rancher-webhook-systemcharts` skill's
documentation** (`skill_manage(action='patch', name='rancher-webhook-systemcharts', ...)`)
to reflect the new reality — that skill currently documents the OLD two-hop
webhook-repo/rancher-charts publish pipeline and the env-var version-plumbing chain in
detail; both sections will be stale once this merge lands and need a rewrite, not just an
addendum.

**Step 4: No default commit for this task** — the concrete diff depends entirely on the
Step 1 decision; implement per whatever was decided, following the same TDD/commit
discipline as other tasks once a concrete direction is chosen.

---

### Task 9: End-to-end verification in a local k3d dev cluster

**Objective:** Confirm the full merged system works: rancher server built from the merged
monorepo starts, and the `rancher-webhook` Deployment (running the image from Task 7) comes
up and registers admission rules correctly — same outcome as the pre-merge baseline.

**Uses:** `rancher-deploy-k3d-helm` skill for the base deploy workflow;
`rancher-webhook-systemcharts` skill's verification commands (once updated per Task 8 Step
3).

**Step 1: Build both images** (rancher server, per normal `rancher-build-system` skill
flow, and `rancher-webhook` per Task 7) from the merged monorepo.

**Step 2: Deploy into a local k3d cluster.**

**Step 3: Verify webhook pod health and admission registration**, same commands as the
embedded-chart plan's Task 9 Step 3:
```sh
kubectl -n cattle-system get pods | grep rancher-webhook
kubectl -n cattle-system logs deploy/rancher-webhook --tail=60
kubectl get validatingwebhookconfigurations rancher.cattle.io
kubectl get mutatingwebhookconfigurations rancher.cattle.io
```
Expected: pod `Running`, same admission-rule counts as the pre-merge baseline (33
validating + 8 mutating per the earlier-established baseline for this codebase).

**Step 4: Confirm no build/test regression across the rest of rancher** by running
rancher's full existing unit test suite (whatever the standard invocation is per
`rancher-build-system` skill) and confirming pass rates match the pre-merge baseline aside
from the new `pkg/webhook/...` tests added in Task 3.

**Step 5: Document results** as PR-description evidence (no "Test plan" section per repo
convention — describe what was verified and link supporting output inline).

---

### Task 10: Port webhook's integration test suite and CI wiring

**Objective:** Webhook's `tests/integration/` (17 test files: globalRole, roleTemplate,
clusterRoleTemplateBinding, projectRoleTemplateBinding, mgmtCluster, provCluster,
rkeMachineConfig, machinedeployment_scale, namespace, project, feature, secret,
proxyendpoints, clusterProxyConfig, port, failPolicy, plus `main_test.go`) is a REAL
integration suite exercising the live admission webhooks against a running cluster — it is
NOT covered by Task 3 Step 5's unit-test run or Task 9's kubectl-based smoke checks. Without
this task, the merge silently drops integration coverage.

**What currently drives it (webhook repo, pre-merge):**
- `package/Dockerfile` has an `integration-test-build` stage:
  `go test ./tests/integration/... -c -o /dist/rancher-webhook-integration.test`, copied
  into the final image as `/rancher-webhook-integration.test`.
- `scripts/integration-test` orchestrates a live run: waits for `deploy/rancher` and
  `deploy/rancher-webhook` rollouts, waits for the `apps.catalog.cattle.io/rancher-webhook`
  object to reach `deployed`, scales `rancher` to 0, `helm upgrade`s `rancher-webhook` to the
  freshly-built image (reading `./dist/tags` for `HELM_VERSION`/`IMAGE_REPO`/`IMAGE_TAG`),
  then runs the compiled test binary three times with different `-test.run` filters
  (`IntegrationTest`, `PortTest`, `IntegrationTest -testify.m TestGlobalRole` after a port
  change, then `FailurePolicyTest` after scaling `rancher-webhook` to 0).
- Some CI workflow under webhook's `.github/workflows/` invokes `scripts/integration-test`
  against a real/ephemeral cluster — locate and read it before assuming its shape.

**Files:**
- Move: `tests/integration/*.go` → `pkg/webhook/tests/integration/` (per Task 2's mapping;
  finalize whichever of the two candidate paths Task 2 Step 1 settled on — check rancher's
  own `tests/` directory conventions first, since rancher already has a top-level `tests/`
  tree for its own suites and may expect webhook's integration tests to live under
  `tests/webhook/integration/` instead, sitting alongside rancher's existing integration
  tests rather than nested under `pkg/webhook/`).
- Modify: `package/Dockerfile` — add an equivalent `integration-test-build` stage compiling
  the moved package, `COPY`ed into whichever final image is appropriate (confirm: does the
  compiled test binary belong in the `rancher` image, the `rancher-webhook`-equivalent image
  from Task 7, or a dedicated test-only image — webhook shipped it in its single image, but
  the merged repo produces multiple images now).
- Modify/port: `scripts/integration-test` → adapt to rancher's build output layout. Webhook's
  version reads `./dist/tags` (produced by webhook's own build scripts) for
  `HELM_VERSION`/`IMAGE_REPO`/`IMAGE_TAG` — confirm rancher's build scripts produce an
  equivalent artifact (check `scripts/` for anything analogous) and rewrite the sourcing line
  accordingly; do not assume the file exists unchanged.
- Modify: whichever CI workflow builds+runs this (identify via `search_files` on webhook's
  `.github/workflows/` for `integration-test`) — port the job into rancher's CI, using the
  Task 7 image names and rancher's own runner/cluster-provisioning conventions.

**Step 1: Read webhook's CI workflow that runs this** before writing anything:
```sh
search_files pattern="integration-test" path=".github/workflows" (webhook repo)
```
Read the matched workflow file(s) in full to understand what cluster/environment it expects
(e.g. does it spin up its own k3d/kind cluster, or run against a shared dev cluster) —
Task 9's "local k3d dev cluster" verification is a reasonable target environment to reuse,
but confirm before assuming.

**Step 2: Move the test files and fix imports/build tags**

Same import-rewrite treatment as Task 3 Step 2 (the moved `_test.go` files under
`tests/integration/` import `github.com/rancher/webhook/pkg/...` packages that are now
`github.com/rancher/rancher/pkg/webhook/...`). Run:
```sh
go vet ./pkg/webhook/tests/integration/...   # (or tests/webhook/integration/..., per Step 0 decision)
```
Expected: fails only on missing imports/deps at this stage (no cluster needed for `go vet`).
Iterate until it passes.

**Step 3: Port the Dockerfile stage and `scripts/integration-test`**

Add the `integration-test-build` stage and rewrite `scripts/integration-test`'s
`./dist/tags`/binary-path assumptions to match rancher's actual build output. Do not silently
drop the `FailurePolicyTest` / `PortTest` phases (which exercise webhook's failure-policy and
port-reconfiguration behavior specifically, not covered by the default `IntegrationTest` run)
— all three phases are distinct coverage.

**Step 4: Actually run the suite against a live cluster**

This is a hard verification gate, not optional: build the images (Task 7 + this task's
`integration-test-build` stage), deploy into the Task 9 k3d cluster, and run the ported
`scripts/integration-test` (or its rancher-CI equivalent) end to end.
Expected: all three test-binary invocations pass with the same pass/fail profile as a
pre-merge baseline run captured from the original `rancher/webhook` repo (capture that
baseline BEFORE starting Task 3, per Task 1's spirit, if not already done).

**Step 5: Wire into rancher's CI** so this doesn't silently stop running post-merge — add
the job to rancher's existing GH Actions (or whatever CI system triggers on PRs), following
the same runner/cluster-provisioning pattern already used by webhook's original workflow
(now ported) or an equivalent rancher CI job if one already runs comparable live-cluster
tests (check first — do not add redundant infrastructure if rancher already provisions a
similar ephemeral cluster for another purpose).

**Step 6: Commit**

```bash
git add pkg/webhook/tests/ package/Dockerfile scripts/integration-test .github/workflows/
git commit -s -m "webhook: port integration test suite and CI wiring into rancher/rancher"
```

---

## Risks / Tradeoffs / Open Questions

1. **CI duration.** Merging ~9,555 + 4,960 + ~2,097 = ~16,600 LOC (plus its tests) worth of
   new packages into rancher's build/test graph will lengthen `go build ./...`/`go test
   ./...` runs somewhat. Whether this is material enough to warrant path-based CI job
   splitting (only re-run `pkg/webhook/...` tests when files under that path changed) is a
   decision to make AFTER measuring actual CI time impact post-merge, not preemptively.
2. **Image size/build-graph tradeoff** from Task 7 Step 1's (a) vs (b) decision — needs
   sign-off from whoever owns image-size/build-time budgets for rancher's release
   pipeline.
3. **Loss of independent webhook versioning/release cadence.** Today, a webhook-only CVE fix
   can theoretically be released and consumed (via a `RancherWebhookVersion` bump) without
   a full rancher release. Post-merge, a webhook-only fix requires a rancher release (or at
   minimum a rancher patch release) to ship. This is a real behavior change worth explicit
   sign-off — it's arguably the INTENDED tradeoff (issue 56127 is asking to release
   webhook in lockstep with rancher), but confirm this reading is actually correct and
   desired before treating it as settled.
4. **External consumers of `rancher/webhook`'s repo/releases**, if any exist beyond
   `rancher/charts`' package definition (not verified in this investigation — worth an
   explicit search across `rancher/charts`, `rancher/rancher`, and any known downstream
   integrations for references to `rancher/webhook`'s GitHub Releases before the source
   repo is considered safe to archive/freeze).
5. **`git subtree`/history-preservation choice in Task 3** — if the team has a strong
   preference either way (preserve full webhook git history in `rancher/rancher`'s history
   vs. a clean single "vendor webhook" commit), confirm before Task 3 rather than after —
   redoing the merge commit structure later is expensive.
6. **Namespace choice (`pkg/webhook/...`)** — confirmed non-colliding as of this
   investigation, but re-verify immediately before Task 3 execution (both repos' `main`
   branches may have moved since this plan was written).
