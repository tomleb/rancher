# Embed rancher-webhook chart into rancher image (no ClusterRepo, no go:embed) — Implementation Plan

> **For Hermes:** Use subagent-driven-development skill to implement this plan task-by-task.
> **Context:** This plan is a hand-off; the implementing agent has none of the prior discussion.
> Read the whole "Background / Design Rationale" section before touching code — several
> design decisions were made explicitly to reject simpler-looking alternatives (go:embed,
> a fake ClusterRepo). Don't reintroduce them.

**Goal:** Stop deploying `rancher-webhook` via the external `rancher/charts` repo +
`ClusterRepo`/index.yaml pull pipeline. Instead, bake the webhook Helm chart directly into
the `rancher` and `rancher-agent` Docker images at build time (Docker-stage `COPY`, not
`go:embed`), and load it from a fixed on-disk path with **no `ClusterRepo` Kubernetes object
involved at all**. This removes the two-hop `rancher/webhook` → GitHub Release →
`rancher/charts` bump-bot publish pipeline for webhook specifically, fixing the CVE/dep-sync
pain described in https://github.com/rancher/rancher/issues/56127.

**Architecture:** Add a new Docker build stage that clones/checks out the webhook chart
source and `helm package`s it into `/var/lib/rancher-data/system-charts/rancher-webhook/`,
`COPY`ed into both the `rancher` (server) and `rancher-agent` final image stages (mirroring
the existing `rancher-charts`/`partner-charts`/`rke2-charts` stages). Add a small new Go
package (`pkg/catalogv2/systemchart`) that loads chart bytes directly off that fixed path
(reusing the existing git-catalog chart-loading primitives), and wire three call sites
(`content.Manager`, `helmop.Operations.getSpec`, `system.Manager.install`) to route the
`rancher-webhook` chart name through this new loader instead of a `ClusterRepo` lookup.
The existing `helm-operation-*` pod / `apps.catalog.cattle.io` Release / ConfigMap
values-merge machinery is otherwise completely untouched — only the chart *source* changes.

**Tech Stack:** Go 1.26, Helm SDK v4 (`helm.sh/helm/v4`), Docker multi-stage build.

**Scope:** This plan covers ONLY the deployment/embedding mechanism. It does NOT cover:
moving webhook's Go code into `rancher/rancher` (a separate, larger repo-merge decision —
see "Out of scope" below), or removing `RancherWebhookVersion`/build.yaml plumbing.

---

## Background / Design Rationale (read before implementing)

Traced against the `rancher/rancher` repo (worktree: `webhook-migration/rancher`, branch
built off `upstream/main`) and `rancher/webhook` (worktree: `webhook-migration/webhook`),
July 2026.

### Current deploy pipeline (what we're replacing, for webhook only)
1. `rancher/webhook` CI packages `charts/rancher-webhook` via `scripts/package-helm`,
   `helm package`s it, and attaches the `.tgz` as a GitHub Release asset (not a chart-repo
   commit).
2. `rancher/charts`' `packages/rancher-webhook/package.yaml` (on branch `dev-v2.X`) points
   its `url:` at that GitHub Release asset; an automated bot (`auto-bump.yaml` /
   `charts-build-scripts chart-bump`) re-fetches, repackages, and commits the result into
   `assets/` + `index.yaml` on the `dev-v2.X` branch.
3. Rancher's `pkg/controllers/dashboard/systemcharts/controller.go` watches
   `settings.RancherWebhookVersion` and calls `SystemChartsManager.Ensure(...)`, which
   eventually does `content.Manager.Index("", "rancher-charts", ...)` to look up the chart
   version in that `index.yaml`, then fetches the `.tgz` and runs it through a
   `helm-operation-*` pod.
4. **Downstream clusters**: `cattle-cluster-agent` runs the *same* `systemcharts.Register`
   code (gated by `features.MCMAgent.Enabled()`), with its own `rancher-charts` ClusterRepo,
   independently pulling and installing webhook the same way. The mgmt server never pushes
   chart bytes downstream — only customization *values* (via a `rancher-config` ConfigMap,
   see `pkg/controllers/management/clusterdeploy/clusterdeploy.go` `manageWebhookConfig()`).

This two-hop publish pipeline (webhook repo → GitHub Release → charts-repo bot) is the
concrete thing generating release-week toil and slow CVE-fix turnaround.

### Why Docker-stage embedding, not `go:embed`
- A packaging build step (`helm package` on `charts/rancher-webhook`) is required either
  way. That step belongs in the Dockerfile/build tooling (where `helm`/`git` already run
  for the existing `rancher-charts`/`partner-charts`/`rke2-charts` stages), not inside a Go
  `go:generate` step needing a helm binary available during `go build`.
- Docker-layer embedding is independently cacheable (unchanged chart version = cached
  layer); `go:embed` forces a full binary relink on every chart bump and bloats the single
  monolithic Go binary.
- It's consistent with the three chart-bundling stages that already exist in
  `package/Dockerfile` (`rancher-charts`, `partner-charts`, `rke2-charts`) — same shape,
  same `COPY --from=<stage>` pattern into both server and agent final stages (already
  proven to reach both local AND downstream, since server and agent images are built from
  the same source with `COPY --from=<chart-stage>` appearing in both the server COPY block
  around line ~203-205 and the agent COPY block around line ~554-556 of
  `package/Dockerfile`).

### Why NOT a fake `ClusterRepo` object
Rancher already has a "bundled catalog" mechanism (`pkg/catalogv2/git/utils.go`:
`RepoDir()`, `IsBundled()`) that would make an actual `ClusterRepo`-backed chart "just
work" with zero new Go code. We are **deliberately not doing this**: `rancher-webhook`'s
chart is not a user-configurable, editable, or independently-addable repository — showing
it as a `ClusterRepo` (visible via `kubectl get clusterrepos` and the Rancher UI's
Repositories list) would misrepresent what it is and invite "why can't I edit/disable this
repo" confusion. Instead, add small explicit special-case branches (each with a comment
explaining why) at exactly the three points that currently assume `ClusterRepo`:
1. `content.Manager.Index()` / `content.Manager.Chart()` (`pkg/catalogv2/content/content.go`)
   — currently call `c.getRepo(namespace, name)` → `c.clusterRepos.Get(name)`.
2. `helmop.Operations.getSpec()` (`pkg/catalogv2/helmop/operation.go` line ~286) — currently
   calls `s.clusterRepos.Get(name, ...)` purely to resolve `ServiceAccount`/
   `ServiceAccountNamespace` for impersonation. System-chart installs use the built-in
   `system:masters` `installUser`, so this is a no-op for webhook regardless — short-circuit
   it explicitly instead of resolving a nonexistent/dummy repo.
3. `system.Manager.install()` (`pkg/catalogv2/system/system.go` line ~374) — currently
   hardcodes `"rancher-charts"` as the target of `m.operation.Upgrade(...)`. Needs to be
   parameterized so webhook's `desiredKey` can pass a different "chart source" identifier.

### Upgrade-safety notes (validated against the code, not just theory)
- Helm releases are identified by **release name + namespace** in the Helm release store
  (a Secret/ConfigMap), entirely independent of where the chart tarball came from.
  `system.Manager.isInstalled()` calls `m.helmClient.ListReleases(namespace, name, ...)` —
  no dependency on chart source.
- As long as the new bundled chart keeps `Chart.yaml`'s `name: rancher-webhook`, the release
  name/namespace stay `rancher-webhook` / `cattle-system` (`chart.Definition{ReleaseName,
  ReleaseNamespace: namespace.System}` in `getChartsToInstall()`), and the shipped version is
  semver-greater than whatever's currently installed, a normal `helm upgrade` occurs — same
  release object, new revision, values carried forward via the existing JSON merge-patch
  logic in `desiredVersionAndValues()`.
- `systemcharts/controller.go` already sets `takeOwnership := chartDef.ChartName ==
  chart.WebhookChartName` for a prior "webhook needs to adopt a resource that wasn't
  originally part of its chart" migration (the `MutatingWebhookConfiguration` case). This
  flag flows into `ChartUpgradeAction.TakeOwnership`, and should absorb any minor
  resource-ownership diffs between the old externally-published chart and the new bundled
  one, without new code.
- Values-schema compatibility, CRD-upgrade semantics (Helm doesn't reconcile CRDs on
  upgrade), and binary-rollback-across-the-cutover are pre-existing constraints, not new
  risks introduced by this change — see "Risks" section below, still worth calling out
  explicitly in the PR description.

### Out of scope for this plan
- Moving webhook's Go code (validators/mutators, `pkg/resources/...`) into `rancher/rancher`
  to fix the `go.mod`/dependency-sync pain directly. That's a separate, larger decision
  (full repo merge vs. partial "move only the coupled validators" split) not covered here.
  This plan only fixes the *chart/release packaging* pain.
- Applying the same treatment to `fleet`, `turtles`, `remotedialer-proxy` (other
  systemcharts). The `systemchart` package below is written to be reusable for them later,
  but this plan's tasks only wire up `rancher-webhook`.
- Any UI change to `chart.WebhookChartName` version display, if the Rancher UI marketplace
  currently lists webhook as browsable/pickable — verify in Task 9 whether this needs a UI
  follow-up (flagged as an open question, not solved here).

---

## Files likely to change

- `package/Dockerfile` — new build stage + COPY into server/agent final stages.
- `pkg/catalogv2/systemchart/systemchart.go` — **new file**, chart loader off fixed path.
- `pkg/catalogv2/systemchart/systemchart_test.go` — **new file**.
- `pkg/catalogv2/git/index.go` — refactor: extract the directory-walk-and-load logic from
  `buildOrGetIndex()` into an exported helper reusable by `systemchart`.
- `pkg/catalogv2/content/content.go` — special-case `Index()`/`Chart()` for
  `chart.WebhookChartName`.
- `pkg/catalogv2/helmop/operation.go` — special-case `getSpec()` for the webhook chart name.
- `pkg/catalogv2/system/system.go` — parameterize the hardcoded `"rancher-charts"` repo name
  in `install()`/`Ensure()`/`desiredKey`.
- `pkg/controllers/dashboard/systemcharts/controller.go` — pass the new repo-name parameter
  for webhook's `chart.Definition` entry in `getChartsToInstall()`.
- `pkg/controllers/dashboard/chart/chart.go` — possibly add a constant for the new
  "system chart source" identifier next to `WebhookChartName`.

---

## Step-by-step plan

### Task 1: Add a constant identifying the webhook system-chart source

**Objective:** Introduce a named identifier (not `"rancher-charts"`) for "this chart comes
from the embedded/bundled system-chart path, not a ClusterRepo."

**Files:**
- Modify: `pkg/controllers/dashboard/chart/chart.go` (near existing `WebhookChartName` const,
  ~line 22-23)

**Step 1: Add the constant**

```go
// WebhookChartName name of the chart for rancher-webhook.
WebhookChartName = "rancher-webhook"

// WebhookSystemChartSource identifies the bundled/embedded system-chart source for
// rancher-webhook (see pkg/catalogv2/systemchart). This is intentionally distinct from a
// ClusterRepo name: rancher-webhook is not a user-facing, editable chart repository — it
// is baked into the rancher/rancher-agent images at build time.
WebhookSystemChartSource = "rancher-webhook-embedded"
```

**Step 2: Build to confirm no syntax errors**

Run: `cd pkg/controllers/dashboard/chart && go build ./...`
Expected: success, no output.

**Step 3: Commit**

```bash
git add pkg/controllers/dashboard/chart/chart.go
git commit -s -m "chart: add WebhookSystemChartSource constant for embedded webhook chart"
```

---

### Task 2: Extract a reusable "load chart index from a directory" helper

**Objective:** `pkg/catalogv2/git/index.go`'s `buildOrGetIndex(dir string)` already walks a
directory, loads any chart archives found via `chart.LoadArchive`, and builds a
`repo.IndexFile`. Export this logic so the new `systemchart` package can reuse it without
duplicating the walk logic.

**Files:**
- Modify: `pkg/catalogv2/git/index.go`

**Step 1: Rename the unexported function to an exported one with the same body**

Change `func buildOrGetIndex(dir string) (*repo.IndexFile, error) {` to:

```go
// BuildIndexFromDir walks dir, loading any Helm chart archives/directories found and
// building a repo.IndexFile from their metadata. If an index.yaml already exists under
// dir, that is loaded and returned instead (existing behavior, preserved for git-catalog
// callers). Exported so non-git chart sources (see pkg/catalogv2/systemchart) can reuse
// this walk logic without depending on the git package's ClusterRepo-oriented APIs.
func BuildIndexFromDir(dir string) (*repo.IndexFile, error) {
```

Update the internal caller `buildOrGetIndex(dir)` inside `BuildOrGetIndex()` (the exported
wrapper a few lines above) to call `BuildIndexFromDir(dir)` instead.

**Step 2: Build**

Run: `cd pkg/catalogv2/git && go build ./...`
Expected: success. Fix any other internal callers of `buildOrGetIndex` if `search_files`
turns up more than the one usage already read.

**Step 3: Run existing git package tests to confirm no regression**

Run: `go test ./pkg/catalogv2/git/...`
Expected: PASS (all existing tests, e.g. `pkg/catalogv2/git/index_test.go` if present).

**Step 4: Commit**

```bash
git add pkg/catalogv2/git/index.go
git commit -s -m "catalogv2/git: export BuildIndexFromDir for reuse by non-ClusterRepo chart sources"
```

---

### Task 3: Create the `systemchart` package

**Objective:** New package that loads the bundled webhook chart from a fixed on-disk path,
with no `ClusterRepo` involved.

**Files:**
- Create: `pkg/catalogv2/systemchart/systemchart.go`
- Create: `pkg/catalogv2/systemchart/systemchart_test.go`

**Step 1: Write the package**

```go
// Package systemchart loads Helm charts that are baked directly into the rancher /
// rancher-agent images at build time (see package/Dockerfile), rather than being sourced
// from a catalog.cattle.io ClusterRepo. Currently used for rancher-webhook; see
// docs/webhook-embedded-chart.md (or the design plan in .hermes/plans/) for rationale.
package systemchart

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/rancher/rancher/pkg/catalogv2/chart"
	"github.com/rancher/rancher/pkg/catalogv2/git"
	repo "helm.sh/helm/v4/pkg/repo/v1"
)

// BaseDir is the root directory, baked into the image by package/Dockerfile, under which
// each bundled system chart has its own subdirectory named after the chart.
const BaseDir = "/var/lib/rancher-data/system-charts"

// Index returns an in-memory IndexFile built from the packaged chart(s) found under
// BaseDir/<chartName>. Mirrors content.Manager.Index()'s signature-relevant return type so
// callers can substitute it directly.
func Index(chartName string) (*repo.IndexFile, error) {
	dir := filepath.Join(BaseDir, chartName)
	idx, err := git.BuildIndexFromDir(dir)
	if err != nil {
		return nil, fmt.Errorf("systemchart: building index for %q from %q: %w", chartName, dir, err)
	}
	return idx, nil
}

// Chart returns the packaged chart archive for chartName/version as a ReadCloser.
func Chart(chartName, version string) (io.ReadCloser, error) {
	idx, err := Index(chartName)
	if err != nil {
		return nil, err
	}
	cv, err := idx.Get(chartName, version)
	if err != nil {
		return nil, fmt.Errorf("systemchart: chart %q version %q not found: %w", chartName, version, err)
	}
	if len(cv.URLs) == 0 {
		return nil, fmt.Errorf("systemchart: chart %q version %q has no archive path", chartName, version)
	}
	dir := filepath.Join(BaseDir, chartName)
	archive, ok, err := chart.LoadArchive(filepath.Join(dir, cv.URLs[0]))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("systemchart: failed to load archive for %q version %q", chartName, version)
	}
	return archive.Open()
}
```

Note: confirm `idx.Add(...)`'s second argument (the relative path recorded as `cv.URLs[0]`
by `BuildIndexFromDir`/`git.buildOrGetIndex`) is relative to `dir` — it is, per the existing
`git.buildOrGetIndex` implementation (`rel, err := filepath.Rel(dir, path)`), so
`filepath.Join(dir, cv.URLs[0])` in `Chart()` above is correct.

**Step 2: Write a table-driven test**

```go
package systemchart

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeTestChart packages a minimal chart into dir/rancher-webhook/ via `helm package`,
// mimicking what package/Dockerfile's build stage produces. Requires `helm` on PATH; if
// unavailable, t.Skip().
func writeTestChart(t *testing.T, baseDir, name, version string) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm binary not available")
	}
	srcDir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(filepath.Join(srcDir, "templates"), 0o755))
	chartYAML := "apiVersion: v2\nname: " + name + "\nversion: " + version + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "Chart.yaml"), []byte(chartYAML), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "values.yaml"), []byte("{}\n"), 0o644))
	destDir := filepath.Join(baseDir, name)
	require.NoError(t, os.MkdirAll(destDir, 0o755))
	cmd := exec.Command("helm", "package", srcDir, "-d", destDir)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

func TestIndexAndChart(t *testing.T) {
	base := t.TempDir()
	writeTestChart(t, base, "rancher-webhook", "0.1.0")

	origBaseDir := BaseDir
	// BaseDir is a const; for testability, this test documents the expected shape but
	// exercises git.BuildIndexFromDir directly at base instead. See Task 3 alternative
	// below if BaseDir needs to become a package var for full injectability.
	idx, err := indexFromDir(filepath.Join(base, "rancher-webhook"))
	require.NoError(t, err)
	require.NotNil(t, idx)
	_, err = idx.Get("rancher-webhook", "0.1.0")
	require.NoError(t, err)
	_ = origBaseDir
}
```

**IMPORTANT — fix before running:** `BaseDir` is declared `const` above, which makes it
non-injectable for tests. Change it to `var BaseDir = "/var/lib/rancher-data/system-charts"`
(package-level var, not const) so tests can override it, e.g.:

```go
func TestIndexAndChart(t *testing.T) {
	base := t.TempDir()
	writeTestChart(t, base, "rancher-webhook", "0.1.0")

	old := BaseDir
	BaseDir = base
	defer func() { BaseDir = old }()

	idx, err := Index("rancher-webhook")
	require.NoError(t, err)
	_, err = idx.Get("rancher-webhook", "0.1.0")
	require.NoError(t, err)

	rc, err := Chart("rancher-webhook", "0.1.0")
	require.NoError(t, err)
	defer rc.Close()
}
```

Replace the earlier draft test with this corrected version (drop `indexFromDir`/
`writeTestChart`'s baseDir-mutation dance — just set the package var directly).

**Step 3: Run the test**

Run: `go test ./pkg/catalogv2/systemchart/... -v`
Expected: `TestIndexAndChart` PASS (or SKIP if `helm` isn't on PATH in the dev sandbox —
acceptable for now, must pass in CI which has `helm` available per the existing
`rancher-charts` Dockerfile stage assumption).

**Step 4: Commit**

```bash
git add pkg/catalogv2/systemchart/
git commit -s -m "catalogv2: add systemchart package for image-embedded (non-ClusterRepo) charts"
```

---

### Task 4: Wire `content.Manager` to use `systemchart` for the webhook chart name

**Objective:** Short-circuit `Index()`/`Chart()` before they touch `getRepo()`/
`clusterRepos.Get()` when the requested chart is the embedded webhook chart.

**Files:**
- Modify: `pkg/catalogv2/content/content.go`

**Step 1: Add the import**

```go
"github.com/rancher/rancher/pkg/catalogv2/systemchart"
"github.com/rancher/rancher/pkg/controllers/dashboard/chart" // for chart.WebhookChartName
```

Check for an import cycle: `pkg/controllers/dashboard/chart` must not itself import
`pkg/catalogv2/content` (verify with `search_files(pattern="catalogv2/content",
path="pkg/controllers/dashboard/chart")` before adding this import — if a cycle exists,
move `WebhookChartName`/`WebhookSystemChartSource` into a lower-level package such as
`pkg/catalogv2` instead, and update Task 1 accordingly).

**Step 2: Add the special case to `Index()`**

```go
func (c *Manager) Index(namespace, name, targetK8sVersion string, skipFilter bool) (*repo.IndexFile, error) {
	// rancher-webhook is baked into the image at build time and is not backed by a
	// ClusterRepo — serve its index directly instead of resolving a (nonexistent) repo.
	if namespace == "" && name == chart.WebhookSystemChartSource {
		return systemchart.Index(chart.WebhookChartName)
	}
	r, err := c.getRepo(namespace, name)
	...
```

**Step 3: Add the same special case to `Chart()`**

```go
func (c *Manager) Chart(namespace, name, chartName, version string, skipFilter bool) (io.ReadCloser, error) {
	if namespace == "" && name == chart.WebhookSystemChartSource {
		return systemchart.Chart(chartName, version)
	}
	index, err := c.Index(namespace, name, "", skipFilter)
	...
```

**Step 4: Build**

Run: `go build ./pkg/catalogv2/content/...`
Expected: success.

**Step 5: Add a unit test** in `pkg/catalogv2/content/content_test.go` asserting that
`Index("", chart.WebhookSystemChartSource, ..., ...)` returns a result without calling
`clusterRepos.Get` (use the existing mock — assert `.EXPECT()` is never set/called for
`clusterRepos` in this case). Follow the existing test file's mocking conventions.

Run: `go test ./pkg/catalogv2/content/... -v`
Expected: PASS.

**Step 6: Commit**

```bash
git add pkg/catalogv2/content/content.go pkg/catalogv2/content/content_test.go
git commit -s -m "catalogv2/content: route rancher-webhook chart lookups through systemchart, bypassing ClusterRepo"
```

---

### Task 5: Wire `helmop.Operations.getSpec` to skip ClusterRepo resolution for webhook

**Objective:** `getSpec()` is only used (in the non-`isApp` branch) to find a
`ServiceAccount`/`ServiceAccountNamespace` for impersonation. System-chart installs always
use the built-in `installUser` (`system:masters`), so short-circuit to an empty
`RepoSpec{}` for the webhook source rather than resolving a ClusterRepo that doesn't exist.

**Files:**
- Modify: `pkg/catalogv2/helmop/operation.go` (function `getSpec`, ~line 286)

**Step 1: Add the special case**

```go
func (s *Operations) getSpec(namespace, name string, isApp bool) (*catalog.RepoSpec, error) {
	if isApp {
		...
	}
	if namespace == "" {
		// rancher-webhook is an image-embedded system chart, not a ClusterRepo — it has
		// no per-repo service account, so return an empty spec instead of failing a
		// clusterRepos.Get() lookup for a repo that intentionally does not exist.
		if name == chart.WebhookSystemChartSource {
			return &catalog.RepoSpec{}, nil
		}
		clusterRepo, err := s.clusterRepos.Get(name, metav1.GetOptions{})
		...
```

Add the import for `pkg/controllers/dashboard/chart` if not already present in this file
(check for import cycles the same way as Task 4 — `pkg/catalogv2/helmop` must not be
imported back by `pkg/controllers/dashboard/chart`).

**Step 2: Build**

Run: `go build ./pkg/catalogv2/helmop/...`
Expected: success.

**Step 3: Add/update a unit test** in `pkg/catalogv2/helmop/operation_test.go` covering
`getSpec("", chart.WebhookSystemChartSource, false)` returns `&catalog.RepoSpec{}, nil`
without calling `clusterRepos.Get`.

Run: `go test ./pkg/catalogv2/helmop/... -v`
Expected: PASS.

**Step 4: Commit**

```bash
git add pkg/catalogv2/helmop/operation.go pkg/catalogv2/helmop/operation_test.go
git commit -s -m "catalogv2/helmop: skip ClusterRepo lookup for embedded rancher-webhook chart source"
```

---

### Task 6: Parameterize the repo-name target in `system.Manager`

**Objective:** `install()` currently hardcodes `"rancher-charts"` as both the name passed
to `m.content.Index()`/`m.content.Chart()` (indirectly, via `m.operation.Upgrade(...,
"rancher-charts", ...)`) and the fixed target namespace/name for the upgrade operation.
Add a `repoName` field to `desiredKey` (or a new parameter threaded through `Ensure()` →
`install()`) so callers can select `chart.WebhookSystemChartSource` instead of
`"rancher-charts"`.

**Files:**
- Modify: `pkg/catalogv2/system/system.go`

**Step 1: Add `repoName` to `desiredKey` and thread it through**

```go
type desiredKey struct {
	namespace            string
	chartName            string
	releaseName          string
	minVersion           string
	exactVersion         string
	installImageOverride string
	repoName             string // NEW: which "repo" (ClusterRepo name, or a systemchart
	                             // source identifier like chart.WebhookSystemChartSource)
	                             // to resolve chartName/version against. Defaults to
	                             // "rancher-charts" when empty, preserving existing
	                             // behavior for all other system charts.
}
```

Update `Ensure(...)` signature to accept `repoName string` as a new parameter (append at
the end to minimize diff churn at call sites — but note this DOES require updating every
call site; see Task 7), and pass it through into the `desired{key: desiredKey{...,
repoName: repoName}}` construction.

**Step 2: Update `install()` to use `key.repoName` with a fallback**

```go
func (m *Manager) install(namespace, chartName, releaseName, minVersion, exactVersion string, values map[string]interface{}, takeOwnership bool, installImageOverride, repoName string) error {
	if repoName == "" {
		repoName = "rancher-charts" // preserve existing default for fleet/turtles/RDP
	}
	index, err := m.content.Index("", repoName, "", true)
	...
	op, err := m.operation.Upgrade(m.ctx, installUser, "", repoName, bytes.NewBuffer(upgrade), installImageOverride)
	...
```

Update the two other call sites inside `install()`'s signature change ripple
(`installCharts()` calling `m.install(key.namespace, key.chartName, key.releaseName,
key.minVersion, key.exactVersion, values, takeOwnership, key.installImageOverride,
key.repoName)`).

**Step 3: Update the `Manager` interface definition** wherever `Ensure(...)` is declared as
an interface (search for `Ensure(namespace, chartName` across the codebase — likely in
`pkg/controllers/dashboard/chart/chart.go`'s `Manager` interface, given
`h.manager.Ensure(...)` is called from `systemcharts/controller.go`). Update that interface
signature too, and regenerate/update any mocks (`pkg/controllers/dashboard/chart/fake/manager.go`
was seen in the earlier repo scan — check if it's hand-written or mockery-generated; if
mockery-generated, re-run the `go:generate` directive for it).

**Step 4: Build the whole module**

Run: `go build ./...`
Expected: success. This will surface every call site needing the new parameter — fix them
by passing `""` (preserving default `"rancher-charts"` behavior) except webhook's, which
gets `chart.WebhookSystemChartSource` (done in Task 7).

**Step 5: Run existing tests**

Run: `go test ./pkg/catalogv2/system/... ./pkg/controllers/dashboard/chart/... -v`
Expected: PASS (update any test fixtures asserting on `Ensure()`'s old arg count).

**Step 6: Commit**

```bash
git add pkg/catalogv2/system/system.go pkg/controllers/dashboard/chart/
git commit -s -m "catalogv2/system: parameterize repo/source name in Ensure/install, default to rancher-charts"
```

---

### Task 7: Point webhook's `chart.Definition` at the new source

**Objective:** In `getChartsToInstall()`, pass `chart.WebhookSystemChartSource` as the new
`repoName` argument for the webhook entry's `Ensure()` call, and `""` (default) for
fleet/turtles/RDP.

**Files:**
- Modify: `pkg/controllers/dashboard/systemcharts/controller.go`

**Step 1: Locate the loop calling `h.manager.Ensure(...)`** (~line 225) and the
`chart.Definition` struct (~`pkg/controllers/dashboard/chart/chart.go` line 52). Add a
`RepoName string` field to `Definition` (empty by default) and set it to
`chart.WebhookSystemChartSource` on webhook's `Definition` entry only.

**Step 2: Update the `Ensure()` call site**

```go
if err := h.manager.Ensure(chartDef.ReleaseNamespace, chartDef.ChartName, chartDef.ReleaseName, minVersion, exactVersion, values, takeOwnership, helmOpInstallImageOverride, chartDef.RepoName); err != nil {
```

**Step 3: Build**

Run: `go build ./pkg/controllers/dashboard/systemcharts/...`
Expected: success.

**Step 4: Run the systemcharts controller tests**

Run: `go test ./pkg/controllers/dashboard/systemcharts/... -v -run Test_ChartInstallation`
Expected: PASS. Update the test's expected `Ensure()` mock-call arguments to include the
new `repoName` parameter (empty string for RDP/turtles, `chart.WebhookSystemChartSource`
for webhook).

**Step 5: Commit**

```bash
git add pkg/controllers/dashboard/systemcharts/controller.go pkg/controllers/dashboard/chart/chart.go
git commit -s -m "systemcharts: route rancher-webhook installs through the embedded chart source"
```

---

### Task 8: Add the Docker build stage that produces the bundled chart

**Objective:** Package `rancher-webhook`'s Helm chart at image-build time and land it under
`/var/lib/rancher-data/system-charts/rancher-webhook/`, matching what `systemchart.Index`/
`Chart` expect.

**Files:**
- Modify: `package/Dockerfile`

**Step 1: Add a new build stage**, placed near the existing `rancher-charts`/
`partner-charts`/`rke2-charts` stages (~line 71-96):

```dockerfile
FROM builder AS webhook-chart
ARG CATTLE_RANCHER_WEBHOOK_VERSION
RUN mkdir -p /var/lib/rancher-data/system-charts/rancher-webhook && \
    git config --global url."https://github.com/rancher/".insteadOf https://git.rancher.io/ && \
    git clone --no-checkout --depth 1 https://github.com/rancher/webhook /tmp/webhook-src && \
    git -C /tmp/webhook-src fetch --depth 1 origin "refs/tags/v${CATTLE_RANCHER_WEBHOOK_VERSION}" && \
    git -C /tmp/webhook-src checkout FETCH_HEAD -- charts/rancher-webhook && \
    helm package /tmp/webhook-src/charts/rancher-webhook -d /var/lib/rancher-data/system-charts/rancher-webhook
```

Notes / open questions to resolve with the team before finalizing this exact stage:
- Confirm the exact webhook git ref/tag format expected — `CATTLE_RANCHER_WEBHOOK_VERSION`
  today (per `build.yaml`) looks like `110.0.0+up0.11.0-rc.24`; the actual webhook repo tag
  is `v0.11.0-rc.24` (the `+up<x>` prefix is rancher/charts' own packaging-revision scheme,
  stripped). This stage needs the *webhook repo's own tag*, not the rancher/charts
  packaged-version string — extract accordingly (likely via a small shell parse of
  `build.yaml`'s `webhookVersion:` field, same as `scripts/export-config` already does for
  `CATTLE_RANCHER_WEBHOOK_VERSION`, but keeping only the part after `+up`).
- Confirm `helm` CLI is available in the `builder` stage (check `FROM
  registry.suse.com/bci/bci-base:15.7` — likely needs `helm` installed via a package
  manager RUN step if not already present; the existing `rancher-charts` stage only uses
  `git`+`mkdir`, not `helm package`, so this is new tooling for the builder stage).
- If webhook is a private/rate-limited clone in some build environments, consider caching
  or using the same `git.rancher.io` mirror indirection already used for the other three
  chart stages (see the `git config --global url.` line reused above).

**Step 2: Add `COPY --from=webhook-chart` to both final image stages**

Immediately following the existing `COPY --from=rancher-charts ...` lines at ~203-205
(server final stage) and ~554-556 (agent final stage), add:

```dockerfile
COPY --from=webhook-chart /var/lib/rancher-data/system-charts/rancher-webhook /var/lib/rancher-data/system-charts/rancher-webhook
```

**Step 3: Build a Docker image locally to validate the stage in isolation**

Run: `docker build --target webhook-chart -t webhook-chart-test -f package/Dockerfile --build-arg CATTLE_RANCHER_WEBHOOK_VERSION=<pin-a-real-version> .`
Expected: build succeeds; inspect via
`docker run --rm webhook-chart-test ls /var/lib/rancher-data/system-charts/rancher-webhook`
and confirm a `rancher-webhook-<version>.tgz` file is present.

**Step 4: Commit**

```bash
git add package/Dockerfile
git commit -s -m "package: bundle rancher-webhook chart into rancher/rancher-agent images at build time"
```

---

### Task 9: End-to-end verification in a local k3d dev cluster

**Objective:** Confirm the full path works: image build → rancher server install → webhook
pod up with admission rules registered, sourced from the embedded chart, not
`rancher-charts`.

**Uses:** `rancher-deploy-k3d-helm` skill for the base deploy workflow;
`rancher-webhook-systemcharts` skill's verification commands.

**Step 1: Build a full dev image** with the new Dockerfile changes (see
`rancher-build-system` skill for the standard `make` targets).

**Step 2: Deploy into a local k3d cluster** per `rancher-deploy-k3d-helm` skill.

**Step 3: Verify webhook installed from the new source, not rancher-charts**

```sh
kubectl -n cattle-system get pods | grep rancher-webhook
kubectl -n cattle-system logs deploy/rancher-webhook --tail=60
kubectl get validatingwebhookconfigurations rancher.cattle.io
kubectl get mutatingwebhookconfigurations rancher.cattle.io
kubectl get apps.catalog.cattle.io -n cattle-system rancher-webhook -o jsonpath='{.spec.chart.metadata.version}{"\n"}'
```

Expected: webhook pod is `Running`, admission webhook configs are registered (same rule
counts as before — 33 validating + 8 mutating per the earlier baseline in the
`rancher-webhook-systemcharts` skill notes), and the `apps.catalog.cattle.io` object exists
and shows a chart version (confirming the Helm-release/observability path is unaffected).

**Step 4: Verify no `rancher-webhook` ClusterRepo exists**

```sh
kubectl get clusterrepo rancher-webhook 2>&1
```

Expected: `NotFound` error — confirms the "no fake ClusterRepo" design goal.

**Step 5: Verify the upgrade path** — deploy an older rancher image (pre-cutover, still
using the `rancher-charts`-sourced webhook), confirm webhook installs normally, then deploy
the new image on top (same cluster) and confirm:

```sh
kubectl -n cattle-system get apps.catalog.cattle.io rancher-webhook -o jsonpath='{.spec.chart.metadata.version}{"\n"}'
kubectl -n cattle-system get secret -l owner=helm,name=rancher-webhook --sort-by=.metadata.creationTimestamp
```

Expected: chart version bumps to the new embedded version; Helm release history shows a
new revision for the same release name (not a new release / not an uninstall+reinstall).

**Step 6: Document results** — capture command output as evidence in the PR description
(no "Test plan" section — per repo convention, just describe what was verified and link
supporting command output/logs inline in the PR body).

---

## Risks / Tradeoffs / Open Questions

1. **Webhook version pinning at Docker-build time vs. today's env-var/`build.yaml` flow.**
   `CATTLE_RANCHER_WEBHOOK_VERSION` today flows through `scripts/export-config` →
   `RancherWebhookVersion` setting → the `systemcharts` controller's `exactVersion` param.
   With the embedded chart, the *chart bytes* are pinned by whichever ref Task 8's Docker
   stage checks out (needs to match `build.yaml`'s `webhookVersion` field). If these two
   drift, `system.Manager.install()`'s version-mismatch check
   (`if v != latestVersionMatcher && chart.Version != v`) will error rather than silently
   installing the wrong version — fail-safe, but worth an explicit CI check that
   `build.yaml`'s webhook version and the Dockerfile's checked-out webhook chart tag always
   match (e.g. derive both from the same source instead of hand-maintaining two places).
2. **Rollback across the cutover.** An older `rancher` binary (pre-cutover) rolled back onto
   a cluster that has already run the embedded-chart version won't know how to source
   webhook from `rancher-charts` again if that path was removed from the old binary's
   `rancher-charts` `index.yaml` — but since we're not modifying `rancher/charts` at all in
   this plan (webhook simply stops being referenced from *new* rancher versions), old
   rancher binaries continue working exactly as before via the untouched `rancher-charts`
   path. No actual regression here — flagging only because it's the kind of thing reviewers
   will ask about.
3. **UI marketplace listing.** If Rancher's UI currently lists `rancher-webhook` as a
   browsable/selectable chart under Apps & Marketplace (sourced from the `rancher-charts`
   ClusterRepo), removing it from that ClusterRepo's index means it silently disappears
   from that UI. Need to confirm with the UI/dashboard team whether webhook is intentionally
   hidden already (likely, since it's a system chart) or whether this is a user-visible
   change requiring its own callout.
4. **`helm` CLI availability in the Docker `builder` stage** (Task 8) — needs verification;
   may require adding a package-manager install step, increasing image build time slightly.
5. **CRDs bundled in the webhook chart, if any**, are not reconciled on `helm upgrade` by
   Helm's own semantics — pre-existing limitation, unaffected by this change, but worth a
   footnote in the PR description so reviewers don't think it's a new gap.
6. **Downstream cluster-agent image** must also carry the new Docker build-stage output
   (confirmed already true structurally per Task 8 Step 2 modifying both COPY blocks) —
   Task 9 should include a downstream-cluster verification pass, not just local/mgmt
   cluster, before this is considered fully validated.
