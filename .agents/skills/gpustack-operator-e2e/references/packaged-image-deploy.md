# Packaged image ↔ chart deploy contract

The default e2e flow builds the image **locally** and loads it into the node (`build-load.sh` keeps
`PACKAGE_PUSH=false`; `deploy.sh <NS> <TAG>` pins `image.tag=<TAG>` + `pullPolicy=IfNotPresent`). When a
verification instead runs an image that was **packaged and pushed to a registry** (e.g. a remote amd64
builder ran `make package … PACKAGE_PUSH=true`), you need the image-ref the packager emits and the chart
knobs that point at it. Both are recorded here so a run does not have to re-derive them.

## The operator image ref `make package` emits

`hack/package.sh` (invoked by `make package`) builds one image:

```
${PACKAGE_NAMESPACE}/gpustack-operator:${PACKAGE_TAG}
```

- `PACKAGE_NAMESPACE` — default `gpustack`.
- `PACKAGE_TAG` — default `dev`.
- `PACKAGE_ARCH` — default from `uname` (`x86_64`→`amd64`, `aarch64`→`arm64`).
- `PACKAGE_PUSH` — default `false`; the image is pushed only when `true`.

The image name is always `gpustack-operator` (the sole build task), and the **same image is shared by
the worker Deployment and the per-manufacturer device-manager DaemonSets** — there is no separate
device-manager image.

## The chart image knobs

`deploy/gpustack-operator/chart/values.yaml`:

- `image.repository` — default `gpustack/gpustack-operator`.
- `image.tag` — default `""`, which the chart renders as `v<.Chart.AppVersion>`.
- `image.pullPolicy`.

## Deploying a registry-pushed image

`deploy.sh` already forwards trailing `--set` flags to `helm install` (last `--set` wins), so no new
script is needed — override the repository and force a pull:

```
deploy.sh <NS> <PACKAGE_TAG> \
  --set image.repository=<PACKAGE_NAMESPACE>/gpustack-operator \
  --set image.pullPolicy=Always
```

`deploy.sh` sets `image.tag=<PACKAGE_TAG>` itself; the trailing `image.pullPolicy=Always` overrides its
default `IfNotPresent` so the kubelet pulls the freshly pushed tag instead of a cached one. Example, for
an image pushed as `thxcode/gpustack-operator:dev`:

```
bash .claude/skills/_e2e-lib/scripts/deploy.sh gpustack-system dev \
  --set image.repository=thxcode/gpustack-operator \
  --set image.pullPolicy=Always
```

## Remote amd64 builder: mirror the exact commit before `make package`

When the image is built on a **remote** builder (e.g. an amd64 host reached over ssh), the build must
reproduce the exact commit under test, or the verification is unsound. Two failure modes bit us and are
avoidable:

1. **Stale files → duplicate declarations.** A plain `rsync` (no `--delete`) layered onto an older
   checkout leaves files that a later rename/delete removed (e.g. an old `instancetypeflavor.go`
   alongside the renamed `instance_type_flavor.go`), so the build fails with `redeclared in this block`.
   Mirror with `--delete`, honouring `.gitignore` so build caches are not wiped:

   ```
   rsync -az --delete --filter=':- .gitignore' --exclude='.git' ./ <user>@<host>:<remote-repo>/
   ```

2. **Wrong revision stamp → the staleness gate fails.** `hack/package.sh` stamps the image from
   `git rev-parse HEAD` **run on the builder**; `assert-core.sh` then asserts the running binary's
   embedded 40-hex commit equals the *local* `git rev-parse HEAD`. If the builder dir is not a git
   checkout at the same commit, the stamp is `unknown` (or an old sha) and the gate fails even though
   the code is correct. Sync `.git` too and confirm HEAD matches (a `dirty` version suffix from LFS
   binaries the builder can't smudge is harmless — the gate greps only the 40-hex):

   ```
   rsync -az ./.git/ <user>@<host>:<remote-repo>/.git/
   git -C <remote-repo> rev-parse HEAD   # must equal the local HEAD under test
   ```

   Query remote git with `git -C <dir>` — **never** `ssh '... cd <dir> && ... $(git rev-parse HEAD)'`,
   because the `$(…)` expands in the ssh login's *home* dir before the `cd`, so it reports
   "not a git repository" and misleads the diagnosis.

Then package and deploy:

```
ssh <user>@<host> 'bash -lc "cd <remote-repo> && PACKAGE_ARCH=amd64 PACKAGE_NAMESPACE=<ns> PACKAGE_PUSH=true make package"'
# ... then deploy.sh with the registry overrides above.
```

`bash -lc` is required so Go/Docker are on `PATH` (a non-login shell drops them). After a re-push to the
**same** tag, force the kubelet to re-pull with `kubectl -n <ns> rollout restart deploy/gpustack-operator-worker`
(and the device-manager DaemonSets) — `pullPolicy=Always` alone does nothing without a pod restart.
