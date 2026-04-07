package wellknown

import (
	"path"

	openspec3 "k8s.io/kube-openapi/pkg/spec3"

	"gpustack.ai/gpustack/pkg/extensionroute/openapi"
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

func getOpenapiDecorate(prefix string) openapi.Extender {
	return func(spec *openspec3.OpenAPI) *openspec3.OpenAPI {
		decoratePaths(spec, prefix)
		return spec
	}
}

func decoratePaths(spec *openspec3.OpenAPI, prefix string) {
	if spec.Paths == nil {
		spec.Paths = &openspec3.Paths{
			Paths: map[string]*openspec3.Path{},
		}
	}

	decorateBootstrapPath(spec.Paths, prefix)
}

func decorateBootstrapPath(spec *openspec3.Paths, prefix string) {
	pJson := `
{
	"get": {
		"tags": [
			"WellKnown"
		],
		"description": "get metadata of peer for bootstrapping the p2p network",
		"operationId": "getPeer",
		"responses": {
			"200": {
				"description": "OK",
				"content": {
					"application/json": {
						"schema": {
							"type": "object",
							"properties": {
								"bootstrapPeerIDs": {
									"type": "array",
									"items": {
										"type": "string"
									}
								},
								"bootstrapPort": {
									"type": "integer"
								}
							}
						}
					}
				}
			}
        }
	}
}
`

	p := new(openspec3.Path)
	json.MustUnmarshal(stringx.ToBytes(&pJson), p)
	spec.Paths[path.Join(prefix, "/peer")] = p
}
