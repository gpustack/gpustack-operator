// Package protocol holds the wire types and validation of the model-transfer reader
// contract. The types are transport-independent: no Kubernetes clients, no
// vendor code, and no engine tensor layouts appear here. The Python reader
// gpustack_model_transfer/gpustack_model_transfer speaks the same shapes, and both sides verify the
// shared golden fixtures under model-transfer/contracts.
package protocol

import "fmt"

// ContractVersion is the default version of the reader contract this package speaks.
const ContractVersion = 2

// SupportedContractVersions lists every reader contract version this server/client
// build supports. Version 2 carries layout only; version 1 is not supported.
var SupportedContractVersions = []int{2}

// ContractName prefixes the contract identity string.
const ContractName = "gpustack-model-transfer/reader"

// ContractIdentity returns the name and version, for example as a protocol
// header value shared by both language sides.
func ContractIdentity() string {
	return fmt.Sprintf("%s/%d", ContractName, ContractVersion)
}

// CheckContractVersion reports whether this package supports the version a
// manifest or session negotiation names. An unsupported version is refused,
// never interpreted as the latest known one.
func CheckContractVersion(version int) error {
	for _, supported := range SupportedContractVersions {
		if version == supported {
			return nil
		}
	}
	return New(KindContractVersion, "contract",
		"unsupported contract version %d; supported: %v", version,
		SupportedContractVersions)
}

// NegotiateContractVersion selects the highest contract version supported by both
// the client and the server. An empty clientVersions slice from a v1 client that
// predates explicit version lists still means version 1 and is refused.
func NegotiateContractVersion(clientVersions []int) (int, error) {
	if len(clientVersions) == 0 {
		clientVersions = []int{1}
	}
	best := 0
	for _, cv := range clientVersions {
		for _, sv := range SupportedContractVersions {
			if cv == sv && cv > best {
				best = cv
			}
		}
	}
	if best == 0 {
		return 0, New(KindContractVersion, "contract",
			"no mutually supported contract version between client %v and server %v",
			clientVersions, SupportedContractVersions)
	}
	return best, nil
}
