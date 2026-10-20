#!/usr/bin/env bash
# Exercise the fatal H2-H6 heading checks with a small Markdown tree.
set -euo pipefail

ROOT_DIR="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)}"
CHECKER="${ROOT_DIR}/.agents/skills/gpustack-operator-docs/scripts/check-docs.sh"
FIXTURE="$(mktemp -d)"
trap 'rm -rf "${FIXTURE}"' EXIT

mkdir -p "${FIXTURE}/docs" "${FIXTURE}/site/content" "${FIXTURE}/.agents/skills"
cat >"${FIXTURE}/README.md" <<'EOF'
# Fixture
EOF
cat >"${FIXTURE}/docs/README.md" <<'EOF'
# Fixture index

## All pages

| Page | Link |
| --- | --- |
| [Fixture page](x.md) | page |
EOF
cat >"${FIXTURE}/docs/x.md" <<'EOF'
# Fixture page

Short introduction.

## Contents

- [Step 1: Create Pod](#step-1-create-pod)
- [Example](#example)

## Step 1: Create Pod

Run the command.

## Example

```markdown
## Step 99: this heading is intentionally long and lowercase
```

---

**See also** — [Fixture index](README.md)
EOF

bash "${CHECKER}" "${FIXTURE}" >/dev/null

sed -i.bak \
  -e 's/^- \[Step 1: Create Pod\](#step-1-create-pod)$/- [Step 1: this heading is intentionally long and lowercase](#step-1-this-heading-is-intentionally-long-and-lowercase)/' \
  -e 's/^## Step 1: Create Pod$/## Step 1: this heading is intentionally long and lowercase/' \
  "${FIXTURE}/docs/x.md"
rm -f "${FIXTURE}/docs/x.md.bak"
if bash "${CHECKER}" "${FIXTURE}" >"${FIXTURE}/bad.out" 2>&1; then
  echo "check-docs self-test: invalid numbered heading unexpectedly passed" >&2
  exit 1
fi
grep -q "capitalize the first word after the numbered prefix" "${FIXTURE}/bad.out"
grep -q "heading topic is" "${FIXTURE}/bad.out"

echo "OK: check-docs heading self-test passed"
