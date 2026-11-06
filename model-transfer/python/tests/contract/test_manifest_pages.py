import hashlib
import json

import pytest

from gpustack_model_transfer.core.errors import InvalidDescriptorError
from gpustack_model_transfer.core.manifest import load_manifest
from gpustack_model_transfer.core.versioning import CONTRACT_VERSION


def write_pages(tmp_path, *, duplicate=False, overlap=False):
    directory = tmp_path / "pages"
    directory.mkdir()
    pages = []
    for number in range(2):
        begin = 8 + number * (2 if overlap else 4)
        tensors = [
            {
                "name": "first" if number == 0 or duplicate else "last",
                "file": "weights.safetensors",
                "dtype": "U8",
                "shape": [4],
                "data_begin": begin,
                "data_end": begin + 4,
            }
        ]
        raw = json.dumps(tensors).encode()
        digest = hashlib.sha256(raw).hexdigest()
        (directory / (digest + ".json")).write_bytes(raw)
        pages.append(
            {
                "sha256": digest,
                "size": len(raw),
                "count": 1,
                "file": "weights.safetensors",
                "begin": begin,
                "end": begin + 4,
            }
        )
    root = {
        "contract_version": CONTRACT_VERSION,
        "artifact_uid": "artifact",
        "source_generation": 1,
        "files": [{"path": "weights.safetensors", "size": 32}],
        "tensors": [],
        "tensor_pages": pages,
    }
    path = tmp_path / "manifest.json"
    path.write_text(json.dumps(root))
    return path


def test_paged_manifest_lookup(tmp_path):
    manifest = load_manifest(write_pages(tmp_path))
    assert len(manifest.tensors) == 2
    assert manifest.tensor("last").data_begin == 12
    assert [tensor.name for tensor in manifest.tensors] == ["first", "last"]
    assert not isinstance(manifest.tensors, (list, tuple))


@pytest.mark.parametrize("duplicate,overlap", [(True, False), (False, True)])
def test_paged_manifest_cross_page_corruption(tmp_path, duplicate, overlap):
    with pytest.raises(InvalidDescriptorError):
        load_manifest(write_pages(tmp_path, duplicate=duplicate, overlap=overlap))
