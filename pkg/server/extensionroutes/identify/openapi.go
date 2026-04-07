package identify

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
		decorateComponents(spec)
		return spec
	}
}

func decoratePaths(spec *openspec3.OpenAPI, prefix string) {
	if spec.Paths == nil {
		spec.Paths = &openspec3.Paths{
			Paths: map[string]*openspec3.Path{},
		}
	}

	decorateListProvidersPath(spec.Paths, prefix)
	decorateLoginPath(spec.Paths, prefix)
	decorateCallbackPath(spec.Paths, prefix)
	decorateProfilePath(spec.Paths, prefix)
	decorateTokenPath(spec.Paths, prefix)
	decorateRulesPath(spec.Paths, prefix)
	decorateLogoutPath(spec.Paths, prefix)
}

func decorateListProvidersPath(spec *openspec3.Paths, prefix string) {
	pJson := `
{
    "get": {
        "tags": [
            "Identify"
        ],
        "description": "list all providers of identify",
        "operationId": "listIdentifyProviders",
        "responses": {
            "200": {
                "description": "OK",
                "content": {
                    "application/json": {
                        "schema": {
                            "type": "object",
                            "properties": {
                                "items": {
                                    "type": "array",
                                    "items": {
                                        "type": "object",
                                        "properties": {
                                            "name": {
                                                "type": "string"
                                            },
                                            "type": {
                                                "type": "string"
                                            },
                                            "displayName": {
                                                "type": "string"
                                            },
                                            "description": {
                                                "type": "string"
                                            },
                                            "loginWithPassword": {
                                                "type": "boolean"
                                            }
                                        }
                                    }
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
	spec.Paths[path.Join(prefix, "providers")] = p
}

func decorateLoginPath(spec *openspec3.Paths, prefix string) {
	pJson := `
{
    "get": {
        "tags": [
            "Identify"
        ],
        "description": "login identify via oauth",
        "operationId": "loginIdentifyViaOAuth",
        "parameters": [
            {
                "name": "provider",
                "in": "query",
                "description": "Provider is the name of subject provider who provides this subject.",
                "required": true,
                "schema": {
                    "type": "string"
                }
            }
        ],
        "responses": {
            "302": {
                "description": "Redirect"
            }
        }
    },
    "post": {
        "tags": [
            "Identify"
        ],
        "description": "login identify via password",
        "operationId": "loginIdentifyViaPassword",
        "parameters": [
            {
                "name": "provider",
                "in": "query",
                "description": "Provider is the name of subject provider who provides this subject.",
                "schema": {
                    "type": "string"
                }
            }
        ],
        "requestBody": {
            "content": {
                "application/json": {
                    "schema": {
                        "type": "object",
                        "properties": {
                            "username": {
                                "type": "string",
                                "description": "Username is the username of the subject."
                            },
                            "password": {
                                "type": "string",
                                "format": "password",
                                "description": "Password is the password of the subject."
                            }
                        }
                    }
                }
            },
            "required": true
        },
        "responses": {
            "200": {
                "description": "OK",
                "headers": {
                    "Set-Cookie": {
                        "schema": {
                            "type": "string",
                            "example": "gpustack_session=abcde12345; Path=/; HttpOnly"
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
	spec.Paths[path.Join(prefix, "login")] = p
}

func decorateCallbackPath(spec *openspec3.Paths, prefix string) {
	pJson := `
{
    "get": {
        "tags": [
            "Identify"
        ],
        "description": "login callback of identify",
        "operationId": "loginIdentifyCallback",
        "parameters": [
            {
                "name": "provider",
                "in": "path",
                "description": "Provider is the name of subject provider who provides this subject.",
                "required": true,
                "schema": {
                    "type": "string"
                }
            },
            {
                "name": "code",
                "in": "query",
                "description": "Code is for OAuth exchange.",
                "required": true,
                "schema": {
                    "type": "string"
                }
            },
            {
                "name": "state",
                "in": "query",
                "description": "State is for GPUStack verify the OAuth request.",
                "required": true,
                "schema": {
                    "type": "string"
                }
            }
        ],
        "responses": {
            "302": {
                "description": "Redirect",
                "headers": {
                    "Set-Cookie": {
                        "schema": {
                            "type": "string",
                            "example": "gpustack_session=abcde12345; Path=/; HttpOnly"
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
	spec.Paths[path.Join(prefix, "callback", "{provider}")] = p
}

func decorateProfilePath(spec *openspec3.Paths, prefix string) {
	pJson := `
{
    "get": {
        "tags": [
            "Identify"
        ],
        "description": "get profile of identify",
        "operationId": "getIdentifyProfile",
        "security": [{"CookieAuth":[]},{"BearerAuth":[]}],
        "responses": {
            "200": {
                "description": "OK",
                "content": {
                    "application/json": {
                        "schema": {
                            "type": "object",
                            "properties": {
                                "description": {
                                    "description": "Description is the description of the subject.",
                                    "type": "string"
                                },
                                "displayName": {
                                    "description": "DisplayName is the display name of the subject.",
                                    "type": "string"
                                },
                                "email": {
                                    "description": "Email is the email of the subject.",
                                    "type": "string",
                                    "default": ""
                                },
                                "groups": {
                                    "description": "Groups is the groups that the subject belongs to.",
                                    "type": "array",
                                    "uniqueItems": true,
                                    "items": {
                                        "type": "string",
                                        "default": ""
                                    }
                                },
                                "name": {
                                    "description": "Name of the subject.",
                                    "type": "string",
                                    "default": ""
                                },
                                "provider": {
                                    "description": "Provider is the name of subject provider who provides this subject, which is immutable.",
                                    "type": "string",
                                    "default": ""
                                },
                                "role": {
                                    "description": "Role is the role of the subject.",
                                    "type": "string",
                                    "default": "",
                                    "enum": [
                                        "Admin",
                                        "Manager",
                                        "User"
                                    ]
                                }
                            }
                        }
                    }
                }
            }
        }
    },
    "put": {
        "tags": [
            "Identify"
        ],
        "description": "update profile of identify",
        "operationId": "updateIdentifyProfile",
        "security": [{"CookieAuth":[]},{"BearerAuth":[]}],
        "requestBody": {
            "content": {
                "application/json": {
                    "schema": {
                        "type": "object",
                        "properties": {
                            "displayName": {
                                "type": "string",
                                "description": "DisplayName is the display name of the environment."
                            },
                            "email": {
                                "type": "string",
                                "description": "Email is the email of the subject."
                            },
                            "originalPassword": {
                                "type": "string",
                                "description": "Original password is the original password of the subject used to verify."
                            },
                            "password": {
                                "type": "string",
                                "description": "Password is the new password of the subject."
                            }
                        }
                    }
                }
            },
            "required": true
        },
        "responses": {
            "200": {
                "description": "OK",
                "content": {
                    "application/json": {
                        "schema": {
                            "type": "object",
                            "properties": {
                                "description": {
                                    "description": "Description is the description of the subject.",
                                    "type": "string"
                                },
                                "displayName": {
                                    "description": "DisplayName is the display name of the subject.",
                                    "type": "string"
                                },
                                "email": {
                                    "description": "Email is the email of the subject.",
                                    "type": "string",
                                    "default": ""
                                },
                                "groups": {
                                    "description": "Groups is the groups that the subject belongs to.",
                                    "type": "array",
                                    "uniqueItems": true,
                                    "items": {
                                        "type": "string",
                                        "default": ""
                                    }
                                },
                                "name": {
                                    "description": "Name of the subject.",
                                    "type": "string",
                                    "default": ""
                                },
                                "provider": {
                                    "description": "Provider is the name of subject provider who provides this subject, which is immutable.",
                                    "type": "string",
                                    "default": ""
                                },
                                "role": {
                                    "description": "Role is the role of the subject.",
                                    "type": "string",
                                    "default": "",
                                    "enum": [
                                        "Admin",
                                        "Manager",
                                        "User"
                                    ]
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
	spec.Paths[path.Join(prefix, "profile")] = p
}

func decorateTokenPath(spec *openspec3.Paths, prefix string) {
	pJson := `
{
    "get": {
        "tags": [
            "Identify"
        ],
        "description": "get token of identify",
        "operationId": "getIdentifyToken",
        "security": [{"CookieAuth":[]},{"BearerAuth":[]}],
        "parameters": [
            {
                "name": "expirationSeconds",
                "in": "query",
                "description": "Expiration seconds of token.",
                "required": false,
                "schema": {
                    "type": "integer",
                    "format": "int64"
                }
            }
        ],
        "responses": {
            "200": {
                "description": "OK",
                "content": {
                    "application/json": {
                        "schema": {
                            "$ref": "#/components/schemas/com.github.gpustack.anystack.pkg.server.apis.server.v1.SubjectTokenStatus"
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
	spec.Paths[path.Join(prefix, "token")] = p
}

func decorateRulesPath(spec *openspec3.Paths, prefix string) {
	pJson := `
{
    "get": {
        "tags": [
            "Identify"
        ],
        "description": "get rules of identify",
        "operationId": "getIdentifyRules",
        "security": [{"CookieAuth":[]},{"BearerAuth":[]}],
        "parameters": [
            {
                "name": "namespace",
                "in": "path",
                "description": "object name and auth scope, such as for teams and projects",
                "required": true,
                "schema": {
                    "type": "string",
                    "uniqueItems": true
                }
            }
        ],
        "responses": {
            "200": {
                "description": "OK",
                "content": {
                    "application/json": {
                        "schema": {
                            "type": "object",
                            "properties": {
                                "items": {
                                    "type": "array",
                                    "items": {
                                        "type": "object",
                                        "properties": {
                                            "verbs": {
                                                "type": "array",
                                                "items": {
                                                    "type": "string"
                                                }
                                            },
                                            "apiGroups": {
                                                "type": "array",
                                                "items": {
                                                    "type": "string"
                                                }
                                            },
                                            "resources": {
                                                "type": "array",
                                                "items": {
                                                    "type": "string"
                                                }
                                            },
                                            "resourceNames": {
                                                "type": "array",
                                                "items": {
                                                    "type": "string"
                                                }
                                            }
                                        }
                                    }
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
	spec.Paths[path.Join(prefix, "rules", "{namespace}")] = p
}

func decorateLogoutPath(spec *openspec3.Paths, prefix string) {
	pJson := `
{
    "get": {
        "tags": [
            "Identify"
        ],
        "description": "logout identify",
        "operationId": "logoutIdentify",
        "security": [{"CookieAuth":[]},{"BearerAuth":[]}],
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
	spec.Paths[path.Join(prefix, "logout")] = p
}

func decorateComponents(spec *openspec3.OpenAPI) {
	if spec.Components == nil {
		spec.Components = &openspec3.Components{}
	}

	decorateComponentsSecuritySchemes(spec.Components)
}

func decorateComponentsSecuritySchemes(spec *openspec3.Components) {
	if spec.SecuritySchemes == nil {
		spec.SecuritySchemes = map[string]*openspec3.SecurityScheme{}
	}

	spec.SecuritySchemes["CookieAuth"] = &openspec3.SecurityScheme{
		SecuritySchemeProps: openspec3.SecuritySchemeProps{
			Type:        "apiKey",
			In:          "cookie",
			Name:        _AuthenticationCookie,
			Description: "Cookie Authentication, the value must be a valid GPUStack token.",
		},
	}
}
