# Development

Build, lint and regenerate the operator with `make`. The same tooling manages vendored Helm
charts and patched Kubernetes dependencies. Read [Architecture](../getting-started/architecture.md)
before changing the operator's behavior.

## Contents

- [Commands](#commands)
- [Documentation site](#documentation-site)
- [Checks that can report false success](#checks-that-can-report-false-success)
- [Shipped specification corrections](#shipped-specification-corrections)
- [Runtime log verbosity](#runtime-log-verbosity)
- [API groups & code generation](#api-groups--code-generation)
- [Vendored / patched dependencies](#vendored--patched-dependencies)

## Commands

`make <target> [args]` runs `hack/<target>.sh` and forwards `args` to it. All builds use `CGO_ENABLED=1`,
`GODEBUG=gotypesalias=0` and the build tags `goccy netgo`.

- `make deps` — vendor patched k8s staging modules into `staging/` **and** upstream Helm charts into `deploy/gpustack-operator/chart/charts/`, then `go mod tidy && go mod download`; `make deps update` adds `go get -u ./...`.
- `make generate` — the `gen/api` generators: deepcopy, register, apiservice, CRDs, conversion, protobuf, webhooks. `make generate binding` regenerates the CGO bindings in `binding/` via c-for-go.
- `make lint` — golangci-lint (`.golangci.yaml`); `make lint dirty` also fails on a dirty tree. `make lint docs` checks documentation and spec structure, shared routing and the generated Hugo site, with no cluster; see [the docs skill](../../.claude/skills/gpustack-operator-docs/SKILL.md).
- `make build` — cross-build `cmd/gpustack-operator` into `.dist/build/`, version ldflag-injected into `pkg/utils/version`; `VERSION=vX.y.z+l.m make build` sets it, `BUILD_PLATFORMS="linux/amd64 linux/arm64"` cross-compiles.
- `make test` — `go test -v -failfast -race -cover -shuffle=on -timeout=30m ./...`, coverage to `.dist/test/coverage.out`. The order is shuffled on every run so an order-dependent test cannot hide behind the fixed one; a failure banner prints the seed, and `go test -shuffle=<seed>` reproduces that exact order. Trailing args are regexes of packages to **exclude**. `RACE=false make test` drops `-race` and changes nothing else.
- `make package` — images via `docker buildx` from `pack/*/Dockerfile` (Linux only).

CI (`hack/ci.sh`) runs `make generate && make deps && make lint && make build` inside the image build. The unit tests run
separately, in `test.yml`, as `RACE=false make test` on linux/amd64 and linux/arm64.

An image built from a git worktree carries the worktree's `.git` pointer file but not the gitdir it names, so git cannot read
the tree inside the build. There the `.agents` shell gate, `hack/check/agents-shell.sh`, reports itself skipped with its reason
instead of failing the build: in local mode it checks only uncommitted changes, so inside an image its set is empty even from a
clone. The verdict on committed `.agents` shell comes from `agents-shell.yml` and from `make lint` on the host.

> **`make lint` writes.** It runs `goimports-reviser -output=file` and `golangci-lint --fix`, so it
> edits the source rather than only reading it. Anything generated *before* it — `make generate`,
> `make generate chart` — was produced from a version of the source that no longer exists, and
> nothing downstream notices: the build passes, the tests pass, and `git status` is clean. Regenerate
> after linting. `api.yml` and `chart.yml` both fail when the committed artifacts do not match a
> fresh regeneration, which is what catches it when nobody remembers.

> **The image build lints with its own golangci-lint**, and it can be stricter than the one on your
> host: the packaged build runs `make lint` inside the image, where the pinned `.golangci.yaml`
> applies against the image's golangci-lint release, and a rule your host binary does not enable —
> `predeclared`, which refuses parameter names like `new` — fails there while the tree passed
> locally minutes earlier. When the packaged build fails a rule your host never reported, fix the
> name rather than the config: the image's verdict is the one CI ships.

### Helm chart

`generate`, `lint` and `test` take a `chart` argument, operating on `deploy/gpustack-operator/chart` via
[chart-testing](https://github.com/helm/chart-testing),
[helm-docs](https://github.com/norwoodj/helm-docs) and helm-schema:

- `make generate chart` — regenerate `README.md` (from `README.md.gotmpl`) and `values.schema.json`. **Never hand-edit those two**: edit `values.yaml`/its annotations/`README.md.gotmpl` and re-run, after **any** `values.yaml` edit — the generated schema is what rejects a bad install. Nothing in `values.yaml` is generated: Kueue's `resources.transformations` is rendered at install time by a chart helper a patch adds to its config.
- `make lint chart` — `ct lint` in a container, then assert the `global.*` image knobs reach every image the chart and its subcharts render (`gpustack::helm::verify_images`).
- `make test chart` — `ct install` onto the current cluster in a container; needs a reachable cluster (e.g. kind) and `~/.kube/config`. It installs `CHART_TEST_IMAGE_REPOSITORY`:`CHART_TEST_IMAGE_TAG`, by default the published `gpustack/gpustack-operator:dev`, which is built from `main`. To test a chart change together with the binary it needs, build the tree, load the image into the cluster, and name it in those two variables. The Chart workflow does this on every run.

### Vendored subcharts

Kueue, Node Feature Discovery, `csi-driver-nfs` and `csi-driver-s3` are vendored unpacked under
`deploy/gpustack-operator/chart/charts/<name>/` and committed, so `helm install` works from a bare
clone and CI stays offline.

`gpustack::chart_staging` (`hack/deps.sh`) pulls each pinned archive, unpacks it, stamps `_VERSION_` and
applies `hack/deploy/gpustack-operator/chart/charts/<name>/*.patch`; a tree at the pinned stamp is skipped,
so runs are idempotent and a patched tree is never clobbered.

Unpacked is what lets the patches exist: Helm merges subchart values rather than rendering them, so the
parent cannot compose `global.imageRegistry` into a subchart's `image.repository`; each tree's
`global-image.patch` makes the subchart's templates read `.Values.global.*`, which Helm does propagate.

To change an upstream chart:

1. **Never edit a staged tree in place**: a version bump makes `make deps` delete and re-unpack it, so
   every change lives in a patch file.
2. Write the patch against the unpacked tree (`git diff` from a scratch copy works), drop it into
   `hack/deploy/gpustack-operator/chart/charts/<name>/`, bump the pinned version in `hack/deps.sh` if that
   is the change, and re-run `make deps`.
3. A patch that no longer applies, or leaves a `.rej`, **fails `make deps`**; otherwise a moved chart
   ships half-patched and silent. A **shifted** hunk is fine: `patch` runs `-F0`, so context still matches
   exactly, and two patches on one file shift each other.

**Mirror the images before bumping a pinned version**: every image the chart renders points at a
`gpustack/mirrored-*` repository, an unmirrored bump lands every install in `ImagePullBackOff`, and
`make lint chart` only checks that the override knobs reach each reference, not that it resolves.

`chart.yml` runs all three across the supported Kubernetes matrix and gates drift: `make generate chart`
must leave `README.md`/`values.schema.json` unchanged, `make deps` the vendored trees; both fail with the
command to run. For a full install → version-consistency → uninstall cycle on a real cluster use the
`gpustack-operator-chart-e2e` skill, and `gpustack-operator-e2e` for scheduling-chain behavior.

### Commit messages

[Conventional Commits](https://www.conventionalcommits.org) (`type: subject`), checked by
[commitsar](https://github.com/aevea/commitsar) (`v1.0.3`, pinned in `hack/lib/style.sh`) within
`make lint`, but only over a clean tree (`hack/lint.sh` → `gpustack::commit::lint`) and only across the
commits ahead of `origin/main` (scope in `.commitsar.yml`). Types: `feat`, `fix`, `refactor`, `test`,
`docs`, `chore`.

### Pull request review

`code-review.yml` runs an AI review on every pull request (opened/synchronize/reopened) and posts
inline findings plus a summary under the org's GitHub App identity. The pipeline itself lives in
[`gpustack/.github`](https://github.com/gpustack/.github/blob/main/.github/workflows/code-review.yml)
as a reusable workflow — this repository only pins its model backing and maps the org secrets. To
re-review the latest head, comment `/open-code-review` on the PR (MEMBER/OWNER/COLLABORATOR only).

What each file is reviewed *against* is owned here, in `.opencodereview/rule.json`: its
include/exclude lists and path-scoped rules are the single source of truth, and a review-scope change
lands there, not in this document. Verify a rule change before committing: `npx -y
@alibaba-group/open-code-review rules check <path>` shows which rule a file resolves to; `npx -y
@alibaba-group/open-code-review review --preview` shows which files a diff would send to review.

### Running a single test

`make test` only excludes packages, so target one package or test with `go test` directly:

```bash
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race ./pkg/nodefeature/...
GODEBUG=gotypesalias=0 CGO_ENABLED=1 go test -race -run TestExtractGeneralNodeKey ./pkg/nodefeature/
```

## Documentation site

Build the site or start a live preview from the repository root:

```bash
make site
make site-serve
```

`make site` renders the Markdown under `docs/` into `site/public/` and checks the rendered site's
links and anchors. `make site-serve` opens the preview at `http://localhost:1313/` and rebuilds it
as you edit pages, navigation or styles. Stop the preview with Ctrl-C.

Both commands validate the pinned Hugo version. When needed, the installer builds extended Hugo
into `.sbin/`, using Go and a C compiler; its first installation needs network access. The Mermaid
script and license are bundled under `site/assets/vendor/mermaid/`, so site builds do not fetch them.

Edit documentation under `docs/`, navigation labels and order in `site/data/navigation.yaml`, and
layouts and styles under `site/`. Run `make lint docs` before committing to check source links,
page structure, index entries and rendered site links. CI runs the same gate.

The [shared module map](../README.md#module-map) connects guides, specs, code and skills for code
changes. Update its routing in the same PR when those entry points or associations change.
`make site` also generates `/llms.txt` from that index and `index.md` beside each article's HTML.
Markdown exports preserve the prose and code blocks, with links adjusted for the website.
Hugo watches the shared index and article sources during `make site-serve`.

`make lint docs` runs the same build and checks module coverage, skill inputs, generated index
coverage, Markdown content and local links. These checks cannot establish whether a design record
still describes the implementation; verify that against the relevant code and spec status.

### Publishing

The Site workflow publishes `main` after merges and a separate site for each `v*` version tag.
It writes documentation alongside the charts on `github-pages`, then deploys the complete branch
through GitHub Pages. Chart publication and both deployment jobs share a queue so concurrent runs
preserve each other's files. The documentation composer verifies that chart bytes stay unchanged.

| Path | Content |
|---|---|
| `/` | Redirect to the latest published stable documentation. |
| `/main/` | Development documentation from the current `main` branch. |
| `/v0.9.0/` | Documentation built from that exact stable tag. |
| `/v0.9.0-rc2/` | Preview built from that exact prerelease tag. |
| `/charts/index.yaml` | Existing Helm chart index; its address stays unchanged. |
| `/versions.json` | Generated version list and source revisions. |
| `/llms.txt` | Index for the documentation selected by the root redirect. |

These paths are relative to `https://docs.gpustack.ai/gpustack-operator/`. A stable version has a
`vMAJOR.MINOR.PATCH` tag without a prerelease suffix. Only versions actually published by the Site
workflow count when selecting the default; until the first stable site exists, the default is `main`.
Tags that predate the site sources cannot be rebuilt using future documentation.

Prerelease sites have `noindex` metadata. Publishing the matching stable version hides those previews
from the version menu, while keeping their direct URLs and source revisions for troubleshooting.
Older stable versions remain available. Published tags cannot be moved to a different commit;
`main` is the only site whose source revision changes over time.

To retry publication, run the Site workflow with `ref` set to `main` or an existing version tag.
It uses the repository's `GITHUB_TOKEN` with contents, Pages and identity-token permissions;
a separate personal access token is not required. The repository must use the GitHub Actions Pages
source, and its `github-pages` environment must allow deployment from `main` and version tags.

Before a release, run the production-path build locally as well as `make lint docs`:

```bash
SITE_BASE_URL=https://docs.gpustack.ai/gpustack-operator/main/ make site
```

This catches links that work at the preview root but lose their version prefix when published.
The first RC after this workflow merges should also verify the deployed redirect, search, version
menu, chart index and package downloads. A local build cannot confirm Pages environment permissions.

### Translations

Hugo assigns unsuffixed Markdown files to English. Chinese is configured but disabled until its
content is ready. Add translations beside the originals, for example `requests.zh.md` beside
`requests.md`, and `_index.zh.md` beside a section's `_index.md`. Keep file stems the same so Hugo
can associate translated pages. English URLs stay unchanged; Chinese uses `/zh/` within each version.

Before enabling Chinese, translate the site home and search page under `site/content/`, navigation
labels and interface text, and update the source index and documentation checks for the translated
pages. Remove `zh` from `disableLanguages` in `site/hugo.toml` once that work is ready. A language
menu links only translations that actually exist; each language builds its own search index.


## Checks that can report false success

REQUIRED: take a check's verdict from its **return code** and from the object under test, never from
the shape of its output. The traps below can make a failed check look successful when stderr, a
return code or missing output goes unnoticed. The shell examples use the interactive shell you
type these commands into, which on macOS is zsh; the repository's own scripts run under bash with `pipefail` set.

**An unquoted `$var` does not word-split in zsh.** `FILES="a b c"; cp $FILES $dir` passes the list
as one filename and the copy fails, where bash would split it into three arguments. Keep a list in
an array and expand it as `"${FILES[@]}"`: a bare `$FILES` over an array is three words in zsh but
only its first element in bash. The comparison downstream then read a match, because both sides were
the empty string a failed `git hash-object` returned.

**zsh arrays are 1-indexed.** A `for i in 0 1 2 3 4` loop over `${IDS[$i]}` drops one end. Two
arrays stepped together stay aligned, so the other iterations land correctly and the single missing
one reads as a flake rather than as a boundary error.

**A pipeline reports only its last command.** `make lint | tail -5; echo "rc=$?"` gives `tail`'s
status, not lint's, unless `pipefail` is set, and neither zsh nor bash sets it by default. A green
lint prints nothing after its closing banner, so "the code is 0" and "the last line is the banner"
confirm each other. Redirect instead: `make lint >/tmp/out.log 2>&1; echo "RC=$?"`.

**An empty result is not a negative result.** A `grep -c` of `0` is a real count from whatever search
ran, and a dropped `-i` quietly changes which search that is. A command that never ran prints no
count at all: a missing path, or `-P` on the macOS grep, exits `2` with its message on stderr, where
`|| echo none` prints the reassuring branch over it. Feed the check an input it MUST match first.

**Let the check veto the cleanup.** Verification and teardown joined by `;` tear down even when the
verification failed, costing the evidence needed to diagnose it; `&&` is the guard, and it guards
only a check that exits non-zero. Carry the verdict in the status
(`[ -n "$a" ] && [ -n "$b" ] && [ "$a" = "$b" ] && rm -rf "$tree"`) rather than printing `SAME` or
`DIFF` and returning `0` either way. The non-empty tests matter: two missing values compare equal.

## Shipped specification corrections

Shipped specifications are historical design records. A later design change MUST be recorded in a new
specification whose header names the earlier sections it supersedes; leave those sections unchanged.

An in-place edit is ALLOWED only when evidence proves a factual claim or its supporting reason wrong
while the shipped design and conclusion remain unchanged. Mark prose `**Corrected after shipping.**`, or
use `Corrected after shipping.` inside a preserved code block. Retain enough of the former claim to
explain the correction, and state the replacement evidence at the same location. Keep the edit to that
correction; terminology, formatting and later design belong elsewhere.

**A bug fix writes no NEW specification into this repository.** Three shipped ones predate that rule
and stay exactly where they are, as the historical records they already were. What it changes is the
FIRST paragraph above: a design change a bug fix makes has no new specification to be recorded in, so
recording it there is impossible rather than merely skipped.

Record it in the superseded section instead, marked `**Corrected after shipping.**`. That is the SECOND
paragraph's marker carrying one thing that paragraph otherwise forbids — the resulting rule does change
here — so the note MUST name the page that carries the rule now. Everything else in that paragraph
still applies: retain enough of the former text to explain what changed, and keep the edit to it.

A superseded section MUST NOT be left carrying a **file name** that resolves to nothing; the reader
follows it and lands nowhere. Naming the document is fine and often necessary; what must go is the
extension that makes the name a path. So write `2026-01-02-a-thing` rather than `2026-01-02-a-thing.md`
once that file is gone. The prohibition is on the dangling path, not on the mention.

## Runtime log verbosity

Every component registers `PUT /debug/flags/v` on its own secure port, so klog verbosity can be raised on
a running pod and dropped again without a restart (`pkg/manager/manager.go`, `pkg/worker/worker.go`,
`pkg/workergateway/gateway.go`; the device-manager's port is 32443, from `pkg/devicemanager/option.go`).

```bash
kubectl -n gpustack-system exec <pod> -- \
  curl -sk -X PUT -H "Host: 127.0.0.1" -d '4' https://127.0.0.1:32443/debug/flags/v
# → successfully set klog.logging.verbosity to 4
kubectl -n gpustack-system exec <pod> -- \
  curl -sk -X PUT -H "Host: 127.0.0.1" -d '2' https://127.0.0.1:32443/debug/flags/v
```

`-H "Host: 127.0.0.1"` is **mandatory**: `httpx.LoopbackAccessHandlerFunc` compares `r.Host` against the
bare `127.0.0.1` / `localhost` / `::1` and an ordinary request carries the port (`127.0.0.1:32443`), so
without it the guard answers a plain `404` that reads like a missing route. A `GET` with the header
answers `406 unsupported http method`, which is the guard passing.

Use it to see a decision logged above the deployment's verbosity. The device plugin is the sharpest case:
its `ResourceServer`s use `Logger: logger.V(3)` (`pkg/devicemanager/allocator/allocator.go`) while the
DaemonSet runs `-v=2`, so `Allocate`/`GetPreferredAllocation` decisions (which accelerator a slice landed
on) are discarded by default.

Raise `v` **before** creating the workload to trace; those lines fire only on an allocation, so a quiet
window afterwards proves nothing. The `gpustack-operator-e2e` skill carries the same recipe as a triage
step, with the operational caveats.

## API groups & code generation

| Path | Group / Version | Kind |
|------|-----------------|------|
| `api/v1` | `gpustack.ai/v1` | Extension API (settings, status) |
| `api/worker/v1` | `worker.gpustack.ai/v1` | Public API served by the aggregated apiserver, including resource proxies and the read-only (get, list, watch) `InstanceTypeFlavor` catalog |
| `api/worker/v1alpha1` | internal storage | Controller-managed CRDs behind the public API |

`gen/api/main.go` configures which packages are CRDs vs extension APIs and drives the custom generators in
`gen/api/generator` (apireg-gen, crd-gen, webhook-gen). **Never hand-edit generated files**
(`zz_generated.*`, `generated.pb.go`, `generated.proto`): edit the source `*.go` types or `gen/api/main.go`
and run `make generate` (the `gpustack-operator-generate` skill automates this).

The patched applyconfiguration generator strips list and continuation indentation from API comments
in `staging/k8s.io/code-generator/cmd/applyconfiguration-gen/generators/applyconfiguration.go:317-326`
(`commentsWithoutMarkers`). A comment scan of `pkg/kubeclients/applyconfiguration/` can therefore report
list formatting that cannot be fixed by editing the source comment; fix the generator patch instead.

## Vendored / patched dependencies

`go.mod` replaces several Kubernetes modules (`k8s.io/api`, `apimachinery`, `code-generator`,
`apiextensions-apiserver`, `kube-aggregator`, `klog`), `gogo/protobuf` and `go-logr/logr` with patched
copies under `./staging/`. `make deps` checks out their versions from `hack/deps.sh` and applies
patches from `hack/staging/`. Change the patch and re-run `make deps`; do not hand-edit `staging/`.

`make deps` also stages the subcharts. Their versions are in `hack/deps.sh` and their patches
are under `hack/deploy/`.

---

**See also** — [Internals](internals.md) (the invariants the code keeps) ·
[Installation Modes](../operate/installation-modes.md) · [Settings](../reference/settings.md)

**Next** → [All documentation](../README.md) — pick the next page on the contributor path.
