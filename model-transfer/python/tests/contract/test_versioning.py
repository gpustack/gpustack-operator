"""Contract version identity: one version number shared by every party."""

import unittest

from gpustack_model_transfer.core import versioning


class TestVersioning(unittest.TestCase):
    def test_contract_version_is_two(self):
        # Version 2 carries layout only.
        self.assertEqual(versioning.CONTRACT_VERSION, 2)
        self.assertIn(2, versioning.SUPPORTED_CONTRACT_VERSIONS)

    def test_contract_identity_string_pins_name_and_version(self):
        identity = versioning.contract_identity()
        self.assertEqual(identity, "gpustack-model-transfer/reader/2")

    def test_check_accepts_supported_version(self):
        versioning.check_contract_version(2)  # must not raise

    def test_check_rejects_unsupported_version(self):
        with self.assertRaises(versioning.ContractVersionError) as ctx:
            versioning.check_contract_version(1)
        self.assertEqual(ctx.exception.code, "contract-version")

    def test_negotiate_older_v1_client_against_newer_server(self):
        # An older v1 client against a newer server supporting {1, 2} selects v1.
        self.assertEqual(
            versioning.negotiate_contract_version([1], server_versions={1, 2}),
            1,
        )
        # An older v1 client that omits version lists defaults to v1.
        self.assertEqual(
            versioning.negotiate_contract_version(None, server_versions={1, 2}),
            1,
        )
        # When both support v2, the highest mutually supported version wins.
        self.assertEqual(
            versioning.negotiate_contract_version([1, 2], server_versions={1, 2}),
            2,
        )

    def test_negotiate_disjoint_versions_rejected(self):
        with self.assertRaises(versioning.ContractVersionError) as ctx:
            versioning.negotiate_contract_version([99], server_versions={1, 2})
        self.assertEqual(ctx.exception.code, "contract-version")


if __name__ == "__main__":
    unittest.main()
