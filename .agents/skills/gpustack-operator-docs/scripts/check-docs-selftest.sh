#!/usr/bin/env bash
# Self-test for check-docs.sh covering legacy mode and strict three-zone mode.
set -euo pipefail

ROOT_DIR="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)}"
CHECKER="${ROOT_DIR}/.agents/skills/gpustack-operator-docs/scripts/check-docs.sh"
WORK_ROOT="$(mktemp -d)"
trap 'rm -rf "${WORK_ROOT}"' EXIT

expect_pass() {
  local name="$1"
  shift
  if ! bash "${CHECKER}" "$@" >"${WORK_ROOT}/out.txt" 2>&1; then
    echo "check-docs self-test (${name}): unexpectedly failed:" >&2
    cat "${WORK_ROOT}/out.txt" >&2
    exit 1
  fi
}

expect_fail() {
  local name="$1" dir="$2" pattern="$3"
  shift 3
  if bash "${CHECKER}" "$@" "${dir}" >"${WORK_ROOT}/out.txt" 2>&1; then
    echo "check-docs self-test (${name}): unexpectedly passed" >&2
    exit 1
  fi
  if ! grep -qE "${pattern}" "${WORK_ROOT}/out.txt"; then
    echo "check-docs self-test (${name}): missing expected diagnostic '${pattern}'" >&2
    cat "${WORK_ROOT}/out.txt" >&2
    exit 1
  fi
}

# Legacy mode (no docs/reference/README.md).
LEGACY="${WORK_ROOT}/legacy"
mkdir -p "${LEGACY}/docs/reference" "${LEGACY}/site/content" "${LEGACY}/.agents/skills"
cat >"${LEGACY}/README.md" <<'EOF'
# Fixture
EOF
cat >"${LEGACY}/docs/README.md" <<'EOF'
# Fixture index

## All pages

| Page | Link |
| --- | --- |
| [Fixture page](x.md) | page |
| [Settings](reference/settings.md) | settings |
EOF
cat >"${LEGACY}/docs/x.md" <<'EOF'
# Fixture page

Short introduction linking to [Settings](reference/settings.md).

## Contents

- [Step 1: Create Pod](#step-1-create-pod)
- [Example](#example)

## Step 1: Create Pod

Run the command.

#### Allowed fourth level

Text under H4.

## Example

```markdown
## Step 99: this heading is intentionally long and lowercase
```

---

**See also** — [Fixture index](README.md)
EOF
cat >"${LEGACY}/docs/reference/settings.md" <<'EOF'
# Settings

Short introduction.

## Contents

- [Values](#values)

## Values

Configure `Pods` enum before strict cutover.

---

**See also** — [Fixture page](../x.md)

**Next** -> [Fixture page](../x.md)
EOF

expect_pass "legacy-baseline" "${LEGACY}"

sed -i.bak \
  -e 's/^- \[Step 1: Create Pod\](#step-1-create-pod)$/- [Step 1: this heading is intentionally long and lowercase](#step-1-this-heading-is-intentionally-long-and-lowercase)/' \
  -e 's/^## Step 1: Create Pod$/## Step 1: this heading is intentionally long and lowercase/' \
  "${LEGACY}/docs/x.md"
rm -f "${LEGACY}/docs/x.md.bak"
expect_fail "legacy-heading-case" "${LEGACY}" "capitalize the first word after the numbered prefix"
grep -q "heading topic is" "${WORK_ROOT}/out.txt"

# Strict three-zone baseline (docs/reference/README.md present).
STRICT_BASE="${WORK_ROOT}/strict-base"
mkdir -p \
  "${STRICT_BASE}/docs/getting-started" \
  "${STRICT_BASE}/docs/reference" \
  "${STRICT_BASE}/docs/contribute" \
  "${STRICT_BASE}/site/content/docs" \
  "${STRICT_BASE}/.agents/skills/demo"

cat >"${STRICT_BASE}/README.md" <<'EOF'
# Operator
EOF

cat >"${STRICT_BASE}/.agents/skills/demo/SKILL.md" <<'EOF'
# Demo skill

Negative examples allowed in skills: `Pods`, `Nodes`, `Instances`, `ModelDeployments`,
`ModelArtifacts`, `Workloads`, `Topology`s, `Devices`s.
Valid path reference: docs/getting-started/guide.md and docs/reference/README.md.
EOF

cat >"${STRICT_BASE}/docs/README.md" <<'EOF'
# Operator Documentation

## All pages

| Page | Description |
| --- | --- |
| [Operator Guide](getting-started/guide.md) | Configure operator workloads |

## Reference

| Page | Description |
| --- | --- |
| [Reference Hub](reference/README.md) | Flat LLM navigation hub |
| [Guide Parameters](reference/guide-parameters.md) | Exhaustive parameter table |

## Contribute

| Page | Description |
| --- | --- |
| [Development](contribute/development.md) | Build and test commands |
EOF

cat >"${STRICT_BASE}/docs/getting-started/guide.md" <<'EOF'
# Operator Guide

Configure `Pod`s, `Node`s, `Instance`s, `ModelDeployment`s, `ModelArtifact`s, `Workload`s,
`Topology` resources, and `Devices` objects. See [Makefile](../../README.md) and [third retry](#retry-2).

## Contents

- [Step 1: Create workload](#step-1-create-workload)
- [Exemptions](#exemptions)

## Step 1: Create workload

Use `--nodes`, `kubectl get pods`, `/pods`, `/api/v1/namespaces/{namespace}/pods`, `pods: 10`,
`"nodes": []`, `type: Pods`, and `"Pods"`.

### Retry

First retry.

### Retry-1

Explicit retry-1 heading.

### Retry

Third retry allocates retry-2 without colliding with retry-1.

## Exemptions

```yaml
#### Ignored fenced H4
# [Ignored fenced link](../reference/guide-parameters.md)
# Ignored fenced `Pods`
```

    #### Ignored four-space indented H4
    [Ignored indented link](../contribute/development.md) `Nodes`

<!--
#### Ignored HTML comment H4
[Ignored comment link](/reference/README.md) `Instances`
-->

Inline code lookalikes `[ignored](../reference/README.md)` and `<https://docs.gpustack.ai/reference/README.md>` stay exempt.[^1]

[^1]: Footnote citing [Operator](../../README.md).

    Continuation paragraph in footnote citing [Operator](../../README.md).

---

**See also** — [Operator](../../README.md) (project overview)

**Next** -> [Operator](../../README.md) — continue setup.
EOF

cat >"${STRICT_BASE}/docs/reference/README.md" <<'EOF'
# Reference Hub

Flat index for coding agents.

## Task routing

| Task | Reference | Public |
| --- | --- | --- |
| Parameters | [Guide Parameters](guide-parameters.md) | [Operator Guide](../getting-started/guide.md) |

## Exact aliases

| Alias | Reference |
| --- | --- |
| `stepPolicy` | [Guide Parameters](guide-parameters.md#parameters) |

## Evidence labels

| Reference | Evidence |
| --- | --- |
| [Guide Parameters](guide-parameters.md) | contract |

---

**See also** — [Operator Guide](../getting-started/guide.md) (operator workflow)
EOF

cat >"${STRICT_BASE}/docs/reference/guide-parameters.md" <<'EOF'
# Guide Parameters

Evidence: contract. Parameter defaults for `Pod`s.

## Contents

- [Parameters](#parameters)

## Parameters

See [Reference Hub](README.md), [Operator Guide](../getting-started/guide.md#retry-2), and
[Development](../contribute/development.md).

---

**See also** — [Reference Hub](README.md) (all reference tables)
EOF

cat >"${STRICT_BASE}/docs/contribute/development.md" <<'EOF'
# Development

Build and verify the operator.

## Contents

- [Commands](#commands)

## Commands

Read [Reference Hub](../reference/README.md) and [Operator Guide](../getting-started/guide.md).

---

**See also** — [Operator Guide](../getting-started/guide.md) (user setup)

**Next** -> [Reference Hub](../reference/README.md) — inspect contracts.
EOF

cat >"${STRICT_BASE}/site/content/_index.md" <<'EOF'
---
title: Home
---

Schedule `Pod`s with [Operator Guide](docs/getting-started/guide/).
EOF

cat >"${STRICT_BASE}/site/content/docs/_index.md" <<'EOF'
---
title: Documentation
---

# Documentation

Start at [Operator Guide](getting-started/guide/).
EOF

expect_pass "strict-baseline" "${STRICT_BASE}"
expect_pass "strict-baseline-report" --report "${STRICT_BASE}"

clone_strict() {
  local dst="${WORK_ROOT}/$1"
  rm -rf "${dst}"
  cp -R "${STRICT_BASE}" "${dst}"
  printf '%s\n' "${dst}"
}

# Heading depth ceiling (H4-H6 and blockquoted H4 forbidden in docs/**, even under --report).
T="$(clone_strict h4-public)"
printf '\n#### Forbidden fourth level\n' >>"${T}/docs/getting-started/guide.md"
expect_fail "strict-h4-public" "${T}" "exceeds max depth H3"
expect_fail "strict-h4-report" "${T}" "exceeds max depth H3" --report

T="$(clone_strict h4-blockquoted)"
printf '\n> #### Blockquoted fourth\n' >>"${T}/docs/getting-started/guide.md"
expect_fail "strict-h4-blockquoted" "${T}" "exceeds max depth H3"

T="$(clone_strict h5-ref)"
printf '\n##### Forbidden fifth level\n' >>"${T}/docs/reference/guide-parameters.md"
expect_fail "strict-h5-ref" "${T}" "exceeds max depth H3"

T="$(clone_strict h6-contrib)"
printf '\n###### Forbidden sixth level\n' >>"${T}/docs/contribute/development.md"
expect_fail "strict-h6-contrib" "${T}" "exceeds max depth H3"

# Duplicate heading slug collision check (retry-3 does not exist).
T="$(clone_strict bad-retry-anchor)"
printf '\nBad [retry-3](#retry-3).\n' >>"${T}/docs/getting-started/guide.md"
expect_fail "bad-retry-anchor" "${T}" "no such anchor"

# Flat docs/reference/ layout (reject subdirectories).
T="$(clone_strict ref-subdir)"
mkdir -p "${T}/docs/reference/nested"
cp "${T}/docs/reference/guide-parameters.md" "${T}/docs/reference/nested/extra.md"
expect_fail "strict-ref-subdir" "${T}" "subdirectory forbidden under flat docs/reference/"

# One-way link isolation across public docs/ and site/content/**.
T="$(clone_strict iso-rel-ref)"
printf '\nSee [Ref](../reference/guide-parameters.md).\n' >>"${T}/docs/getting-started/guide.md"
expect_fail "iso-rel-ref" "${T}" "public page links to excluded zone"

T="$(clone_strict iso-rel-contrib)"
printf '\nSee [Dev](../contribute/development.md).\n' >>"${T}/docs/getting-started/guide.md"
expect_fail "iso-rel-contrib" "${T}" "public page links to excluded zone"

T="$(clone_strict iso-root-ref)"
printf '\nSee [Ref](/reference/guide-parameters.md).\n' >>"${T}/docs/getting-started/guide.md"
expect_fail "iso-root-ref" "${T}" "public page links to excluded zone"

T="$(clone_strict iso-root-docs-ref)"
printf '\nSee [Ref](/docs/reference/guide-parameters/).\n' >>"${T}/docs/getting-started/guide.md"
expect_fail "iso-root-docs-ref" "${T}" "public page links to excluded zone"

T="$(clone_strict iso-root-contrib)"
printf '\nSee [Dev](/docs/contribute/development/).\n' >>"${T}/docs/getting-started/guide.md"
expect_fail "iso-root-contrib" "${T}" "public page links to excluded zone"

T="$(clone_strict iso-same-origin)"
printf '\nSee [Ref](https://docs.gpustack.ai/gpustack-operator/main/reference/guide-parameters.md).\n' >>"${T}/docs/getting-started/guide.md"
expect_fail "iso-same-origin" "${T}" "public page links to excluded zone"

T="$(clone_strict iso-root-alias-no-version)"
printf '\nSee [Ref](https://docs.gpustack.ai/gpustack-operator/reference/settings.md).\n' >>"${T}/docs/getting-started/guide.md"
expect_fail "iso-root-alias-no-version" "${T}" "public page links to excluded zone"

T="$(clone_strict iso-root-alias-protocol-relative)"
printf '\nSee [Ref](//docs.gpustack.ai/gpustack-operator/reference/settings.md).\n' >>"${T}/docs/getting-started/guide.md"
expect_fail "iso-root-alias-protocol-relative" "${T}" "public page links to excluded zone"

T="$(clone_strict iso-root-alias-path)"
printf '\nSee [Ref](/gpustack-operator/reference/settings.md).\n' >>"${T}/docs/getting-started/guide.md"
expect_fail "iso-root-alias-path" "${T}" "public page links to excluded zone"

T="$(clone_strict iso-origin-upper-port)"
printf '\nSee [Ref](HTTPS://DOCS.GPUSTACK.AI:443/reference/guide-parameters.md).\n' >>"${T}/docs/getting-started/guide.md"
expect_fail "iso-origin-upper-port" "${T}" "public page links to excluded zone"

T="$(clone_strict iso-protocol-relative)"
printf '\nSee [Ref](//docs.gpustack.ai/gpustack-operator/main/docs/contribute/development/).\n' >>"${T}/docs/getting-started/guide.md"
expect_fail "iso-protocol-relative" "${T}" "public page links to excluded zone"

T="$(clone_strict iso-percent-encoded)"
printf '\nSee [Ref](/docs/%%72eference/guide-parameters/).\n' >>"${T}/docs/getting-started/guide.md"
expect_fail "iso-percent-encoded" "${T}" "public page links to excluded zone"

T="$(clone_strict iso-multiline-indented-dest)"
cat >>"${T}/docs/getting-started/guide.md" <<'EOF'

See [indented dest](
    ../reference/guide-parameters.md).
EOF
expect_fail "iso-multiline-indented-dest" "${T}" "public page links to excluded zone"

T="$(clone_strict iso-blockquoted-refdef)"
cat >>"${T}/docs/getting-started/guide.md" <<'EOF'

> [bqref]: ../reference/guide-parameters.md
EOF
expect_fail "iso-blockquoted-refdef" "${T}" "public page links to excluded zone"

T="$(clone_strict iso-angle-autolink)"
printf '\nSee <https://docs.gpustack.ai/gpustack-operator/main/reference/guide-parameters.md>.\n' >>"${T}/docs/getting-started/guide.md"
expect_fail "iso-angle-autolink" "${T}" "public page links to excluded zone"

T="$(clone_strict iso-inline-code-comment)"
cat >>"${T}/docs/getting-started/guide.md" <<'EOF'

Use `<!--` to open a comment and see [Ref](../reference/guide-parameters.md).
EOF
expect_fail "iso-inline-code-comment" "${T}" "public page links to excluded zone"

T="$(clone_strict iso-footnote-continuation)"
cat >>"${T}/docs/getting-started/guide.md" <<'EOF'

Note.[^2]

[^2]: Footnote first line.

    Continuation with [Ref](../reference/guide-parameters.md).
EOF
expect_fail "iso-footnote-continuation" "${T}" "public page links to excluded zone"

T="$(clone_strict iso-site-content-rel)"
printf '\n- [Reference](reference/)\n' >>"${T}/site/content/docs/_index.md"
expect_fail "iso-site-content-rel" "${T}" "public page links to excluded zone"

T="$(clone_strict iso-site-content-root)"
printf '\nSee [Hub](/reference/README.md).\n' >>"${T}/site/content/_index.md"
expect_fail "iso-site-content-root" "${T}" "public page links to excluded zone"

# Three-table docs/README.md coverage, H1 labels, and reference hub/topic shape.
T="$(clone_strict missing-hub-row)"
grep -v 'reference/README.md' "${T}/docs/README.md" >"${T}/docs/README.md.tmp"
mv "${T}/docs/README.md.tmp" "${T}/docs/README.md"
expect_fail "missing-hub-row" "${T}" "docs/reference/README.md: not in the '## Reference' table"

T="$(clone_strict wrong-zone-table)"
grep -v 'contribute/development.md' "${T}/docs/README.md" >"${T}/docs/README.md.tmp"
mv "${T}/docs/README.md.tmp" "${T}/docs/README.md"
awk '
  /^\| \[Operator Guide\]/ {
    print
    print "| [Development](contribute/development.md) | Wrong table |"
    next
  }
  { print }
' "${T}/docs/README.md" >"${T}/docs/README.md.tmp"
mv "${T}/docs/README.md.tmp" "${T}/docs/README.md"
expect_fail "wrong-zone-table" "${T}" "belongs in '## Contribute', not '## All pages'"

T="$(clone_strict ref-label-mismatch)"
sed -i.bak 's/\[Guide Parameters\]/[Wrong Ref Label]/' "${T}/docs/README.md"
rm -f "${T}/docs/README.md.bak"
expect_fail "ref-label-mismatch" "${T}" "index label 'Wrong Ref Label'"

T="$(clone_strict contrib-label-mismatch)"
sed -i.bak 's/\[Development\]/[Wrong Contrib Label]/' "${T}/docs/README.md"
rm -f "${T}/docs/README.md.bak"
expect_fail "contrib-label-mismatch" "${T}" "index label 'Wrong Contrib Label'"

T="$(clone_strict hub-missing-seealso)"
grep -v 'See also' "${T}/docs/reference/README.md" >"${T}/docs/reference/README.md.tmp"
mv "${T}/docs/reference/README.md.tmp" "${T}/docs/reference/README.md"
expect_fail "hub-missing-seealso" "${T}" "docs/reference/README.md: no '\\*\\*See also\\*\\*' footer"

T="$(clone_strict hub-forbids-next)"
printf '\n**Next** -> [Guide Parameters](guide-parameters.md)\n' >>"${T}/docs/reference/README.md"
expect_fail "hub-forbids-next" "${T}" "docs/reference/README.md: reference page must omit '\\*\\*Next\\*\\*' footer"

T="$(clone_strict hub-missing-topic-link)"
cat >"${T}/docs/reference/extra-ref.md" <<'EOF'
# Extra Ref

Evidence: contract.

## Contents

- [Details](#details)

## Details

Body.

---

**See also** — [Reference Hub](README.md) (index)
EOF
awk '
  /^\| \[Guide Parameters\]/ {
    print
    print "| [Extra Ref](reference/extra-ref.md) | Extra reference |"
    next
  }
  { print }
' "${T}/docs/README.md" >"${T}/docs/README.md.tmp"
mv "${T}/docs/README.md.tmp" "${T}/docs/README.md"
expect_fail "hub-missing-topic-link" "${T}" "docs/reference/extra-ref.md: not linked from reference hub"

T="$(clone_strict hub-missing-aliases)"
awk '
  /^## Exact aliases$/ { skip = 1; next }
  skip && /^## /       { skip = 0 }
  !skip                { print }
' "${T}/docs/reference/README.md" >"${T}/docs/reference/README.md.tmp"
mv "${T}/docs/reference/README.md.tmp" "${T}/docs/reference/README.md"
expect_fail "hub-missing-aliases" "${T}" "missing exact aliases section or marker"

T="$(clone_strict hub-missing-evidence)"
awk '
  /^## Evidence labels$/ { skip = 1; next }
  skip && /^---$/        { skip = 0 }
  !skip                  { print }
' "${T}/docs/reference/README.md" >"${T}/docs/reference/README.md.tmp"
mv "${T}/docs/reference/README.md.tmp" "${T}/docs/reference/README.md"
expect_fail "hub-missing-evidence" "${T}" "missing evidence labels section"

T="$(clone_strict ref-topic-forbids-next)"
printf '\n**Next** -> [Reference Hub](README.md)\n' >>"${T}/docs/reference/guide-parameters.md"
expect_fail "ref-topic-forbids-next" "${T}" "docs/reference/guide-parameters.md: reference page must omit '\\*\\*Next\\*\\*' footer"

T="$(clone_strict ref-topic-missing-contents)"
awk '
  /^## Contents$/ { skip = 1; next }
  skip && /^## /  { skip = 0 }
  !skip           { print }
' "${T}/docs/reference/guide-parameters.md" >"${T}/docs/reference/guide-parameters.md.tmp"
mv "${T}/docs/reference/guide-parameters.md.tmp" "${T}/docs/reference/guide-parameters.md"
expect_fail "ref-topic-missing-contents" "${T}" "docs/reference/guide-parameters.md: missing the '## Contents' list"

# Authored-prose kind-plural checker across docs/** and site/content/**.
while IFS= read -r bad_token; do
  [ -n "${bad_token}" ] || continue
  T="$(clone_strict plural-docs)"
  printf '\nMalformed plural %s in prose.\n' "${bad_token}" >>"${T}/docs/getting-started/guide.md"
  expect_fail "plural-${bad_token}" "${T}" "kind plural"
  expect_fail "plural-report-${bad_token}" "${T}" "kind plural" --report
done <<'EOF'
`Pods`
`Nodes`
`Instances`
`ModelDeployments`
`ModelArtifacts`
`Workloads`
`Topology`s
`Devices`s
**`Pods`**
**`Topology`**s
EOF

T="$(clone_strict plural-list-continuation)"
cat >>"${T}/docs/getting-started/guide.md" <<'EOF'

- List item opening line.

    Continuation paragraph with malformed `Pods` in list.
EOF
expect_fail "plural-list-continuation" "${T}" "kind plural"

T="$(clone_strict plural-site-content)"
cat >>"${T}/site/content/_index.md" <<'EOF'

Malformed `Nodes` in site content.
EOF
expect_fail "plural-site-content" "${T}" "kind plural"

# Strict heading style: reject layout-container nouns and leading articles on non-Step H2-H3.
T="$(clone_strict heading-container-noun)"
cat >>"${T}/docs/getting-started/guide.md" <<'EOF'

### Host-state rows

Container heading should fail.
EOF
expect_fail "heading-container-noun" "${T}" "layout-container noun 'rows'"

T="$(clone_strict heading-leading-article)"
cat >>"${T}/docs/getting-started/guide.md" <<'EOF'

### The resource keys

Leading article should fail on non-Step heading.
EOF
expect_fail "heading-leading-article" "${T}" "non-Step heading starts with article 'the'"

# Public H2 cap (<= 8 content H2 sections).
T="$(clone_strict public-h2-cap-exceeded)"
cat >"${T}/docs/getting-started/guide.md" <<'EOF'
# Operator Guide

Short intro.

## Contents

- [Section 1](#section-1)
- [Section 2](#section-2)
- [Section 3](#section-3)
- [Section 4](#section-4)
- [Section 5](#section-5)
- [Section 6](#section-6)
- [Section 7](#section-7)
- [Section 8](#section-8)
- [Section 9](#section-9)

## Section 1

Body.

## Section 2

Body.

## Section 3

Body.

## Section 4

Body.

## Section 5

Body.

## Section 6

Body.

## Section 7

Body.

## Section 8

Body.

## Section 9

Body.

---

**See also** — [Operator](../../README.md) (overview)
EOF
expect_fail "public-h2-cap-exceeded" "${T}" "9 '##' sections, over the cap of 8"

# Public prose noise filter: reject blank 'not recorded' placeholders and version-qualified stamps,
# while allowing legitimate prose such as "The condition is recorded on the Pod" or decimals.
T="$(clone_strict prose-recorded-on-valid)"
cat >>"${T}/docs/getting-started/guide.md" <<'EOF'

The condition is recorded on the `Pod` once admitted.
Usage is recorded on the `Pod` in increments of 0.5 cores.
EOF
expect_pass "prose-recorded-on-valid" "${T}"

T="$(clone_strict prose-blank-not-recorded)"
cat >>"${T}/docs/getting-started/guide.md" <<'EOF'

| Property | Value |
| --- | --- |
| Driver | not recorded |
EOF
expect_fail "prose-blank-not-recorded" "${T}" "omit blank 'not recorded' placeholder rows"

T="$(clone_strict prose-recorded-on-guide)"
cat >>"${T}/docs/getting-started/guide.md" <<'EOF'

Behavior recorded on vLLM 0.29.0.
EOF
expect_fail "prose-recorded-on-guide" "${T}" "avoid dev-journal 'recorded on' stamps"

T="$(clone_strict prose-verified-on-guide)"
cat >>"${T}/docs/getting-started/guide.md" <<'EOF'

Verified on v0.23.0 only.
EOF
expect_fail "prose-verified-on-guide" "${T}" "avoid dev-journal 'recorded on' stamps"

T="$(clone_strict prose-verified-on-k8s)"
cat >>"${T}/docs/getting-started/guide.md" <<'EOF'

Verified on Kubernetes 1.32.
EOF
expect_fail "prose-verified-on-k8s" "${T}" "avoid dev-journal 'recorded on' stamps"

T="$(clone_strict prose-recorded-on-date)"
cat >>"${T}/docs/getting-started/guide.md" <<'EOF'

Recorded on 2026-11-21.
EOF
expect_fail "prose-recorded-on-date" "${T}" "avoid dev-journal 'recorded on' stamps"

# Architecture component sync gate: exact token matching (worker vs worker-gateway) and
# distinct Go subcommand vs staged Python package (model-transfer vs gpustack_model_transfer).
T="$(clone_strict arch-component-sync)"
mkdir -p \
  "${T}/engine-sentinel/python" \
  "${T}/model-transfer/python" \
  "${T}/cmd/gpustack-operator" \
  "${T}/pkg/worker" \
  "${T}/pkg/workergateway" \
  "${T}/pkg/modeltransfer"
cat >"${T}/engine-sentinel/python/pyproject.toml" <<'EOF'
[project]
name = "gpustack-engine-sentinel"
EOF
cat >"${T}/model-transfer/python/pyproject.toml" <<'EOF'
[project]
name = "gpustack-model-transfer"
EOF
cat >"${T}/cmd/gpustack-operator/main.go" <<'EOF'
package main
func main() {
  c.AddCommand(worker.NewCommand())
  c.AddCommand(workergateway.NewCommand())
  c.AddCommand(modeltransfer.NewCommand())
}
EOF
cat >"${T}/pkg/worker/cmd.go" <<'EOF'
package worker
var cmd = Command{Use: "worker"}
EOF
cat >"${T}/pkg/workergateway/cmd.go" <<'EOF'
package workergateway
var cmd = Command{Use: "worker-gateway"}
EOF
cat >"${T}/pkg/modeltransfer/cmd.go" <<'EOF'
package modeltransfer
var cmd = Command{Use: "model-transfer"}
EOF
cat >"${T}/docs/getting-started/architecture.md" <<'EOF'
# Architecture

Overview.

## Contents

- [Components](#components)

## Components

Runs `worker`, `worker-gateway`, `model-transfer` (`gpustack_model_transfer`), and `gpustack_engine_sentinel`.

---

**See also** — [Operator Guide](guide.md) (guide)
EOF
awk '
  /^\| \[Operator Guide\]/ {
    print
    print "| [Architecture](getting-started/architecture.md) | Architecture overview |"
    next
  }
  { print }
' "${T}/docs/README.md" >"${T}/docs/README.md.tmp"
mv "${T}/docs/README.md.tmp" "${T}/docs/README.md"
expect_pass "arch-component-sync-baseline" "${T}"

# Removing `worker` must fail even though `worker-gateway` contains "worker" as a substring.
T_SUB="$(clone_strict arch-missing-worker-subcmd)"
cp -R "${T}/." "${T_SUB}/"
cat >"${T_SUB}/docs/getting-started/architecture.md" <<'EOF'
# Architecture

Overview.

## Contents

- [Components](#components)

## Components

Runs `worker-gateway`, `model-transfer` (`gpustack_model_transfer`), and `gpustack_engine_sentinel`.

---

**See also** — [Operator Guide](guide.md) (guide)
EOF
expect_fail "arch-missing-worker-subcmd" "${T_SUB}" "## Components missing runtime subcommand 'worker'"

# Removing `gpustack_model_transfer` must fail even though Go `model-transfer` remains.
T_PY="$(clone_strict arch-missing-python-pkg)"
cp -R "${T}/." "${T_PY}/"
cat >"${T_PY}/docs/getting-started/architecture.md" <<'EOF'
# Architecture

Overview.

## Contents

- [Components](#components)

## Components

Runs `worker`, `worker-gateway`, `model-transfer`, and `gpustack_engine_sentinel`.

---

**See also** — [Operator Guide](guide.md) (guide)
EOF
expect_fail "arch-missing-python-pkg" "${T_PY}" "## Components missing staged Python package 'gpustack_model_transfer'"

# Indented / single-quoted TOML keys and multiline descriptions must still parse [project] name.
T_TOML="$(clone_strict arch-indented-single-quote-toml)"
cp -R "${T_PY}/." "${T_TOML}/"
cat >"${T_TOML}/model-transfer/python/pyproject.toml" <<'EOF'
  [project]
  description = """
name = "worker"
"""
  name = 'gpustack-model-transfer'
EOF
expect_fail "arch-indented-single-quote-toml" "${T_TOML}" "## Components missing staged Python package 'gpustack_model_transfer'"

# Malformed / unterminated TOML must fail closed instead of silently bypassing the check.
T_BAD_TOML="$(clone_strict arch-malformed-toml)"
cp -R "${T}/." "${T_BAD_TOML}/"
cat >"${T_BAD_TOML}/model-transfer/python/pyproject.toml" <<'EOF'
[project]
name = "gpustack-model-transfer
EOF
expect_fail "arch-malformed-toml" "${T_BAD_TOML}" "unable to parse \[project\] name from model-transfer/python/pyproject.toml"

echo "OK: check-docs self-test passed"
