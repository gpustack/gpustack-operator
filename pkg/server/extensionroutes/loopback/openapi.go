package loopback

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

	decorateLoopbackPath(spec.Paths, prefix)
}

func decorateLoopbackPath(spec *openspec3.Paths, prefix string) {
	pJson := `
{
    "get": {
        "tags": [
            "Loopback"
        ],
        "description": "proxy get request to loopback kubernetes api server",
        "operationId": "getLoopback",
        "security": [{"CookieAuth":[]},{"BearerAuth":[]}],
        "parameters": [
            {
                "name": "k8sapi",
                "in": "path",
                "required": true,
                "schema": {
                    "type": "string"
                },
                "description": "kubernetes api path"
            }
        ],
        "responses": {
            "200": {
                "description": "OK"
            }
        }
    },
    "put": {
        "tags": [
            "Loopback"
        ],
        "description": "proxy put request to loopback kubernetes api server",
        "operationId": "putLoopback",
        "security": [{"CookieAuth":[]},{"BearerAuth":[]}],
        "parameters": [
            {
                "name": "k8sapi",
                "in": "path",
                "required": true,
                "schema": {
                    "type": "string"
                },
                "description": "kubernetes api path"
            }
        ],
        "requestBody": {
            "content": {
                "application/json": {
                    "schema": {
                        "type": "object"
                    }
                }
            }
        },
        "responses": {
            "200": {
                "description": "OK"
            }
        }
    },
    "post": {
        "tags": [
            "Loopback"
        ],
        "description": "proxy post request to loopback kubernetes api server",
        "operationId": "postLoopback",
        "security": [{"CookieAuth":[]},{"BearerAuth":[]}],
        "parameters": [
            {
                "name": "k8sapi",
                "in": "path",
                "required": true,
                "schema": {
                    "type": "string"
                },
                "description": "kubernetes api path"
            }
        ],
        "requestBody": {
            "content": {
                "application/json": {
                    "schema": {
                        "type": "object"
                    }
                }
            }
        },
        "responses": {
            "200": {
                "description": "OK"
            }
        }
    },
    "delete": {
        "tags": [
            "Loopback"
        ],
        "description": "proxy delete request to loopback kubernetes api server",
        "operationId": "deleteLoopback",
        "security": [{"CookieAuth":[]},{"BearerAuth":[]}],
        "parameters": [
            {
                "name": "k8sapi",
                "in": "path",
                "required": true,
                "schema": {
                    "type": "string"
                },
                "description": "kubernetes api path"
            }
        ],
        "responses": {
            "200": {
                "description": "OK"
            }
        }
    },
    "patch": {
        "tags": [
            "Loopback"
        ],
        "description": "proxy patch request to loopback kubernetes api server",
        "operationId": "patchLoopback",
        "security": [{"CookieAuth":[]},{"BearerAuth":[]}],
        "parameters": [
            {
                "name": "k8sapi",
                "in": "path",
                "required": true,
                "schema": {
                    "type": "string"
                },
                "description": "kubernetes api path"
            }
        ],
        "responses": {
            "200": {
                "description": "OK"
            }
        }
    }
}
`

	p := new(openspec3.Path)
	json.MustUnmarshal(stringx.ToBytes(&pJson), p)
	spec.Paths[path.Join(prefix, "{k8sapi}")] = p
}
