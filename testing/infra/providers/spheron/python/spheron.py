#!/usr/bin/env python3
"""Spheron REST client and JSON CLI. Authentication comes only from the environment."""

import argparse
import json
import math
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from decimal import Decimal


class APIError(Exception):
    """A stable error code, HTTP status, and safe diagnostic for callers."""

    def __init__(self, code, message, status=0):
        super().__init__(message)
        self.code = code
        self.status = status


class NoRedirect(urllib.request.HTTPRedirectHandler):
    """Do not forward authentication to a redirect destination."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


TERMINATED = {"terminated", "terminated-provider"}
CREATE_FIELDS = {
    "provider",
    "offerId",
    "gpuType",
    "gpuCount",
    "region",
    "operatingSystem",
    "instanceType",
    "sshKeyId",
    "ssh_public_key",
    "teamId",
    "name",
    "cloudInit",
    "volumeIds",
    "maxHourlyRate",
}


def validate_create(body):
    """Validate the supported VM/CPU contract before making a request."""
    if not isinstance(body, dict) or body.keys() - CREATE_FIELDS:
        raise APIError(
            "invalid_input", "Expected a VM request with documented fields only"
        )
    for key in (
        "provider",
        "offerId",
        "gpuType",
        "region",
        "operatingSystem",
        "instanceType",
    ):
        if not isinstance(body.get(key), str) or not body[key].strip():
            raise APIError("invalid_input", f"{key} must be a non-empty string")
    if body["instanceType"] not in {"SPOT", "DEDICATED"}:
        raise APIError(
            "invalid_input", "Only SPOT and DEDICATED VM/CPU deployments are supported"
        )
    count = body.get("gpuCount")
    if (
        type(count) is not int
        or count < 0
        or (body["gpuType"] == "CPU") != (count == 0)
    ):
        raise APIError(
            "invalid_input", "gpuCount must be positive for GPU or zero for CPU"
        )
    if sum(bool(body.get(k)) for k in ("sshKeyId", "ssh_public_key")) != 1:
        raise APIError(
            "invalid_input", "Supply exactly one of sshKeyId and ssh_public_key"
        )
    for key in ("sshKeyId", "ssh_public_key", "teamId", "name"):
        if key in body and (not isinstance(body[key], str) or not body[key].strip()):
            raise APIError("invalid_input", f"{key} must be a non-empty string")
    if "cloudInit" in body and not isinstance(body["cloudInit"], dict):
        raise APIError("invalid_input", "cloudInit must be an object")
    if "volumeIds" in body and (
        not isinstance(body["volumeIds"], list)
        or not all(isinstance(v, str) and v for v in body["volumeIds"])
    ):
        raise APIError(
            "invalid_input", "volumeIds must be an array of non-empty strings"
        )
    if "maxHourlyRate" in body:
        rate = body["maxHourlyRate"]
        if (
            type(rate) not in (int, float)
            or not math.isfinite(rate)
            or not 0.01 <= rate <= 1000
        ):
            raise APIError(
                "invalid_input",
                "maxHourlyRate must be between 0.01 and 1000 USD per instance hour",
            )


def selection_requirements(data):
    """Validate selection constraints before reading the catalog."""
    fields = {
        "nodeCount",
        "gpuCount",
        "gpuType",
        "instanceType",
        "providers",
        "regions",
        "operatingSystem",
        "maxTotalHourlyRate",
        "minStorageGB",
        "minRootDiskGB",
    }
    if not isinstance(data, dict) or data.keys() - fields:
        raise APIError("invalid_input", "Unknown machine selection fields")
    criteria = {"instanceType": "SPOT", **data}
    for key, minimum in (("nodeCount", 1), ("gpuCount", 0)):
        if type(criteria.get(key)) is not int or criteria[key] < minimum:
            raise APIError(
                "invalid_input", f"{key} must be an integer in its supported range"
            )
    gpu = criteria.get("gpuType")
    if (
        not isinstance(gpu, str)
        or not gpu.strip()
        or (gpu == "CPU") != (criteria["gpuCount"] == 0)
    ):
        raise APIError(
            "invalid_input", "Supply an exact gpuType and a matching per-node gpuCount"
        )
    if criteria["instanceType"] not in ("SPOT", "DEDICATED"):
        raise APIError("invalid_input", "instanceType must be SPOT or DEDICATED")
    for key in ("providers", "regions"):
        if key in criteria and (
            not isinstance(criteria[key], list)
            or not criteria[key]
            or not all(isinstance(v, str) and v.strip() for v in criteria[key])
        ):
            raise APIError(
                "invalid_input", f"{key} must be a nonempty array of strings"
            )
    if "operatingSystem" in criteria and (
        not isinstance(criteria["operatingSystem"], str)
        or not criteria["operatingSystem"].strip()
    ):
        raise APIError("invalid_input", "operatingSystem must be a nonempty string")
    for key in ("maxTotalHourlyRate", "minStorageGB", "minRootDiskGB"):
        if key in criteria and not finite_nonnegative(criteria[key]):
            raise APIError(
                "invalid_input", f"{key} must be a finite nonnegative number"
            )
    return criteria


def finite_nonnegative(value):
    return type(value) in (int, float) and 0 <= value <= sys.float_info.max


def selection_candidate(offer, criteria):
    """Return a matching offer with explicit price and quantity evidence."""
    if (
        offer.get("available") is not True
        or offer.get("maintenance") is True
        or type(offer.get("gpuCount")) is not int
        or offer["gpuCount"] != criteria["gpuCount"]
        or offer.get("instanceType") != criteria["instanceType"]
        or (
            "providers" in criteria
            and offer.get("provider") not in criteria["providers"]
        )
    ):
        return None
    extras = offer.get("extras", {})
    if not isinstance(extras, dict):
        raise APIError("invalid_response", "Matching offer has invalid extras")
    if extras.get("deployment_type") not in ("vm", "baremetal"):
        return None
    for key in ("provider", "offerId"):
        if not isinstance(offer.get(key), str) or not offer[key].strip():
            raise APIError("invalid_response", f"Matching offer lacks {key}")
    regions = offer.get("clusters")
    if isinstance(regions, str):
        regions = [regions]
    options = offer.get("os_options")
    for values in (regions, options):
        if (
            not isinstance(values, list)
            or not values
            or not all(isinstance(v, str) and v.strip() for v in values)
        ):
            raise APIError(
                "invalid_response", "Matching offer lacks valid regions or OS options"
            )
    regions = sorted(set(regions))
    eligible_regions = [
        r for r in regions if "regions" not in criteria or r in criteria["regions"]
    ]
    if not eligible_regions:
        return None
    if "operatingSystem" in criteria:
        options = [v for v in options if v == criteria["operatingSystem"]]
        if not options:
            return None
    price = offer.get("discountedPrice")
    if price is None:
        price = offer.get("price")
    if not finite_nonnegative(price):
        raise APIError(
            "invalid_response", "Matching offer lacks a finite nonnegative price"
        )
    total = Decimal(str(price)) * criteria["nodeCount"]
    if not math.isfinite(float(total)):
        raise APIError(
            "invalid_input", "Requested fleet cost exceeds the supported numeric range"
        )
    if "maxTotalHourlyRate" in criteria and total > Decimal(
        str(criteria["maxTotalHourlyRate"])
    ):
        return None
    technical = extras.get("technical", {})
    if not isinstance(technical, dict):
        raise APIError(
            "invalid_response", "Matching offer has invalid technical metadata"
        )
    units = technical.get("units_available")
    if units is not None and (type(units) is not int or units < 0):
        raise APIError(
            "invalid_response", "Matching offer has an invalid machine count"
        )
    if units is not None and units < criteria["nodeCount"]:
        return None
    evidence = "reported_quantity"
    if units is None:
        evidence = (
            "available_flag" if criteria["nodeCount"] == 1 else "quantity_unknown"
        )
    elif criteria["nodeCount"] > 1 and len(regions) != 1:
        evidence = "region_quantity_unknown"
    unverified = []
    if evidence in ("quantity_unknown", "region_quantity_unknown"):
        unverified.append(evidence)
    storage = offer.get("storage")
    root_storage = extras.get("root_storage_gb")
    ephemeral_storage = extras.get("ephemeral_storage_gb")
    for size in (storage, root_storage, ephemeral_storage):
        if size is not None and not finite_nonnegative(size):
            raise APIError(
                "invalid_response", "Matching offer has invalid storage metadata"
            )
    for key, size, reason in (
        ("minStorageGB", storage, "storage_unknown"),
        ("minRootDiskGB", root_storage, "root_storage_unknown"),
    ):
        minimum = criteria.get(key, 0)
        if size is None and minimum > 0:
            unverified.append(reason)
        elif size is not None and size < minimum:
            return None
    return {
        "provider": offer["provider"],
        "offerId": offer["offerId"],
        "gpuType": criteria["gpuType"],
        "gpuCount": criteria["gpuCount"],
        "instanceType": criteria["instanceType"],
        "regions": eligible_regions,
        "osOptions": sorted(set(options)),
        "vcpus": offer.get("vcpus"),
        "memory": offer.get("memory"),
        "storage": storage,
        "rootStorageGB": root_storage,
        "ephemeralStorageGB": ephemeral_storage,
        "hourlyRate": price,
        "totalHourlyRate": float(total),
        "unitsAvailable": units,
        "capacityEvidence": evidence,
        "unverifiedReasons": unverified,
    }


class Client:
    """Bounded HTTP calls with no implicit mutation retries or credential output."""

    def __init__(self, api_key=None, *, opener=None, timeout=30):
        self.api_key = (
            api_key if api_key is not None else os.environ.get("SPHERON_API_KEY", "")
        )
        if not self.api_key.strip() or any(c in self.api_key for c in "\r\n"):
            raise APIError(
                "credentials_missing", "Set SPHERON_API_KEY to a valid API key"
            )
        self.opener = opener or urllib.request.build_opener(NoRedirect())
        self.timeout = timeout

    def request(self, method, path, body=None, *, request_key=None):
        headers = {
            "Authorization": "Bearer " + self.api_key,
            "Accept": "application/json",
            "User-Agent": "gpustack-infra-spheron/0.1",
        }
        raw = None
        if body is not None:
            raw = json.dumps(body, allow_nan=False).encode()
            headers["Content-Type"] = "application/json"
        if request_key is not None:
            if not re.fullmatch(r"[A-Za-z0-9_.:-]{8,200}", request_key):
                raise APIError(
                    "invalid_input",
                    "request_key must be 8-200 ASCII letters, digits, or _.:-",
                )
            headers["Idempotency-Key"] = request_key
        request = urllib.request.Request(
            "https://app.spheron.ai" + path, data=raw, headers=headers, method=method
        )
        try:
            with self.opener.open(request, timeout=self.timeout) as response:
                data = response.read(8 * 1024 * 1024 + 1)
                status = getattr(response, "status", 200)
        except urllib.error.HTTPError as exc:
            code = "http_error"
            try:
                error = json.loads(exc.read(65536))
                candidate = error.get("code", "")
                if isinstance(candidate, str) and re.fullmatch(
                    r"[a-z][a-z0-9_]{0,80}", candidate
                ):
                    code = candidate
            except (ValueError, AttributeError):
                pass
            finally:
                exc.close()
            # Server error bodies can echo inputs or credentials. Keep only a bounded code.
            if self.api_key in code:
                code = "http_error"
            raise APIError(
                code,
                f"Spheron returned HTTP {exc.code}; inspect the account or request",
                exc.code,
            ) from None
        except (urllib.error.URLError, TimeoutError, OSError):
            raise APIError(
                "transport_error",
                "Request outcome is unknown; retain IDs and reuse the same create request_key",
            ) from None
        if len(data) > 8 * 1024 * 1024:
            raise APIError("invalid_response", "Response exceeded 8 MiB")
        if method == "DELETE" and status == 204 and not data:
            return {}
        try:
            return json.loads(data)
        except ValueError:
            raise APIError("invalid_response", "Expected JSON from Spheron") from None

    def offers(self, query=None):
        """Collect the complete catalog; fail instead of returning a partial inventory."""
        query = dict(query or {})
        groups = []
        for page in range(1, 101):
            query.update(page=page, limit=100)
            result = self.request(
                "GET", "/api/gpu-offers?" + urllib.parse.urlencode(query)
            )
            if not isinstance(result, dict) or not isinstance(result.get("data"), list):
                raise APIError("invalid_response", "Catalog lacks a data array")
            if any(
                not isinstance(group, dict)
                or not isinstance(group.get("offers"), list)
                or any(not isinstance(offer, dict) for offer in group["offers"])
                for group in result["data"]
            ):
                raise APIError(
                    "invalid_response", "Catalog contains malformed groups or offers"
                )
            pages = result.get("totalPages")
            if type(pages) is int and pages == 0 and page == 1 and result["data"] == []:
                return []
            if type(pages) is not int or not 1 <= pages <= 100:
                raise APIError(
                    "invalid_response",
                    "Catalog pagination is missing or exceeds 100 pages",
                )
            groups.extend(result["data"])
            if page >= pages:
                return groups
        raise APIError("invalid_response", "Catalog pagination did not finish")

    def validate_offer(self, body):
        matches = [
            (group, offer)
            for group in self.offers({"computeType": "vm"})
            for offer in group.get("offers", [])
            if offer.get("provider") == body["provider"]
            and offer.get("offerId") == body["offerId"]
        ]
        if len(matches) != 1:
            raise APIError(
                "offer_unavailable",
                "The selected provider/offer is missing or ambiguous",
            )
        group, offer = matches[0]
        regions = offer.get("clusters", [])
        if isinstance(regions, str):
            regions = [regions]
        if (
            offer.get("available") is not True
            or offer.get("maintenance") is True
            or group.get("gpuType") != body["gpuType"]
            or offer.get("gpuCount") != body["gpuCount"]
            or offer.get("instanceType") != body["instanceType"]
            or body["region"] not in regions
            or body["operatingSystem"] not in offer.get("os_options", [])
        ):
            raise APIError(
                "offer_mismatch",
                "Availability, GPU, region, image, or instance type differs from the offer",
            )
        if "cloudInit" in body and offer.get("supportsCloudInit") is not True:
            raise APIError(
                "unsupported_option", "This offer does not support cloudInit"
            )
        if (
            "maxHourlyRate" in body
            and offer.get("extras", {}).get("live_spot_price") is not True
        ):
            raise APIError(
                "unsupported_option", "This offer does not enforce maxHourlyRate"
            )
        return offer

    def select(self, data):
        """Rank complete catalog results without allocating or inferring capacity."""
        criteria = selection_requirements(data)
        candidates, unverified = [], []
        seen = set()
        for group in self.offers({"computeType": "vm"}):
            if group.get("gpuType") != criteria["gpuType"]:
                continue
            for offer in group["offers"]:
                candidate = selection_candidate(offer, criteria)
                if candidate is None:
                    continue
                identity = (candidate["provider"], candidate["offerId"])
                if identity in seen:
                    raise APIError(
                        "invalid_response",
                        "Matching provider/offer identity is duplicated",
                    )
                seen.add(identity)
                if candidate["unverifiedReasons"]:
                    unverified.append(candidate)
                else:
                    candidates.append(candidate)

        def order(candidate):
            return (
                Decimal(str(candidate["hourlyRate"])),
                candidate["provider"],
                candidate["offerId"],
            )

        candidates.sort(key=order)
        unverified.sort(key=order)
        return {
            "nodeCount": criteria["nodeCount"],
            "currency": "USD",
            "selected": candidates[0] if candidates else None,
            "candidates": candidates,
            "unverifiedCandidates": unverified,
        }

    def create(self, body, request_key):
        validate_create(body)
        if not isinstance(request_key, str):
            raise APIError("invalid_input", "create requires a non-empty request_key")
        # Do not revalidate availability here: recovery with the same key must reach the
        # server even when the original allocation consumed the final available machine.
        return self.request("POST", "/api/deployments", body, request_key=request_key)

    def deployment(self, deployment_id):
        value = self.request("GET", deployment_path(deployment_id))
        if (
            not isinstance(value, dict)
            or value.get("id") != deployment_id
            or not isinstance(value.get("status"), str)
        ):
            raise APIError(
                "invalid_response",
                "Deployment identity or status is missing or mismatched",
            )
        return value

    def delete(self, deployment_id):
        try:
            current = self.deployment(deployment_id)
        except APIError as exc:
            if exc.status == 404:
                return {"id": deployment_id, "status": "terminated"}
            raise
        if current["status"] in TERMINATED:
            return current
        eligibility = self.request(
            "GET", deployment_path(deployment_id) + "/can-terminate"
        )
        if (
            not isinstance(eligibility, dict)
            or eligibility.get("canTerminate") is not True
        ):
            raise APIError(
                "termination_blocked",
                "Cannot terminate yet; retain the deployment and inspect can-terminate",
            )
        return self.request("DELETE", deployment_path(deployment_id))

    def wait(self, deployment_id, target, timeout, interval=5):
        deadline = time.monotonic() + timeout
        while True:
            try:
                current = self.deployment(deployment_id)
            except APIError as exc:
                if target == "terminated" and exc.status == 404:
                    return {"id": deployment_id, "status": "terminated"}
                raise
            status = current["status"]
            if (target == "terminated" and status in TERMINATED) or status == target:
                return current
            if target == "running" and status in TERMINATED | {"failed", "stopped"}:
                raise APIError(
                    "deployment_not_running",
                    "Deployment failed, stopped, or terminated; retain its ID for inspection",
                )
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise APIError(
                    "wait_timeout",
                    "Deadline reached; retain the deployment ID and inspect its current status",
                )
            time.sleep(min(interval, remaining))


def deployment_path(deployment_id):
    if not isinstance(deployment_id, str) or not deployment_id.strip():
        raise APIError("invalid_input", "A non-empty deployment ID is required")
    return "/api/deployments/" + urllib.parse.quote(deployment_id, safe="")


def dispatch(client, operation, data):
    """The same JSON operations serve the CLI and the Terraform subprocess bridge."""
    if operation == "providers":
        return client.request("GET", "/api/providers")
    if operation == "offers":
        return client.offers(data)
    if operation == "select":
        return client.select(data)
    if operation == "deployments":
        return client.request("GET", "/api/deployments?" + urllib.parse.urlencode(data))
    if operation == "ssh-keys":
        return client.request("GET", "/api/ssh-keys")
    if operation == "validate":
        validate_create(data["request"])
        return client.validate_offer(data["request"])
    if operation == "create":
        return client.create(data["request"], data["request_key"])
    if operation == "get":
        return client.deployment(data["id"])
    if operation == "can-terminate":
        return client.request("GET", deployment_path(data["id"]) + "/can-terminate")
    if operation == "delete":
        return client.delete(data["id"])
    if operation == "wait":
        target = data.get("target", "running")
        timeout = data.get("timeout", 1800)
        if (
            target not in {"running", "terminated"}
            or type(timeout) not in (int, float)
            or not math.isfinite(timeout)
            or not 0 <= timeout <= 86400
        ):
            raise APIError(
                "invalid_input",
                "wait requires a running/terminated target and a 0-86400 second timeout",
            )
        return client.wait(data["id"], target, timeout)
    raise APIError("invalid_input", "Unknown operation")


def redact(value, secret):
    """Remove credentials from structured output before JSON escaping."""
    if not secret:
        return value
    if isinstance(value, str):
        return value.replace(secret, "[redacted]")
    if isinstance(value, list):
        return [redact(item, secret) for item in value]
    if isinstance(value, dict):
        return {
            redact(key, secret): redact(item, secret) for key, item in value.items()
        }
    return value


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "operation",
        choices=[
            "providers",
            "offers",
            "select",
            "deployments",
            "ssh-keys",
            "validate",
            "create",
            "get",
            "can-terminate",
            "delete",
            "wait",
        ],
    )
    parser.add_argument(
        "--input", default="-", help="JSON input file, or - for stdin (empty means {})"
    )
    args = parser.parse_args()
    try:
        if args.input == "-":
            raw = sys.stdin.read(1024 * 1024 + 1)
        else:
            with open(args.input, encoding="utf-8") as stream:
                raw = stream.read(1024 * 1024 + 1)
        if len(raw) > 1024 * 1024:
            raise APIError("invalid_input", "Input exceeded 1 MiB")
        data = json.loads(raw) if raw.strip() else {}
        if not isinstance(data, dict):
            raise APIError("invalid_input", "Input must be a JSON object")
        result = dispatch(Client(), args.operation, data)
        secret = os.environ.get("SPHERON_API_KEY", "")
        output = json.dumps(redact(result, secret), allow_nan=False)
        print(output)
    except APIError as exc:
        print(
            json.dumps({"code": exc.code, "status": exc.status, "message": str(exc)}),
            file=sys.stderr,
        )
        return 1
    except (ValueError, KeyError, TypeError, OSError):
        print(
            json.dumps(
                {
                    "code": "invalid_input",
                    "status": 0,
                    "message": "Invalid JSON input or inaccessible input file",
                }
            ),
            file=sys.stderr,
        )
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
