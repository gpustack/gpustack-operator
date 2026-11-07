# Local Spheron Terraform provider

This development provider calls an embedded Python client for the current
`https://app.spheron.ai` REST API. Go handles the Terraform protocol and resource state;
Python owns authentication, HTTP requests, error codes and deployment polling.
It is independent of the archived Spheron Protocol provider.

## Build and use

Prerequisites: Go 1.25.8 or newer, Python 3.10 or newer, Terraform 1.9 or newer,
and `SPHERON_API_KEY` exported in the shell that invokes Terraform or Python.
There is no API-key Terraform variable. The key is inherited by the Python subprocess
and is not placed in provider configuration, subprocess arguments or resource state.

From the repository root:

```bash
python3 testing/infra/providers/spheron/dev.py
export TF_CLI_CONFIG_FILE="$PWD/testing/infra/providers/spheron/.terraform/dev.tfrc"
terraform -chdir=testing/infra/computes/spheron init
terraform -chdir=testing/infra/computes/spheron validate
```

The build writes a local mirror and a `dev_overrides` configuration under this provider's
ignored `.terraform` directory. It does not modify `~/.terraformrc`. Rebuild after changes;
plan/apply then use the new binary. The embedded Python source is rebuilt into that binary.
The mirror can resolve the unpublished `gpustack.local/infra/spheron` provider during
initialization. Terraform 1.17 skips installing providers with a development override;
the mirror also supports clients that still resolve them during init. After a rebuild, use the development override
without re-running init. If init is needed again and rejects the changed local checksum,
remove only this disposable development root's `.terraform.lock.hcl` and reinitialize.
Do not distribute that lock file as a release lock.

This is a development installation. For reproducible CI or release use, build versioned
packages with checksums and a filesystem/private registry installation instead of keeping
development overrides enabled. See HashiCorp's
[development override contract](https://developer.hashicorp.com/terraform/cli/config/config-file#development-overrides-for-provider-developers).

## Python interface

The client uses the Python standard library. Each operation accepts one JSON object on
stdin (or `--input file.json`), returns JSON on stdout, and returns a structured
`code`, `status`, `message` error on stderr with exit code 1. API error bodies are not echoed.
HTTP redirects are refused and mutations are never retried automatically.

```bash
python3 testing/infra/providers/spheron/python/spheron.py providers </dev/null
printf '%s\n' '{"search":"h100","computeType":"gpu"}' |
  python3 testing/infra/providers/spheron/python/spheron.py offers
printf '%s\n' '{"id":"deployment-id"}' |
  python3 testing/infra/providers/spheron/python/spheron.py get
```

| Operation | Input | Result |
|---|---|---|
| `providers` | `{}` | Provider list |
| `offers` | Catalog filters | All catalog pages, as a group array; incomplete pagination fails |
| `select` | Node/GPU requirements below | Cheapest qualifying offer, ranked alternatives, and candidates with unverified quantity or disk capacity; read-only |
| `deployments` | Documented list filters | API list response |
| `ssh-keys` | `{}` | Existing SSH keys |
| `validate` | `{"request": {...}}` | Exact live offer after checking selection and optional capabilities; read-only |
| `create` | `{"request": {...}, "request_key": "unique-allocation-key"}` | Immediate deployment response, including ID |
| `get` | `{"id": "..."}` | Identity-checked deployment |
| `can-terminate` | `{"id": "..."}` | Current termination eligibility |
| `delete` | `{"id": "..."}` | Termination request after eligibility check; may still require polling |
| `wait` | `{"id": "...", "target": "running", "timeout": 1800}` | Observed target state; target can also be `terminated` |

Create supports GPU/CPU VMs and dedicated bare-metal offers, selected by exact catalog
fields. Containers and reserved-cluster bidding are outside this version's contract.
Run `validate` before a new allocation. Create deliberately does not repeat the catalog
preflight: a recovery request with the same idempotency key must reach the server even if
the original allocation consumed the last available machine.

## Select machines

The local `select` operation reads every page of Spheron's
[`GET /api/gpu-offers`](https://docs.spheron.ai/api-reference) and filters the returned
rows itself. It does not create or reserve machines and is not a separate Spheron HTTP
endpoint. Use the [machine selection workflow](../../computes/spheron/README.md#machine-selection)
before applying the result.

From the repository root:

```bash
printf '%s\n' '{"nodeCount":2,"gpuCount":8,"gpuType":"H100_SXM5"}' |
  python3 testing/infra/providers/spheron/python/spheron.py select
```

| Input | Contract |
|---|---|
| `nodeCount` | Required positive integer; number of identical nodes requested |
| `gpuCount` | Required GPUs per node, not the fleet total; positive integer, or zero with `gpuType = "CPU"` |
| `gpuType` | Required exact catalog group identifier; for example `H100_SXM5` and `H100_NVL` are distinct |
| `instanceType` | `SPOT` by default; `DEDICATED` must be requested explicitly; no automatic fallback |
| `providers` | Optional nonempty array of allowed provider IDs; omitted means all providers |
| `regions` | Optional nonempty array of allowed catalog region strings; omitted means all regions |
| `operatingSystem` | Optional exact OS option; omitted leaves the supported choices in the result |
| `maxTotalHourlyRate` | Optional nonnegative USD/hour limit for all requested nodes together; a selection filter, not a running price cap |
| `minStorageGB` | Optional nonnegative minimum advertised total storage per node in GB; includes ephemeral storage where the offer does |
| `minRootDiskGB` | Optional nonnegative minimum root disk per node in GB; requires explicit `extras.root_storage_gb` evidence |

The result contains `selected` (an offer or `null`), `candidates` sorted by fleet hourly
cost, and separately sorted `unverifiedCandidates`. Each candidate reports `provider`,
`offerId`, `gpuType`, `gpuCount`, `instanceType`, eligible `regions`, `osOptions`, CPU/RAM/disk
shape, `rootStorageGB`, `ephemeralStorageGB`, `hourlyRate`, `totalHourlyRate`, `unitsAvailable`,
`capacityEvidence`, and `unverifiedReasons`. Rates are
USD per whole node-hour and fleet-hour respectively, using `discountedPrice` when supplied,
otherwise `price`. Fleet cost is `nodeCount * hourlyRate`; ties use provider then offer ID.
These are current catalog rates, not guaranteed lifetime costs or minimum-runtime charges.

Only available VM or bare-metal offers outside maintenance qualify. The selector chooses
one offer for all nodes; it does not combine suppliers, regions or storage variants whose
capacity may overlap. For multiple nodes, recommendation requires an integer
`extras.technical.units_available >= nodeCount` on an offer with one advertised region.
Missing counts or counts spanning multiple regions remain unverified. For one node,
`available = true` suffices when no count is supplied. An explicit zero never qualifies.
Missing storage metadata for a positive disk requirement also places an offer in
`unverifiedCandidates`; total storage is never substituted for root capacity. A known
undersized disk excludes the offer. Additional volumes are not selected, priced or created
by this operation. The group-level `totalAvailable` is not used as a machine count. Catalog evidence never
reserves capacity or proves shared RDMA fabric.

No match is a successful query with `selected = null`; missing quantity or disk evidence can leave cheaper
offers in `unverifiedCandidates`. Do not treat that outcome as authorization to buy a more
expensive or non-Spot alternative. Invalid inputs return `invalid_input`; malformed matching
catalog rows return `invalid_response`; authentication, transport and pagination errors
propagate instead of returning a partial recommendation. Repeating selection is read-only
and can return different offers as inventory changes.

## State and recovery

`spheron_instance` takes immutable `request_json` and `request_key`. Changes require replacement.
JSON formatting alone does not cause replacement. The API key is never stored, but the
creation request is stored in Terraform state and marked sensitive. Keep secrets out of
`cloudInit` and protect the state as usual.

Use one distinct `request_key` per allocation and keep it in configuration. After a create
timeout with no returned ID, keep the same request and key to reconcile the outcome. Do not
generate a new key just because a response was lost. Once an allocation is terminated,
preempted, or replaced, select a new key for its replacement; the API rejects spent keys.
Do not change the request body under an existing key. There is no automatic capacity retry.

The provider records the returned deployment ID before waiting for `running`. A failed wait
leaves the ID in state for inspection or cleanup. Terraform may mark such a resource tainted;
inspect the deployment and untaint it if it subsequently became healthy before planning again.
An explicit GET 404 or a terminal `terminated`/`terminated-provider` status removes the resource
from state during refresh. Authentication failures, server failures, `stopped` and `failed`
deployments retain state. A refresh does not restart stopped instances.

Delete checks `can-terminate`; blocked termination fails immediately with the ID retained.
Read the API's minimum runtime and reason before retrying. A successful DELETE is followed by
polling; state is cleared only after a terminal status or GET 404. The API's terminal state is
the evidence boundary; the provider has no separate access to the underlying supplier's API.

Import accepts a deployment ID:

```bash
terraform import spheron_instance.this deployment-id
```

Import reconstructs the fields returned by GET and assigns `imported-<id>` as the local
request key. The API does not return the complete original request (for example startup
scripts). Both creation fields are `ForceNew`: differences from the reconstructed
`request_json` or `imported-<id>` key will plan destruction and replacement of the imported
VM. Use `terraform state show` to inspect these values, align configuration, and require a
plan without replacement before applying an imported resource;
an unknown original setting must not be treated as verified or silently replaced.

## Validation

From the repository root:

```bash
python3 -m unittest discover -s testing/infra/providers/spheron/python -v
(cd testing/infra/providers/spheron && env -u GODEBUG go test -race ./... && env -u GODEBUG go vet ./...)
terraform -chdir=testing/infra/computes/spheron test
terraform -chdir=testing/infra/clusters/spheron test
make lint
make lint docs
```

The Python tests inject HTTP responses. Go tests inject API calls and include a real
Terraform CLI lifecycle test covering create, an unchanged plan, import and destroy.
The module tests use Terraform mock providers. These checks exercise state/error contracts
without provisioning. They do not establish
live create/delete, SSH, Kubernetes or fabric acceptance.

See [standalone compute](../../computes/spheron/README.md) and
[cluster composition](../../clusters/spheron/README.md).
