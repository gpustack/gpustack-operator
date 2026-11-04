"""Offline HTTP and lifecycle contracts; these tests never call Spheron."""

import io
import json
import unittest
import urllib.error
from unittest.mock import patch

from spheron import APIError, Client, NoRedirect, dispatch, main, validate_create


def request_body(**changes):
    return dict(
        provider="spheron-es",
        offerId="offer",
        gpuType="h100-sxm5",
        gpuCount=8,
        region="EU North 1",
        operatingSystem="Ubuntu 22.04",
        instanceType="SPOT",
        ssh_public_key="ssh-ed25519 fixture",
        **changes,
    )


class Opener:
    def __init__(self, responses):
        self.responses = list(responses)
        self.requests = []

    def open(self, request, timeout):
        self.requests.append(request)
        result = self.responses.pop(0)
        if isinstance(result, Exception):
            raise result
        if isinstance(result, tuple):
            status, body = result
            response = io.BytesIO(body)
            response.status = status
            return response
        return io.BytesIO(json.dumps(result).encode())


def http_error(status, code="http_error"):
    return urllib.error.HTTPError(
        "https://app.spheron.ai",
        status,
        "failure",
        {},
        io.BytesIO(json.dumps({"code": code, "error": "do not echo secret"}).encode()),
    )


def offer_fixture(offer_id="offer", price=10, **changes):
    return {
        "provider": "spheron-es",
        "offerId": offer_id,
        "gpuCount": 8,
        "price": price,
        "available": True,
        "instanceType": "SPOT",
        "clusters": ["EU North 1"],
        "os_options": ["Ubuntu 22.04", "Ubuntu 24.04"],
        "storage": 512,
        "extras": {"deployment_type": "vm", "technical": {"units_available": 2}},
        **changes,
    }


def catalog_page(offers, pages=1, gpu="H100_SXM5"):
    return {"data": [{"gpuType": gpu, "offers": offers}], "totalPages": pages}


class SelectionTests(unittest.TestCase):
    def select(self, offers, **requirements):
        opener = Opener([catalog_page(offers)])
        result = dispatch(
            Client("secret", opener=opener),
            "select",
            {"nodeCount": 2, "gpuCount": 8, "gpuType": "H100_SXM5", **requirements},
        )
        self.assertTrue(all(r.get_method() == "GET" for r in opener.requests))
        return result

    def test_cheapest_offer_uses_every_page_and_team_price(self):
        opener = Opener(
            [
                catalog_page([offer_fixture("first", 12)], pages=2),
                catalog_page([offer_fixture("second", 20, discountedPrice=8)], pages=2),
            ]
        )
        result = Client("secret", opener=opener).select(
            {"nodeCount": 2, "gpuCount": 8, "gpuType": "H100_SXM5"}
        )
        self.assertEqual(result["selected"]["offerId"], "second")
        self.assertEqual(result["selected"]["totalHourlyRate"], 16)
        self.assertIn("page=2", opener.requests[1].full_url)
        self.assertEqual([r.get_method() for r in opener.requests], ["GET", "GET"])

    def test_filters_apply_to_rows_even_when_server_ignores_query(self):
        cases = [
            ("unavailable", {"available": False}, {}),
            ("maintenance", {"maintenance": True}, {}),
            ("wrong GPU count", {"gpuCount": 1}, {}),
            ("dedicated fallback", {"instanceType": "DEDICATED"}, {}),
            ("provider", {"provider": "other"}, {"providers": ["spheron-es"]}),
            ("region", {"clusters": ["US"]}, {"regions": ["EU North 1"]}),
            ("OS", {"os_options": ["Debian"]}, {"operatingSystem": "Ubuntu 22.04"}),
            ("container", {"extras": {"deployment_type": "container"}}, {}),
        ]
        for name, changes, requirements in cases:
            with self.subTest(name=name):
                result = self.select(
                    [offer_fixture("cheap", 1, **changes), offer_fixture("match", 10)],
                    **requirements,
                )
                self.assertEqual(result["selected"]["offerId"], "match")

    def test_exact_gpu_type_is_required(self):
        opener = Opener([catalog_page([offer_fixture()], gpu="H100_NVL")])
        result = Client("secret", opener=opener).select(
            {"nodeCount": 2, "gpuCount": 8, "gpuType": "H100_SXM5"}
        )
        self.assertIsNone(result["selected"])

    def test_quantity_evidence(self):
        cases = [
            ("enough", 2, 2, ["EU North 1"], "reported_quantity", True, False),
            ("insufficient", 2, 1, ["EU North 1"], None, False, False),
            ("zero overrides available", 1, 0, ["EU North 1"], None, False, False),
            ("unknown fleet", 2, None, ["EU North 1"], "quantity_unknown", False, True),
            (
                "single available",
                1,
                None,
                ["EU North 1"],
                "available_flag",
                True,
                False,
            ),
            (
                "regional count unknown",
                2,
                4,
                ["EU North 1", "US"],
                "region_quantity_unknown",
                False,
                True,
            ),
        ]
        for name, nodes, units, regions, evidence, selected, unverified in cases:
            with self.subTest(name=name):
                offer = offer_fixture(
                    clusters=regions,
                    extras={
                        "deployment_type": "vm",
                        "technical": {"units_available": units},
                    },
                )
                result = self.select([offer], nodeCount=nodes, regions=["EU North 1"])
                self.assertEqual(result["selected"] is not None, selected)
                self.assertEqual(bool(result["unverifiedCandidates"]), unverified)
                rows = result["candidates"] + result["unverifiedCandidates"]
                if rows:
                    self.assertEqual(rows[0]["capacityEvidence"], evidence)

    def test_unknown_quantity_cannot_undercut_confirmed_stock(self):
        result = self.select(
            [
                offer_fixture("unknown", 1, extras={"deployment_type": "vm"}),
                offer_fixture("enough", 10),
            ]
        )
        self.assertEqual(result["selected"]["offerId"], "enough")
        self.assertEqual(result["unverifiedCandidates"][0]["offerId"], "unknown")

    def test_group_count_does_not_prove_machine_quantity(self):
        page = catalog_page([offer_fixture(extras={"deployment_type": "vm"})])
        page["data"][0]["totalAvailable"] = 1000
        result = Client("secret", opener=Opener([page])).select(
            {"nodeCount": 2, "gpuCount": 8, "gpuType": "H100_SXM5"}
        )
        self.assertIsNone(result["selected"])
        self.assertEqual(len(result["unverifiedCandidates"]), 1)

    def test_budget_is_for_the_whole_fleet(self):
        for budget, selected in [(0.3, True), (0.29, False)]:
            with self.subTest(budget=budget):
                result = self.select(
                    [
                        offer_fixture(
                            price=0.1,
                            extras={
                                "deployment_type": "vm",
                                "technical": {"units_available": 3},
                            },
                        )
                    ],
                    nodeCount=3,
                    maxTotalHourlyRate=budget,
                )
                self.assertEqual(result["selected"] is not None, selected)
                if selected:
                    self.assertEqual(result["selected"]["totalHourlyRate"], 0.3)

    def test_disk_requirements_distinguish_root_and_ephemeral(self):
        cases = [
            ("root too small", 100, 32000, 32100, {"minRootDiskGB": 500}, False, []),
            ("large root", 32100, 0, 32100, {"minRootDiskGB": 500}, True, []),
            (
                "unknown root",
                None,
                None,
                32100,
                {"minRootDiskGB": 500},
                False,
                ["root_storage_unknown"],
            ),
            ("total too small", 100, 100, 200, {"minStorageGB": 500}, False, []),
            ("total enough", 100, 32000, 32100, {"minStorageGB": 500}, True, []),
            (
                "unknown total",
                None,
                None,
                None,
                {"minStorageGB": 500},
                False,
                ["storage_unknown"],
            ),
        ]
        for name, root, ephemeral, storage, requirements, selected, reasons in cases:
            with self.subTest(name=name):
                offer = offer_fixture(
                    storage=storage,
                    extras={
                        "deployment_type": "vm",
                        "technical": {"units_available": 2},
                        "root_storage_gb": root,
                        "ephemeral_storage_gb": ephemeral,
                    },
                )
                result = self.select([offer], **requirements)
                self.assertEqual(result["selected"] is not None, selected)
                self.assertEqual(
                    [
                        reason
                        for row in result["unverifiedCandidates"]
                        for reason in row["unverifiedReasons"]
                    ],
                    reasons,
                )

    def test_ties_are_deterministic(self):
        offers = [
            offer_fixture("z", provider="b"),
            offer_fixture("z", provider="a"),
            offer_fixture("a", provider="a"),
        ]
        for rows in (offers, list(reversed(offers))):
            with self.subTest(rows=rows):
                result = self.select(rows)
                self.assertEqual(
                    [(o["provider"], o["offerId"]) for o in result["candidates"]],
                    [("a", "a"), ("a", "z"), ("b", "z")],
                )

    def test_explicit_dedicated_and_cpu_selection(self):
        for gpu, count, kind in [
            ("H100_SXM5", 8, "DEDICATED"),
            ("CPU", 0, "DEDICATED"),
        ]:
            with self.subTest(gpu=gpu):
                opener = Opener(
                    [
                        catalog_page(
                            [offer_fixture(gpuCount=count, instanceType=kind)], gpu=gpu
                        )
                    ]
                )
                result = Client("secret", opener=opener).select(
                    {
                        "nodeCount": 1,
                        "gpuCount": count,
                        "gpuType": gpu,
                        "instanceType": kind,
                    }
                )
                self.assertEqual(result["selected"]["gpuCount"], count)

    def test_invalid_requirements_fail_before_http(self):
        for change in [
            {"nodeCount": 0},
            {"nodeCount": True},
            {"gpuCount": 1.5},
            {"gpuType": ""},
            {"gpuCount": 0},
            {"providers": []},
            {"providers": "spheron-es"},
            {"regions": [""]},
            {"instanceType": "CLUSTER"},
            {"operatingSystem": None},
            {"maxTotalHourlyRate": -1},
            {"maxTotalHourlyRate": float("nan")},
            {"maxTotalHourlyRate": 10**400},
            {"minStorageGB": True},
            {"minRootDiskGB": -1},
            {"fabric_id": "invented"},
        ]:
            with self.subTest(change=change):
                opener = Opener([])
                with self.assertRaises(APIError) as caught:
                    Client("secret", opener=opener).select(
                        {
                            "nodeCount": 2,
                            "gpuCount": 8,
                            "gpuType": "H100_SXM5",
                            **change,
                        }
                    )
                self.assertEqual(caught.exception.code, "invalid_input")
                self.assertEqual(opener.requests, [])

    def test_bad_catalog_never_yields_partial_recommendation(self):
        bad_offers = [
            offer_fixture(price=None),
            offer_fixture(price=float("nan")),
            offer_fixture(price=-1),
            offer_fixture(clusters=[]),
            offer_fixture(os_options=None),
            offer_fixture(
                extras={"deployment_type": "vm", "technical": {"units_available": True}}
            ),
            offer_fixture(
                extras={"deployment_type": "vm", "technical": {"units_available": -1}}
            ),
            offer_fixture(storage="512"),
        ]
        for offer in bad_offers:
            with self.subTest(offer=offer):
                with self.assertRaises(APIError) as caught:
                    self.select([offer_fixture("good"), offer])
                self.assertEqual(caught.exception.code, "invalid_response")
        with self.assertRaises(APIError):
            self.select([offer_fixture(), offer_fixture()])
        opener = Opener([catalog_page([offer_fixture()], pages=2), http_error(503)])
        with self.assertRaises(APIError) as caught:
            Client("secret", opener=opener).select(
                {"nodeCount": 2, "gpuCount": 8, "gpuType": "H100_SXM5"}
            )
        self.assertEqual(caught.exception.status, 503)


class ClientTests(unittest.TestCase):
    def test_empty_response_requires_delete_no_content(self):
        for method, status, succeeds in [
            ("DELETE", 204, True),
            ("DELETE", 200, False),
            ("GET", 204, False),
        ]:
            with self.subTest(method=method, status=status):
                client = Client("secret", opener=Opener([(status, b"")]))
                if succeeds:
                    self.assertEqual(client.request(method, "/api/deployments/d1"), {})
                else:
                    with self.assertRaises(APIError):
                        client.request(method, "/api/deployments/d1")

    def test_cli_redacts_credentials_before_json_encoding(self):
        for secret in ["test-secret", 'test-"key\\', "test-密钥"]:
            with self.subTest(secret=secret):
                output = io.StringIO()
                result = {"message": "echo " + secret, secret: [secret, 7]}
                with (
                    patch.dict("os.environ", {"SPHERON_API_KEY": secret}),
                    patch("sys.argv", ["spheron", "providers"]),
                    patch("sys.stdin", io.StringIO("{}")),
                    patch("sys.stdout", output),
                    patch("spheron.dispatch", return_value=result),
                ):
                    self.assertEqual(main(), 0)
                self.assertEqual(
                    json.loads(output.getvalue()),
                    {"message": "echo [redacted]", "[redacted]": ["[redacted]", 7]},
                )

    def test_auth_and_json_body(self):
        opener = Opener([{"id": "d1", "status": "deploying"}])
        client = Client("test-secret", opener=opener)
        self.assertEqual(client.create(request_body(), "allocation-001")["id"], "d1")
        request = opener.requests[0]
        self.assertEqual(request.get_header("Authorization"), "Bearer test-secret")
        self.assertEqual(request.get_header("Idempotency-key"), "allocation-001")
        self.assertEqual(request.get_method(), "POST")
        self.assertEqual(json.loads(request.data), request_body())

    def test_transport_failure_does_not_retry_mutation(self):
        opener = Opener([TimeoutError("test-secret")])
        with self.assertRaises(APIError) as caught:
            Client("test-secret", opener=opener).create(
                request_body(), "allocation-001"
            )
        self.assertEqual(caught.exception.code, "transport_error")
        self.assertNotIn("test-secret", str(caught.exception))
        self.assertEqual(len(opener.requests), 1)

    def test_http_errors_remain_distinct(self):
        for status, code in [
            (401, "api_key_invalid"),
            (403, "reservation_access_required"),
            (409, "idempotency_in_progress"),
            (429, "rate_limited"),
            (503, "unavailable"),
        ]:
            with self.subTest(status=status):
                opener = Opener([http_error(status, code)])
                with self.assertRaises(APIError) as caught:
                    Client("secret", opener=opener).deployment("d1")
                self.assertEqual(
                    (caught.exception.status, caught.exception.code), (status, code)
                )
                self.assertNotIn("secret", str(caught.exception))

    def test_redirect_does_not_forward_auth(self):
        self.assertIsNone(
            NoRedirect().redirect_request(
                None, None, 302, "", {}, "https://elsewhere.test"
            )
        )

    def test_catalog_pagination_is_complete(self):
        groups = [{"gpuType": "a", "offers": []}, {"gpuType": "b", "offers": []}]
        opener = Opener(
            [
                {"data": [groups[0]], "totalPages": 2},
                {"data": [groups[1]], "totalPages": 2},
            ]
        )
        self.assertEqual(Client("secret", opener=opener).offers(), groups)
        self.assertIn("page=2", opener.requests[1].full_url)

    def test_catalog_failure_is_not_empty_success(self):
        for response in [
            {"data": [], "totalPages": 101},
            {"data": []},
            {"data": {}, "totalPages": 1},
            {"data": ["malformed"], "totalPages": 1},
            http_error(500),
        ]:
            with self.subTest(response=str(response)), self.assertRaises(APIError):
                Client("secret", opener=Opener([response])).offers()

    def test_invalid_create_is_rejected_without_http(self):
        for change in [
            {"gpuCount": True},
            {"gpuCount": 0},
            {"gpuType": "CPU"},
            {"instanceType": "CLUSTER"},
            {"fabric_id": "invented"},
            {"maxHourlyRate": float("nan")},
            {"sshKeyId": "second-key"},
        ]:
            with self.subTest(change=change):
                opener = Opener([])
                with self.assertRaises(APIError):
                    Client("secret", opener=opener).create(
                        request_body() | change, "allocation-001"
                    )
                self.assertEqual(opener.requests, [])

    def test_create_requires_an_idempotency_key(self):
        for key in [None, "", "line\nbreak", 123]:
            with self.subTest(key=key):
                opener = Opener([])
                with self.assertRaises(APIError):
                    Client("secret", opener=opener).create(request_body(), key)
                self.assertEqual(opener.requests, [])

    def test_cpu_contract(self):
        validate_create(request_body() | {"gpuType": "CPU", "gpuCount": 0})

    def test_offer_preflight(self):
        base = {
            "provider": "spheron-es",
            "offerId": "offer",
            "available": True,
            "gpuCount": 8,
            "instanceType": "SPOT",
            "clusters": ["EU North 1"],
            "os_options": ["Ubuntu 22.04"],
        }
        for change, body, succeeds in [
            ({}, {}, True),
            ({"available": False}, {}, False),
            ({}, {"cloudInit": {}}, False),
            ({}, {"maxHourlyRate": 20}, False),
            ({"clusters": ["elsewhere"]}, {}, False),
        ]:
            with self.subTest(change=change, body=body):
                opener = Opener(
                    [
                        {
                            "data": [
                                {"gpuType": "h100-sxm5", "offers": [base | change]}
                            ],
                            "totalPages": 1,
                        }
                    ]
                )
                client = Client("secret", opener=opener)
                if succeeds:
                    self.assertEqual(
                        client.validate_offer(request_body() | body)["offerId"], "offer"
                    )
                else:
                    with self.assertRaises(APIError):
                        client.validate_offer(request_body() | body)

    def test_delete_requires_eligibility(self):
        for allowed in [True, False]:
            with self.subTest(allowed=allowed):
                opener = Opener(
                    [{"id": "d1", "status": "running"}, {"canTerminate": allowed}, {}]
                )
                client = Client("secret", opener=opener)
                if allowed:
                    client.delete("d1")
                else:
                    with self.assertRaises(APIError) as caught:
                        client.delete("d1")
                    self.assertEqual(caught.exception.code, "termination_blocked")
                self.assertEqual(
                    [r.get_method() for r in opener.requests],
                    ["GET", "GET", "DELETE"] if allowed else ["GET", "GET"],
                )

    def test_absence_is_only_success_for_deletion(self):
        for target, succeeds in [("terminated", True), ("running", False)]:
            with self.subTest(target=target):
                client = Client("secret", opener=Opener([http_error(404)]))
                if succeeds:
                    self.assertEqual(
                        client.wait("d1", target, 0)["status"], "terminated"
                    )
                else:
                    with self.assertRaises(APIError):
                        client.wait("d1", target, 0)

    def test_wait_state_transitions(self):
        for status, target, expected in [
            ("running", "running", None),
            ("stopped", "running", "deployment_not_running"),
            ("failed", "running", "deployment_not_running"),
            ("deploying", "running", "wait_timeout"),
            ("failed", "terminated", "wait_timeout"),
            ("terminated-provider", "terminated", None),
        ]:
            with self.subTest(status=status, target=target):
                client = Client(
                    "secret", opener=Opener([{"id": "d1", "status": status}])
                )
                if expected:
                    with self.assertRaises(APIError) as caught:
                        client.wait("d1", target, 0)
                    self.assertEqual(caught.exception.code, expected)
                else:
                    self.assertEqual(client.wait("d1", target, 0)["status"], status)

    def test_identity_mismatch_is_rejected(self):
        with self.assertRaises(APIError):
            Client(
                "secret", opener=Opener([{"id": "other", "status": "running"}])
            ).deployment("d1")

    def test_missing_key_fails_before_network(self):
        with (
            patch.dict("os.environ", {}, clear=True),
            self.assertRaises(APIError) as caught,
        ):
            Client()
        self.assertEqual(caught.exception.code, "credentials_missing")

    def test_dispatch_rejects_invalid_wait(self):
        for value in [-1, float("inf"), True, "forever"]:
            with self.subTest(value=value), self.assertRaises(APIError):
                dispatch(
                    Client("secret", opener=Opener([])),
                    "wait",
                    {"id": "d1", "timeout": value},
                )


if __name__ == "__main__":
    unittest.main()
