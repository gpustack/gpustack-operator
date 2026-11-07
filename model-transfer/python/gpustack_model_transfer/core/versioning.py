"""Contract version identity.

Every manifest, session, and wire message names the reader contract version
it was built for. A party that does not support the named version refuses the
exchange instead of guessing at the fields.
"""

from gpustack_model_transfer.core.errors import ContractVersionError

CONTRACT_NAME = "gpustack-model-transfer/reader"
CONTRACT_VERSION = 2
SUPPORTED_CONTRACT_VERSIONS = frozenset({2})


def contract_identity() -> str:
    """Return the name and version of this contract, e.g. a User-Agent stem."""
    return f"{CONTRACT_NAME}/{CONTRACT_VERSION}"


def check_contract_version(version) -> None:
    """Raise ContractVersionError unless this party supports the version.

    Accepts an int; any other type is a protocol fault, not a version choice.
    """
    if isinstance(version, bool) or not isinstance(version, int):
        raise ContractVersionError(
            f"contract version must be an integer, got {version!r}"
        )
    if version not in SUPPORTED_CONTRACT_VERSIONS:
        raise ContractVersionError(
            f"unsupported contract version {version}; supported: "
            f"{sorted(SUPPORTED_CONTRACT_VERSIONS)}"
        )


def negotiate_contract_version(
    client_versions=None,
    server_versions=SUPPORTED_CONTRACT_VERSIONS,
) -> int:
    """Return the highest contract version supported by both client and server.

    An omitted or empty client_versions iterable defaults to legacy (1,),
    so it is refused by a layout-only v2 server.
    """
    if client_versions is None:
        candidates = (1,)
    else:
        if isinstance(client_versions, (str, bytes)) or not hasattr(
            client_versions, "__iter__"
        ):
            raise ContractVersionError(
                f"client_versions must be an iterable of ints, got {client_versions!r}"
            )
        candidates = tuple(client_versions)
        if not candidates:
            candidates = (1,)

    for version in candidates:
        if isinstance(version, bool) or not isinstance(version, int):
            raise ContractVersionError(
                f"contract version must be an integer, got {version!r}"
            )

    supported = set(server_versions)
    common = [v for v in candidates if v in supported]
    if not common:
        raise ContractVersionError(
            f"no mutually supported contract version between client "
            f"{sorted(set(candidates))} and server {sorted(supported)}"
        )
    return max(common)
