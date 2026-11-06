"""The control client for the node's source broker.

Every message names one generation, carries the worker's secret, and
names the workerInstanceID of the supervisor boot that sent it — a
fresh random instance per supervisor boot, so the broker can tell an
uncertain old native attempt from this one. The secret is sent to the
broker and nowhere else: never logged, never in a repr, never in an
error.

The wire this client speaks is the contract the Go broker mirrors; the
Python side is the reference. register (/v1/register) precedes
native-start (/v1/native-start), and native-start precedes ANY vendor
context, allocation, or registration call — its acknowledgement is what
authorizes native work, and it is retried idempotently until answered.
"""

import http.client
import json
import uuid

from gpustack_model_transfer.core.manifest import MAX_MANIFEST_RESPONSE_BYTES
from gpustack_model_transfer.core.manifest_pages import MAX_PAGE_BYTES

MAX_RESPONSE_BYTES = MAX_MANIFEST_RESPONSE_BYTES
MAX_ASSIGNMENTS = 16


def _read_response(response, max_bytes=MAX_RESPONSE_BYTES):
    raw = response.read(max_bytes + 1)
    if len(raw) > max_bytes:
        raise ControlError(0, "invalid-response", "response exceeds limit")
    try:
        result = json.loads(raw or b"{}")
    except (ValueError, UnicodeError):
        raise ControlError(0, "invalid-response", "invalid JSON response") from None
    if not isinstance(result, dict):
        raise ControlError(0, "invalid-response", "invalid response object")
    return result


class ControlError(Exception):
    """A typed refusal from the broker.

    kind mirrors the broker's protocol kind (authentication,
    stale-generation, capacity-exhausted, ...) so a worker can branch on the
    verdict instead of parsing prose. status 0 with kind "unavailable"
    means the wire itself failed before an answer — an idempotent retry
    case, not a refusal.
    """

    def __init__(self, status: int, kind: str, reason: str):
        super().__init__(f"{kind}: {reason}")
        self.status = status
        self.kind = kind
        self.reason = reason

    @property
    def transport(self) -> bool:
        return self.kind == "unavailable"


class BootstrapClient:
    """The Pod-local work-order poll, before any generation exists.

    The bearer is the bootstrap credential the broker minted into the
    emptyDir file both containers share; it authorizes this worker
    container on the loopback wire only, and it is not a generation
    secret. The answer carries the admitted assignments: generation,
    secret, digest, namespace, artifactUID, sourceGeneration,
    manifestSHA256, manifestJSON, treePath, totalBytes, registrations.
    The raw manifest travels over the Pod loopback only: the worker
    never mounts or reads the broker's ledger.
    """

    def __init__(self, address: str, bootstrap_file: str, timeout: float = 5.0):
        self._address = address
        self._bootstrap_file = bootstrap_file
        self._timeout = timeout

    def _token(self):
        try:
            with open(self._bootstrap_file, encoding="utf-8") as handle:
                token = handle.read(513).strip()
        except (OSError, UnicodeError):
            raise ControlError(0, "unavailable", "bootstrap unavailable") from None
        if not token or len(token) > 512:
            raise ControlError(0, "invalid-response", "invalid bootstrap")
        return token

    def assignments(self) -> list:
        token = self._token()
        connection = http.client.HTTPConnection(self._address, timeout=self._timeout)
        try:
            connection.request(
                "GET",
                "/v1/assignments",
                headers={"Authorization": token},
            )
            answer = connection.getresponse()
            body = _read_response(answer)
        except (OSError, http.client.HTTPException) as exc:
            raise ControlError(
                0, "unavailable", f"assignment poll failed: {exc}"
            ) from exc
        finally:
            connection.close()
        if answer.status != 200:
            raise ControlError(
                answer.status,
                str(body.get("kind", "error")),
                str(body.get("reason", "the assignment poll was refused")),
            )
        assignments = body.get("assignments")
        if not isinstance(assignments, list) or len(assignments) > MAX_ASSIGNMENTS:
            raise ControlError(0, "invalid-response", "invalid assignment list")
        return assignments

    def manifest_page(self, assignment, page):
        """Read an identity-bound page over the authenticated loopback channel."""
        payload = {
            "generation": assignment["generation"],
            "manifestSHA256": assignment["manifestSHA256"],
            "pageSHA256": page["sha256"],
        }
        connection = http.client.HTTPConnection(self._address, timeout=self._timeout)
        try:
            connection.request(
                "POST",
                "/v1/manifest-pages",
                body=json.dumps(payload),
                headers={
                    "Authorization": self._token(),
                    "Content-Type": "application/json",
                },
            )
            response = connection.getresponse()
            answer = _read_response(response, 6 * MAX_PAGE_BYTES + (1 << 20))
            if response.status != 200:
                raise ControlError(
                    response.status, "unavailable", "manifest page refused"
                )
            if (
                answer.get("manifestSHA256") != payload["manifestSHA256"]
                or answer.get("pageSHA256") != page["sha256"]
                or not isinstance(answer.get("pageJSON"), str)
            ):
                raise ControlError(
                    0, "invalid-response", "manifest page identity mismatch"
                )
            return answer["pageJSON"].encode("utf-8")
        except (OSError, http.client.HTTPException, UnicodeError):
            raise ControlError(0, "unavailable", "manifest page read failed") from None
        finally:
            connection.close()


class ControlClient:
    """One worker's connection to the local broker control service.

    worker_id is the Pod UID; worker_instance_id identifies the supervisor
    boot and is carried by every control message.
    """

    def __init__(
        self,
        address: str,
        worker_id: str,
        generation: str,
        secret: str,
        worker_instance_id: str | None = None,
        timeout: float = 5.0,
    ):
        self._address = address
        self.worker_id = worker_id
        self.generation = generation
        self._secret = secret
        self.worker_instance_id = worker_instance_id or uuid.uuid4().hex
        self._timeout = timeout
        # The lease the broker issued at registration, carried by every
        # message after it.
        self.lease_id = None
        self.native_started = False

    def register(self) -> str:
        """Register the worker and return its lease ID."""
        answer = self._post("/v1/register")
        lease = answer.get("leaseID")
        if not isinstance(lease, str) or not lease or len(lease) > 512:
            raise ControlError(0, "invalid-response", "invalid lease")
        self.lease_id = lease
        return lease

    def native_start(self) -> dict:
        """Announce a native worker boot for this generation.

        Sent before any vendor context, allocation, or registration
        call, with the same worker/generation/secret/lease/instance
        tuple as every other message; idempotent, so a lost response is
        retried until acknowledged. Native work does not start before
        the answer arrives.
        """
        answer = self._post("/v1/native-start")
        self.native_started = True
        return answer

    def heartbeat(
        self,
        resident_bytes: int = 0,
        registrations: int = 0,
        active_sessions: int = 0,
        unknown_sessions: int = 0,
        native_operations: int = 0,
    ) -> dict:
        """Renew the lease and return the broker's answer.

        The local counts describe this worker's own view; the answer's
        activeSessions and unknownSessions are the broker's
        authoritative counts, and only those drive a drain decision.
        """
        return self._post(
            "/v1/heartbeat",
            residentBytes=resident_bytes,
            registrations=registrations,
            activeSessions=active_sessions,
            unknownSessions=unknown_sessions,
            nativeOperations=native_operations,
        )

    def filled(self, registrations: int, descriptors=None) -> None:
        """Report the generation's content verified and registered.

        descriptors lists one entry per registered file: name (the
        manifest-relative file path), addr, length (the complete file's
        extent), memoryType, backend, workerUID. registrations is the
        exact nonempty-file count the order admitted.
        """
        self._post(
            "/v1/filled",
            registrations=registrations,
            descriptors=list(descriptors or []),
        )

    def drain_ack(self, native_released: bool) -> None:
        """Confirm the required disconnect completed.

        native_released is true only after successful native cleanup
        with zero authoritative sessions, unknown sessions, and native
        operations; a worker holding unknown state never sends it.
        """
        self._post("/v1/drain-ack", nativeReleased=bool(native_released))

    def quarantine(self, reason: str) -> None:
        """Report an operation whose outcome could not be proven."""
        self._post("/v1/quarantine", reason=reason)

    def close(self) -> None:
        """Release the client; connections are per request, so a no-op."""

    def _post(self, path: str, **payload) -> dict:
        payload.update(
            workerID=self.worker_id,
            generation=self.generation,
            secret=self._secret,
            leaseID=self.lease_id or "",
            workerInstanceID=self.worker_instance_id,
        )
        body = json.dumps(payload)
        connection = http.client.HTTPConnection(self._address, timeout=self._timeout)
        try:
            connection.request(
                "POST", path, body=body, headers={"Content-Type": "application/json"}
            )
            response = connection.getresponse()
            answer = _read_response(response)
            status = response.status
        except (OSError, http.client.HTTPException) as exc:
            raise ControlError(
                0, "unavailable", f"the control wire failed: {exc!s}"
            ) from None
        finally:
            connection.close()
        if status >= 400:
            raise ControlError(status, str(answer.get("kind", "internal")), "refused")
        return answer

    def __repr__(self):
        return (
            f"ControlClient(worker_id={self.worker_id!r}, "
            f"generation={self.generation!r}, "
            f"worker_instance_id={self.worker_instance_id!r}, "
            f"secret=<redacted>)"
        )


__all__ = ["BootstrapClient", "ControlClient", "ControlError"]
