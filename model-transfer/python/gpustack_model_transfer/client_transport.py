"""Authenticated, bounded TLS client for a selected broker Pod endpoint."""

import http.client
import io
import ipaddress
import json
import os
import select
import socket
import ssl
import threading
import time
from contextlib import suppress
from urllib.parse import quote

from gpustack_model_transfer.core.errors import SourceUnavailableError
from gpustack_model_transfer.core.manifest import MAX_MANIFEST_RESPONSE_BYTES
from gpustack_model_transfer.core.manifest_pages import MAX_PAGE_BYTES

MAX_RESPONSE = MAX_MANIFEST_RESPONSE_BYTES
MAX_REQUEST = 1 << 20


def _retry_after_seconds(header_value, body_value) -> float | None:
    """Resolve the wait a capacity refusal names: the Retry-After header
    first, the JSON retryAfter figure second. A positive, bounded float is
    the only accepted answer; anything else means the client picks its own
    backoff."""
    for raw in (header_value, body_value):
        if isinstance(raw, bool) or raw is None or raw == "":
            continue
        try:
            seconds = float(raw)
        except (TypeError, ValueError):
            continue
        if 0 < seconds <= 3600:
            return seconds
    return None


class WireUnknown(SourceUnavailableError):
    """A request may have reached the authenticated broker."""


class WireRefused(SourceUnavailableError):
    """The broker returned an explicit refusal."""

    def __init__(self, message, *, retry_after=None, kind=None):
        super().__init__(message)
        self.retry_after = retry_after
        self.kind = kind


class WirePreAdmission(WireRefused):
    """No request bytes were sent, or durable broker admission refused."""

    def __init__(self, message, *, authoritative=False, retry_after=None, kind=None):
        super().__init__(message, retry_after=retry_after, kind=kind)
        self.authoritative = authoritative


def _request_timeout(timeout, deadline):
    if deadline is None:
        return timeout
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise TimeoutError("broker request deadline exceeded")
    return min(timeout, remaining)


class _DeadlineReader(io.RawIOBase):
    def __init__(self, owner, raw):
        self.owner, self.raw = owner, raw

    def readable(self):
        return True

    def readinto(self, buffer):
        self.owner.limit_timeout()
        return self.raw.readinto(buffer)

    def close(self):
        try:
            self.raw.close()
        finally:
            super().close()


class _DeadlineSocket:
    """Recompute the remaining budget before every socket send or receive."""

    def __init__(self, sock, timeout, deadline):
        self.sock, self.timeout, self.deadline = sock, timeout, deadline

    def __getattr__(self, name):
        return getattr(self.sock, name)

    def limit_timeout(self):
        self.sock.settimeout(_request_timeout(self.timeout, self.deadline))

    def sendall(self, data):
        if self.deadline is None:
            self.sock.sendall(data)
            return
        view = memoryview(data)
        sent = 0
        while sent < len(view):
            self.limit_timeout()
            count = self.sock.send(view[sent:])
            if not count:
                raise ConnectionError("broker connection closed during send")
            sent += count

    def makefile(self, mode="r", buffering=None):
        if self.deadline is None or mode != "rb":
            return self.sock.makefile(mode, buffering)
        raw = self.sock.makefile(mode, buffering=0)
        return io.BufferedReader(_DeadlineReader(self, raw))


class _PinnedHTTPS(http.client.HTTPSConnection):
    def __init__(self, host, port, server_name, context, timeout):
        super().__init__(server_name, port, context=context, timeout=timeout)
        self._dial_host = host
        self._server_name = server_name
        self.deadline = None

    def set_deadline(self, deadline):
        self.deadline = deadline
        if self.sock is not None:
            self.sock.deadline = deadline
            self.sock.limit_timeout()

    def connect(self):
        try:
            raw = socket.create_connection(
                (self._dial_host, self.port),
                _request_timeout(self.timeout, self.deadline),
            )
            try:
                raw.settimeout(_request_timeout(self.timeout, self.deadline))
                secured = self._context.wrap_socket(
                    raw, server_hostname=self._server_name
                )
                self.sock = _DeadlineSocket(secured, self.timeout, self.deadline)
            except BaseException:
                raw.close()
                raise
        except (OSError, ssl.SSLError):
            raise WirePreAdmission("broker connection was not established") from None


class BrokerTransport:
    """No proxy, redirects, or token caching; connect to an exact Pod IP."""

    def __init__(self, endpoint, token_file, timeout=5.0):
        address = endpoint.get("address", "")
        try:
            host, raw_port = address.rsplit(":", 1)
            host = host.strip("[]")
            ipaddress.ip_address(host)
            port = int(raw_port)
            if port < 1 or port > 65535:
                raise ValueError
        except (ValueError, AttributeError):
            raise SourceUnavailableError("invalid broker Pod address") from None
        server_name = endpoint.get("serverName")
        ca_file = endpoint.get("caFile")
        if (
            not isinstance(server_name, str)
            or not server_name
            or len(server_name) > 253
        ):
            raise SourceUnavailableError("broker TLS name is required")
        if not isinstance(ca_file, str) or not os.path.isabs(ca_file):
            raise SourceUnavailableError("broker CA path is required")
        self._host, self._port, self._server_name = host, port, server_name
        self._context = ssl.create_default_context(cafile=ca_file)
        self._token_file = token_file
        self._timeout = timeout
        self._tls_local = threading.local()

    def _get_connection(self, deadline=None):
        conn = getattr(self._tls_local, "conn", None)
        if conn is not None and conn.sock is not None:
            try:
                readable, _, _ = select.select([conn.sock], [], [], 0)
                if readable:
                    conn.close()
                    conn = None
            except OSError:
                with suppress(OSError):
                    conn.close()
                conn = None
        if conn is None:
            conn = _PinnedHTTPS(
                self._host,
                self._port,
                self._server_name,
                self._context,
                self._timeout,
            )
            self._tls_local.conn = conn
        conn.set_deadline(deadline)
        return conn

    def _close_connection(self):
        conn = getattr(self._tls_local, "conn", None)
        self._tls_local.conn = None
        if conn is not None:
            with suppress(OSError):
                conn.close()

    def _read_token(self):
        try:
            with open(self._token_file, encoding="utf-8") as handle:
                token = handle.read(32769).strip()
        except (OSError, UnicodeError):
            raise WireRefused("projected token unavailable") from None
        if not token or len(token) > 32768 or "\n" in token or "\r" in token:
            raise WireRefused("invalid projected token")
        return token

    def request(self, method, path, payload=None, *, deadline=None):
        """Share an optional monotonic deadline across every network phase."""
        if not path.startswith("/v1/") or "\r" in path or "\n" in path:
            raise WireRefused("invalid request path")
        token = self._read_token()
        raw = json.dumps(payload or {}, separators=(",", ":")).encode()
        if len(raw) > MAX_REQUEST:
            raise WireRefused("request exceeds bound")
        response = None
        try:
            _request_timeout(self._timeout, deadline)
            connection = self._get_connection(deadline)
            connection.request(
                method,
                path,
                body=raw,
                headers={
                    "Authorization": "Bearer " + token,
                    "Content-Type": "application/json",
                    "Connection": "keep-alive",
                },
            )
            response = connection.getresponse()
            _request_timeout(self._timeout, deadline)
            limit = (
                6 * MAX_PAGE_BYTES + (1 << 20)
                if path == "/v1/manifest-pages"
                else MAX_RESPONSE
            )
            body = response.read(limit + 1)
            _request_timeout(self._timeout, deadline)
            if response.version != 11 or response.will_close:
                self._close_connection()
            if len(body) > limit:
                self._close_connection()
                raise WireUnknown("response exceeds bound")
            if response.status >= 500:
                raise WireUnknown("broker request outcome unknown")
            if response.status < 200 or response.status >= 300:
                try:
                    refusal = json.loads(body or b"{}")
                except (ValueError, UnicodeError):
                    refusal = {}
                if (
                    isinstance(refusal, dict)
                    and refusal.get("admission") == "not-created"
                    and refusal.get("requestID") == (payload or {}).get("requestID")
                    and isinstance(refusal.get("requestID"), str)
                ):
                    retry_after = _retry_after_seconds(
                        response.getheader("Retry-After"), refusal.get("retryAfter")
                    )
                    raise WirePreAdmission(
                        "broker refused before admission",
                        authoritative=True,
                        retry_after=retry_after,
                        kind=refusal.get("kind"),
                    )
                raise WireRefused("broker refused request")
            result = json.loads(body or b"{}")
            if not isinstance(result, dict):
                raise WireUnknown("invalid broker response")
            _request_timeout(self._timeout, deadline)
            return result
        except WireRefused:
            self._close_connection()
            raise
        except (OSError, http.client.HTTPException, ValueError, UnicodeError):
            self._close_connection()
            raise WireUnknown("broker request outcome unknown") from None
        finally:
            if response is not None:
                response.close()


def path_id(value):
    if not isinstance(value, str) or not value or len(value) > 512:
        raise WireRefused("invalid request identity")
    return quote(value, safe="")
