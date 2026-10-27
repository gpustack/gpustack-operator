package priors

import (
	"bytes"
	"encoding/json"
	"fmt"

	"gpustack.ai/gpustack/pkg/utils/stringx"
)

// canonicalJSON encodes a value so that equal content always encodes to equal
// bytes.
//
// IT SORTS THE OBJECT KEYS, because the digest must not depend on the order
// the fields happen to be declared in.
//
// IT ENCODES WITH THE STANDARD LIBRARY, because the shared JSON package
// switches to another encoder under the goccy build tag, and that encoder
// writes a small exponent as 1e-07 where the standard library writes 1e-7. A
// digest is a stored value, so its bytes must not depend on how the binary was
// built.
func canonicalJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		return nil, err
	}
	return json.Marshal(generic)
}

// Digest returns the content digest of a value.
//
// THE DIGEST IS STABLE, so the same content always produces the same digest
// and a changed catalog is visible.
func Digest(value any) (string, error) {
	encoded, err := canonicalJSON(value)
	if err != nil {
		return "", fmt.Errorf("encode digest content: %w", err)
	}
	return "sha256:" + stringx.SumBytesBySHA256(encoded), nil
}

// ProfileDigest returns the digest of a profile's content.
func ProfileDigest(profile Profile) (string, error) {
	profile.Digest = ""
	return Digest(profile)
}

// CatalogDigest returns the digest of a catalog's content.
func CatalogDigest(catalog Catalog) (string, error) {
	catalog.Digest = ""
	return Digest(catalog)
}
