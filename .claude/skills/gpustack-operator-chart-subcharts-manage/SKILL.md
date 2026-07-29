---
name: gpustack-operator-chart-subcharts-manage
description: "Add, upgrade, patch, or remove a **vendored subchart** of the operator Helm chart (`deploy/gpustack-operator/chart/charts/*` — Kueue, Node Feature Discovery, csi-driver-nfs, csi-driver-s3): the pinned version list in `hack/deps.sh`, the patch files under `hack/deploy/gpustack-operator/chart/charts/<name>/*.patch`, the parent `Chart.yaml` dependency + `values.yaml` block, and the `make deps` / `make generate chart` / `make lint chart` / `make test chart` ladder CI enforces. Invoke when bumping a bundled upstream chart, writing or rebasing a patch against one, when `make deps` fails to apply a patch or leaves `.rej`, or when dropping a bundled app. Background: `docs/development.md` (*Helm chart* / *Vendored subcharts*) and `specs/2026-07-28-bundled-apps-subchart-split.md` F1–F2. Examples: \"bump Kueue to 0.18.5\", \"vendor a new subchart\", \"my patch no longer applies\", \"patch the NFD chart to honour global.imageRegistry\", \"remove the S3 CSI driver from the chart\"."
allowed-tools: "Read, Edit, Write, Grep, Glob, Bash(make deps*), Bash(make generate chart*), Bash(make lint chart*), Bash(make test chart*), Bash(go test ./pkg/worker/kuberess/*), Bash(git status*), Bash(git diff*), Bash(git log*), Bash(git rm*), Bash(git checkout --*), Bash(git -C /tmp/*), Bash(git init*), Bash(./.sbin/helm*), Bash(curl -sSfL*), Bash(tar -zxf*), Bash(rsync -a*), Bash(mktemp -d*), Bash(find deploy/gpustack-operator/chart/charts*), Bash(ls*), Bash(cat deploy/gpustack-operator/chart/*), Bash(cat hack/deploy/gpustack-operator/chart/*), Bash(rm -rf deploy/gpustack-operator/chart/charts/*), Bash(rm -f deploy/gpustack-operator/chart/Chart.lock), Bash(kubectl get*), Bash(kubectl patch*), Bash(kubectl delete*), Bash(kubectl config current-context), Bash(helm list*), Bash(helm uninstall*)"
model: sonnet
---

# Manage a vendored subchart of the operator chart

Kueue, Node Feature Discovery, `csi-driver-nfs` and `csi-driver-s3` ship as subcharts of the operator chart,
vendored **unpacked and patched** under `deploy/gpustack-operator/chart/charts/<name>/` and **committed** —
which is what makes `helm install` work from a bare clone and keeps CI offline-capable.

## When to use

Bumping a pinned upstream version; writing a patch or rebasing one `make deps` rejected; vendoring an
extra upstream chart or dropping one; any edit under `deploy/gpustack-operator/chart/charts/**` or
`hack/deploy/gpustack-operator/chart/charts/**`.

## The machinery, in one screen

| Thing | Where |
|---|---|
| Pinned `<archive-url> <version> <dest>` list | `chart_staging()` in `hack/deps.sh` |
| Vendored, committed trees | `deploy/gpustack-operator/chart/charts/<name>/` (+ a `_VERSION_` stamp) |
| Patches | `hack/deploy/gpustack-operator/chart/charts/<name>/<topic>.patch` |
| Dependency declaration | `deploy/gpustack-operator/chart/Chart.yaml` (`repository: ""` + `condition`) |
| Configuration surface | `deploy/gpustack-operator/chart/values.yaml` (kebab-case key per subchart) |
| Go guards | `pkg/worker/kuberess/chart_test.go` (in-process render), `apps.go`, `apps_gpustack_operator.go` |

`make deps` → `chart_staging()`: per line, compare `<dest>/_VERSION_` with the pinned version and **skip
the tree when it matches**; otherwise `rm -rf` the tree, download the archive, `tar --strip-components 1`
into it, `rm -f README.md`, stamp `_VERSION_`, then apply `hack/<dest>/*.patch` — **one `patch` call per
file, alphabetical order** — with `patch -p1 -N --forward --silent -F0 --no-backup-if-mismatch --directory
<dest>`. A patch that fails to apply is a hard error, and so is one that leaves a `*.rej`: those are
gitignored, so nothing else would notice a half-applied patch. `-F0` is what makes that check sharp — every
context line must match exactly, so `patch` never guesses — while a hunk landing at a **shifted line
number** is fine and leaves no backup. Two patches touching one file shift each other; before this, editing
either meant re-capturing the other for nothing.

Two rules follow: **never keep a change by editing a staged tree in place** (the next re-extract deletes
it — the patch file is the change), and **no `*.tgz` under `charts/`** (CI fails on any archive there, and
on any drift between the committed trees and a fresh `make deps`).

Patches exist because **Helm merges a subchart's values instead of templating them**: the parent cannot
compose `global.imageRegistry` into a subchart's `image.repository`, and `global` is its only channel into
a subchart. Hence each tree's `global-image.patch`, and — for anything that must follow `.Release.Namespace`
or parent values — a patch making the subchart template `include` a parent helper (`kueue/manager-config.patch`).

## Workflow A — add a subchart

1. **Mirror every image it references first**, under `docker.io/gpustack/mirrored-*` (manual `sync-image.yml`
   dispatch). Unmirrored ⇒ every install `ImagePullBackOff`, and `make lint chart` does **not** catch it: it
   checks that the override knobs reach each reference, not that a reference resolves.
2. Add one line to the heredoc in `chart_staging()`: `<archive-url> <version> ${charts_dir}/<dir>`, then
   `make deps`.
3. The unpacked `Chart.yaml`'s `name:` **must equal `<dir>`** or Helm fails *"found in Chart.yaml, but missing
   in charts/ directory"*. If it differs, add a one-line `chart-rename.patch` (as `csi-driver-s3` does for
   upstream `csi-s3`) — not `alias:`, which breaks `helm dependency build`, i.e. `make lint chart`, whenever
   the directory name and the chart name differ.
4. Declare it in the parent `Chart.yaml` `dependencies`: `name`, `version`, `repository: ""` (Helm's
   convention for "already in `charts/`"), `condition: <name>.enabled`. Declaring is **mandatory** — an
   undeclared tree always renders, and a `condition` only applies to a declared dependency. `enabled:` there
   is **inert** (Helm resets declared dependencies to enabled before evaluating conditions); the switch exists
   only in the parent `values.yaml`.
5. Add the parent `values.yaml` block — its header holds the conventions; two are hard: the kebab-case key is
   **exactly the subchart's own name**, and every image **repository *and* tag** is stated in the parent, so a
   bump cannot move an image without the diff showing it. Expose a key only where you deliberately differ from
   upstream.
6. Write `global-image.patch` so `global.imageRegistry` / `imageNamespace` / `imagePullPolicy` /
   `imagePullSecrets` reach **every** image site, hook Jobs included — follow the nearest existing patch.
   `imageRegistry` **replaces** an existing registry segment (first path segment containing `.` or `:`).
7. Wire the rest — a missed site is silent, not a compile error: `templates/NOTES.txt` (reads subchart values
   as `index .Values "<kebab-name>"`); if the worker may install it, `applicationValuesKeys` in
   `pkg/worker/kuberess/apps.go` plus the overlay in `apps_gpustack_operator.go` (which
   `TestChartDefaultsMatchImageModeOverlay` compares against the chart's defaults); `files/cleanup.sh` if an
   older version installed it as its own release; `docs/development.md` → *Vendored subcharts*.
8. Run the [ladder](#verification-ladder).

## Workflow B — bump a pinned version

1. **Mirror the new version's images first** (A.1), and read the upstream diff for tag changes — the parent
   states each tag, so those edits are yours.
2. Edit that chart's line in `chart_staging()`: **both** the archive URL and the version field.
3. Same version in the parent `Chart.yaml` dependency, then `rm -f deploy/gpustack-operator/chart/Chart.lock`.
4. `make deps` — the stamp no longer matches, so the tree is deleted, re-extracted and re-patched. **A patch
   that no longer applies fails here, and that is the point**: it is the only signal upstream moved under a
   patch. Triage it with Workflow C.
5. Diff the new tree against the old version: new or renamed image sites need patching; new components stay
   off unless the operator uses them.
6. Run the ladder, including a before/after `helm template` diff read line by line.

## Workflow C — write, re-capture or rebase a patch

A `.rej` under `deploy/gpustack-operator/chart/charts/<name>` means a hunk found no home: upstream moved the
lines that hunk's context named. Re-apply that change by hand to the new templates, then capture. There is no
lesser failure to triage — a hunk that merely shifted position applies silently, so `make deps` failing is
always real drift, never bookkeeping.

### Capture, when the tree is already at the committed version

Diff from inside the tree so the paths are chart-root-relative (`a/templates/...`), which is what
`patch -p1 --directory <tree>` expects:

```bash
cd deploy/gpustack-operator/chart/charts/<name>
git diff --relative -- templates/<file> \
  > ../../../../../hack/deploy/gpustack-operator/chart/charts/<name>/<topic>.patch
```

### Capture, after a version bump — `git diff` is unusable here

The vendored tree's committed state is the **old version, already patched**, so a `git diff` against it mixes
the upstream version delta into the patch. Build a pristine baseline of the **new** version instead:

```bash
T=deploy/gpustack-operator/chart/charts/<name>; V=<new-version>; S=$(mktemp -d)
find "$T" -name '*.rej' -delete                                # else they land in the patch
curl -sSfL -o "$S/c.tgz" '<archive-url>'                      # extract exactly as chart_staging() does
tar -zxf "$S/c.tgz" --directory "$S" --strip-components 1 --no-same-owner && rm -f "$S/c.tgz" "$S/README.md"
printf '%s' "$V" > "$S/_VERSION_"                             # or the stamp reads as an addition
git -C "$S" init -q && git -C "$S" add -A && git -C "$S" commit -qm baseline
rsync -a --delete --exclude '.git' "$T"/ "$S"/                # overlay the patched tree make deps produced
git -C "$S" add -A
git -C "$S" diff --cached -- templates/<file> \               # scope to the paths THIS patch owns
  > hack/$T/<topic>.patch                                     # a multi-patch tree: once per patch
```

### Then, either way

Prove it re-applies from a clean extract — `make deps` skips an up-to-date tree, so a new patch does nothing
until the tree is gone, and `rm -rf` + `make deps` **destroys in-place edits** (the `.patch` is the only
durable copy, so capture first).

```bash
git diff -- deploy/gpustack-operator/chart/charts/<name> > /tmp/before.diff
rm -rf deploy/gpustack-operator/chart/charts/<name> && make deps
git diff -- deploy/gpustack-operator/chart/charts/<name> > /tmp/after.diff
diff /tmp/before.diff /tmp/after.diff     # must be empty
```

Name the file after its topic (patches apply alphabetically) and **never hand-edit a `.patch`** —
`.gitattributes` marks `hack/deploy/**/*.patch` as `-text` to keep context byte-exact, and an editor pass can
strip CRs. Regenerate it with the commands above.

## Workflow D — remove a subchart

1. Its line in `chart_staging()`; then `git rm -r` both
   `deploy/gpustack-operator/chart/charts/<name>` and `hack/deploy/gpustack-operator/chart/charts/<name>`.
2. The `dependencies` entry in the parent `Chart.yaml`, plus `rm -f deploy/gpustack-operator/chart/Chart.lock`.
3. The parent `values.yaml` block, then `make generate chart` (that is what drops it from `README.md` and
   `values.schema.json`).
4. Every other mention: `templates/NOTES.txt`, `files/cleanup.sh`, `applicationValuesKeys` + the overlay in
   `pkg/worker/kuberess/`, `chart_test.go` expectations, `docs/development.md`,
   `docs/migration/to-subcharts.md`, and the `gpustack-operator-chart-e2e` / `-e2e` skills.

**What an existing release keeps** — dropping the dependency cleans nothing up:

- A subchart's `crds/` (NFD has one) is applied **on install only**, never deleted: those CRDs and CRs stay.
- CRDs annotated `helm.sh/resource-policy: keep` (csi-driver-nfs's snapshot CRDs) are kept by design.
- Objects from a subchart's `templates/` **are** pruned by `helm upgrade`. For Kueue that means its CRDs
  (templated under `templates/crd/`) and every CR with them — and `kueue.x-k8s.io/resource-in-use` can leave
  them `Terminating` forever, failing every later install. Never drop `kueue` from a live release without the
  reap `files/migrate-pre.sh` performs: delete the webhook configurations **first**, because Kueue's
  validating webhook is `failurePolicy: Fail` and rejects the finalizer patch once its Service has no endpoints.
- Only the `cleanupOnUninstall` post-delete Job deletes CRDs, and its patterns cover `gpustack`/`kueue`/`nfd`.

## Verification ladder

1. **Re-vendor is reproducible** (CI: *Vendor Charts* + *Verify Vendored Charts*):

   ```bash
   make deps && make deps
   git status --porcelain -- deploy/gpustack-operator/chart/charts   # empty
   find deploy/gpustack-operator/chart/charts -name '*.tgz'          # nothing
   ```

2. **Generation is idempotent** (CI: *Verify Generated* compares `README.md`, `values.schema.json` **and**
   `values.yaml` — helm-schema writes the `$schema` line at the top of `values.yaml`):

   ```bash
   make generate chart && make generate chart
   git status --porcelain -- deploy/gpustack-operator/chart/{README.md,values.schema.json,values.yaml}
   ```

   Never hand-edit those two generated files; edit `values.yaml` / its `# --` annotations / `README.md.gotmpl`.

3. `make lint chart` — `ct lint` in a container, then `gpustack::helm::verify_images`: renders at the defaults
   with the four `global.*` image knobs set and asserts every rendered `image`, `imagePullPolicy` and
   `imagePullSecrets` honours them, subcharts included (knobs reaching references, not references resolving).
4. `go test ./pkg/worker/kuberess/...` — chart values held equal to `pkg/nodefeature`, plus an offline
   in-process render of the CSI driver names, manufacturer rows, PCI class whitelist, Kueue's rendered
   `resources.transformations` / `managedJobsNamespaceSelector`, and image-mode overlay parity.
5. **A before/after `helm template` diff, read line by line.** Pinned client, and pin the Kubernetes version —
   Kueue's `selectableFields` is gated at 1.31 and helm otherwise substitutes its own built-in version:

   ```bash
   ./.sbin/helm version --short                       # expect v3.21.0
   ./.sbin/helm template gpustack-operator deploy/gpustack-operator/chart --kube-version 1.33.0
   ```

6. `make test chart` — `ct install` in a container (docker + `~/.kube/config`, current context) into a
   namespace **chart-testing generates**, which is also what proves the chart names no namespace of its own.
   Needs a reachable Kubernetes cluster with **no conflicting release**: the chart templates cluster-scoped
   objects and Helm refuses any object carrying another release's ownership metadata. `ci/smoke-values.yaml`
   pins the operator image to `:dev` and sets `cleanupOnUninstall: true`, so the leg needs network egress.

   A **clean** `ct` cycle needs no cleanup — that flag handles it. An **interrupted** run leaves debris, and
   Helm names only one object per failure, so sweep by annotation instead of discovering them one run at a time:

   ```bash
   kubectl get clusterrole,clusterrolebinding,csidriver -o jsonpath='{range .items[?(@.metadata.annotations.meta\.helm\.sh/release-name)]}{.metadata.name}={.metadata.annotations.meta\.helm\.sh/release-name}{"\n"}{end}' | grep '=chart-'
   ```

   Also expect: the Kueue and NFD CRDs (whose cluster-scoped `ClusterQueue`/`ResourceFlavor` CRs hold
   `kueue.x-k8s.io/resource-in-use`, keeping the CRDs `Terminating` until
   `kubectl patch --type=merge -p '{"metadata":{"finalizers":null}}'` clears it), and the one object outside
   the release namespace: `RoleBinding/kueue-visibility-server-auth-reader` in **kube-system**.

7. Optional: the `gpustack-operator-chart-e2e` skill (install → version consistency → uninstall);
   `gpustack-operator-e2e` for scheduling-chain behaviour.

## Things that bite

- **A PATH `helm` shadows the pin.** `gpustack::helm::helm::validate` returns early on any PATH `helm` and
  `make deps` never installs one; a system 3.13 has no `--take-ownership`. Use `./.sbin/helm` and assert
  `version --short` is v3.21.0. (chart-testing runs its own bundled helm in a container; the pin is moot there.)
- **`Chart.lock` goes stale silently.** `helm template`/`lint` ignore it; `helm dependency build` — which
  chart-testing runs — fails *"the lock file (Chart.lock) is out of sync with the dependencies file"*. Delete
  it after any `Chart.yaml` dependency edit.
- **`make deps` is a no-op on an up-to-date tree** — a new or changed patch does nothing until the tree (or
  its `_VERSION_`) is gone.
- **`*.rej` is gitignored**, so nothing but `chart_staging()`'s own assertion reveals a half-applied patch.
  Rebase it per Workflow C; never work around the error.
- **helm-schema deliberately skips subchart values** (`--dependencies-filter=none`) because NFD's `values.yaml`
  carries a comment its parser rejects; a subchart's keys are absent from `values.schema.json` by design.
- **helm-docs is restricted to the parent chart**, else it writes a generated `README.md` into every vendored
  tree — which is also why `chart_staging()` deletes the upstream `README.md`.
