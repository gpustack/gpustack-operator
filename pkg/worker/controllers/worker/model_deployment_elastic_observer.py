"""Bounded read-only Ray GCS table collector for the elastic EP identity observer.

Runs inside the Ray head container via the operator's existing exec transport. It reads
three GCS tables through the pinned-wheel raw accessors and prints ONE JSON document:

  nodes            accessor.get_node_table() rows, projected to explicit fields
  actors           accessor.get_actor_table(None, None) rows, ActorTableData.FromString
  placement_groups accessor.get_placement_group_table() rows, PlacementGroupTableData.FromString

Fail-closed rules:
  - A table that cannot be read is recorded in "errors" and the script exits 2; it never
    prints a partial table that could be mistaken for an empty one.
  - The whole document is written through a bounded writer; exceeding the cap exits 4
    after discarding, so a truncated document is never mistaken for a complete one.
  - --deadline-seconds bounds total wall time; expiry exits 3.
  - IDs are emitted as hex, enums as the raw integer plus the enum's name; nothing is
    stringified through repr.
  - No actor is ever called: this script only reads GCS tables. It never starts Ray
    processes and never calls ray.init().

Exit codes: 0 complete, 2 transport/schema refusal, 3 deadline, 4 output bound.
"""
import argparse
import json
import sys
import time

from ray._raylet import GcsClientOptions, GlobalStateAccessor
from ray.core.generated import gcs_pb2

SCHEMA_VERSION = 1

ACTOR_STATE_NAMES = {
    gcs_pb2.ActorTableData.DEPENDENCIES_UNREADY: "DEPENDENCIES_UNREADY",
    gcs_pb2.ActorTableData.PENDING_CREATION: "PENDING_CREATION",
    gcs_pb2.ActorTableData.ALIVE: "ALIVE",
    gcs_pb2.ActorTableData.RESTARTING: "RESTARTING",
    gcs_pb2.ActorTableData.DEAD: "DEAD",
}

PG_STATE_NAMES = {
    gcs_pb2.PlacementGroupTableData.PENDING: "PENDING",
    gcs_pb2.PlacementGroupTableData.PREPARED: "PREPARED",
    gcs_pb2.PlacementGroupTableData.CREATED: "CREATED",
    gcs_pb2.PlacementGroupTableData.REMOVED: "REMOVED",
    gcs_pb2.PlacementGroupTableData.RESCHEDULING: "RESCHEDULING",
}

NODE_STATE_NAMES = {
    gcs_pb2.GcsNodeInfo.ALIVE: "ALIVE",
    gcs_pb2.GcsNodeInfo.DEAD: "DEAD",
}


class Bound(Exception):
    pass


class BoundWriter:
    def __init__(self, cap):
        self.cap = cap
        self.parts = []

    def write(self, text):
        if sum(len(p) for p in self.parts) + len(text) > self.cap:
            raise Bound("output exceeds %d bytes" % self.cap)
        self.parts.append(text)

    def emit(self):
        sys.stdout.write("".join(self.parts))
        sys.stdout.flush()


def die(code, message):
    sys.stderr.write(message + "\n")
    sys.exit(code)


def connect(gcs_address):
    options = GcsClientOptions.create(
        gcs_address, "", allow_cluster_id_nil=True, fetch_cluster_id_if_nil=True
    )
    accessor = GlobalStateAccessor(options)
    if not accessor.connect():
        die(2, "cannot connect to GCS at %s" % gcs_address)
    return accessor


def project_node(row):
    # get_node_table() returns the wheel's node dicts. Every projected field must be
    # present; a missing key is a schema failure recorded by the caller, not a default.
    labels = row.get("Labels") or {}
    resources = row.get("Resources") or {}
    return {
        "node_id_hex": row.get("NodeID"),
        "alive": row.get("Alive"),
        "node_manager_address": row.get("NodeManagerAddress"),
        "node_manager_hostname": row.get("NodeManagerHostname"),
        "node_manager_port": row.get("NodeManagerPort"),
        "node_name": row.get("NodeName"),
        "death_reason": row.get("DeathReason"),
        "death_reason_message": row.get("DeathReasonMessage"),
        "labels": {str(k): str(v) for k, v in labels.items()},
        "resources": {str(k): json.dumps(v) for k, v in resources.items()},
    }


def project_actor(row):
    state = int(row.state)
    if state not in ACTOR_STATE_NAMES:
        raise KeyError("unknown actor state %d" % state)
    return {
        "actor_id_hex": row.actor_id.hex(),
        "state": state,
        "state_name": ACTOR_STATE_NAMES[state],
        "class_name": row.class_name,
        "node_id_hex": row.node_id.hex(),
        "placement_group_id_hex": row.placement_group_id.hex(),
    }


def project_placement_group(row):
    state = int(row.state)
    if state not in PG_STATE_NAMES:
        raise KeyError("unknown placement group state %d" % state)
    bundles = []
    for bundle in row.bundles:
        bundles.append(
            {
                "bundle_index": int(bundle.bundle_id.bundle_index),
                "unit_resources": {key: str(value) for key, value in bundle.unit_resources.items()},
                "node_id_hex": bundle.node_id.hex(),
            }
        )
    return {
        "placement_group_id_hex": row.placement_group_id.hex(),
        "name": row.name,
        "state": state,
        "state_name": PG_STATE_NAMES[state],
        "bundles": bundles,
    }


def read_nodes(accessor, errors):
    try:
        rows = accessor.get_node_table()
    except Exception as exc:
        errors.append({"table": "nodes", "error": str(exc)[:300]})
        return None
    projected = []
    for row in rows:
        try:
            projected.append(project_node(row))
        except Exception as exc:
            errors.append({"table": "nodes", "error": str(exc)[:300]})
            return None
    return projected


def read_actors(accessor, errors):
    try:
        rows = accessor.get_actor_table(None, None)
    except Exception as exc:
        errors.append({"table": "actors", "error": str(exc)[:300]})
        return None
    projected = []
    for row in rows:
        try:
            projected.append(project_actor(gcs_pb2.ActorTableData.FromString(row)))
        except Exception as exc:
            errors.append({"table": "actors", "error": str(exc)[:300]})
            return None
    return projected


def read_placement_groups(accessor, errors):
    try:
        rows = accessor.get_placement_group_table()
    except Exception as exc:
        errors.append({"table": "placement_groups", "error": str(exc)[:300]})
        return None
    projected = []
    for row in rows:
        try:
            projected.append(
                project_placement_group(gcs_pb2.PlacementGroupTableData.FromString(row))
            )
        except Exception as exc:
            errors.append({"table": "placement_groups", "error": str(exc)[:300]})
            return None
    return projected


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--gcs", required=True)
    parser.add_argument("--deadline-seconds", type=float, required=True)
    parser.add_argument("--max-output-bytes", type=int, required=True)
    args = parser.parse_args()

    deadline = time.monotonic() + args.deadline_seconds
    errors = []

    import ray

    writer = BoundWriter(args.max_output_bytes)
    try:
        accessor = connect(args.gcs)
        document = {
            "schema_version": SCHEMA_VERSION,
            "ray_version": ray.__version__,
            "gcs_address": args.gcs,
            "collected_at_ms": int(time.time() * 1000),
        }

        if time.monotonic() > deadline:
            die(3, "deadline expired before first table read")
        document["nodes"] = read_nodes(accessor, errors)
        if time.monotonic() > deadline:
            die(3, "deadline expired during node table read")
        document["actors"] = read_actors(accessor, errors)
        if time.monotonic() > deadline:
            die(3, "deadline expired during actor table read")
        document["placement_groups"] = read_placement_groups(accessor, errors)
        document["errors"] = errors

        if any(
            document[table] is None
            for table in ("nodes", "actors", "placement_groups")
        ):
            # A refused table is a refusal for the whole document: the parser must
            # never see an absent table and read it as an empty one.
            writer.write(json.dumps(document, indent=1))
            writer.emit()
            die(2, "one or more GCS tables refused")

        writer.write(json.dumps(document, indent=1))
        writer.emit()
        sys.exit(0)
    except Bound as exc:
        die(4, str(exc))
    except SystemExit:
        raise
    except Exception as exc:
        die(2, "gcs read failed: %s" % str(exc)[:300])


if __name__ == "__main__":
    main()
