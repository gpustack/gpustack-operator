package priors

import (
	"fmt"

	_ "embed"
)

// DefaultCatalogConfigMapKey is the ConfigMap data key a catalog reference
// reads when the key is omitted.
//
// IT IS THE YAML FILE, because the catalog is written in YAML and a reader that
// names one format reads one file.
const DefaultCatalogConfigMapKey = "priors.yaml"

// LegacyCatalogConfigMapKey is the ConfigMap data key a catalog was read from
// before the catalog changed its file name.
//
// IT IS READ AS A FALLBACK, because a ConfigMap written before the change still
// holds the catalog the operator shipped.
const LegacyCatalogConfigMapKey = "priors.json"

// embeddedCatalog is the catalog shipped with the operator.
//
// IT MAY HOLD NO PROFILES, because a profile only enters the catalog when a
// source measured the evidence the resolver needs. An empty catalog refuses
// every resolution, which is the correct behavior until a measured profile
// is available.
//
//go:embed priors.yaml
var embeddedCatalog []byte

// LoadEmbedded returns the catalog shipped with the operator.
//
// IT IS PARSED AND CHECKED, so a catalog that was generated wrong fails here
// instead of at the first resolution.
func LoadEmbedded() (*Catalog, error) {
	catalog, err := ParseCatalog(embeddedCatalog)
	if err != nil {
		return nil, fmt.Errorf("parse embedded catalog: %w", err)
	}
	return catalog, nil
}

// EmbeddedBytes returns the raw bytes of the catalog shipped with the
// operator.
//
// IT EXISTS SO A CALLER CAN COMPARE THE EMBEDDED CATALOG WITH A CATALOG READ
// FROM ELSEWHERE.
func EmbeddedBytes() []byte {
	return embeddedCatalog
}
