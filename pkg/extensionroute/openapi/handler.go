package openapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/golang/groupcache/singleflight"
	"github.com/gorilla/mux"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	openspec3 "k8s.io/kube-openapi/pkg/spec3"
	openvalidatespec "k8s.io/kube-openapi/pkg/validation/spec"

	"gpustack.ai/gpustack/pkg/utils/httpx"
	"gpustack.ai/gpustack/pkg/utils/json"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"k8s.io/apimachinery/pkg/util/sets"
)

// Extender extends the given OpenAPI3 spec and return it.
type Extender func(spec *openspec3.OpenAPI) *openspec3.OpenAPI

// Route registers the OpenAPI route with the given extenders.
func Route(gvs []meta.GroupVersion, r *mux.Route, extenders ...Extender) {
	p, _ := r.GetPathTemplate()
	r.Handler(http.StripPrefix(p, index(gvs, extenders...)))
}

func index(gvs []meta.GroupVersion, extenders ...Extender) http.Handler {
	in := interceptor{
		gvs: gvs,
		e:   extenders,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bs, err := in.Proxy(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bs)
	})
}

type interceptor struct {
	gvs []meta.GroupVersion
	e   []Extender
	v   atomic.Value
	g   singleflight.Group
}

func (l *interceptor) Proxy(r *http.Request) ([]byte, error) {
	if v := l.v.Load(); v != nil {
		return v.([]byte), nil
	}

	j, err := l.g.Do("", func() (any, error) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		var s *openspec3.OpenAPI
		for _, gv := range l.gvs {
			reqURL := fmt.Sprintf("https://%s/openapi/v3/apis/%s/%s", r.Host, gv.Group, gv.Version)

			req, err := httpx.NewGetRequestWithContext(ctx, reqURL)
			if err != nil {
				return nil, fmt.Errorf("new request: %w", err)
			}

			resp, err := httpx.DefaultInsecureClient.Do(req)
			if err != nil {
				return nil, fmt.Errorf("do request: %w", err)
			}

			oas := new(openspec3.OpenAPI)
			_ = json.NewDecoder(resp.Body).Decode(oas)
			httpx.Close(resp)

			if s == nil {
				s = oas
				continue
			}
			mergeOpenAPI(oas, s)
		}

		s = decorate(s)
		for i := range l.e {
			if l.e[i] == nil {
				continue
			}
			s = l.e[i](s)
		}

		v, err := json.Marshal(s)
		if err != nil {
			return nil, fmt.Errorf("encode openapi: %w", err)
		}

		l.v.Store(v)
		return v, nil
	})
	if err != nil {
		return nil, err
	}

	return j.([]byte), nil
}

func decorate(spec *openspec3.OpenAPI) *openspec3.OpenAPI {
	decorateInfo(spec)
	decoratePaths(spec)
	decorateComponents(spec)

	return spec
}

func decorateInfo(spec *openspec3.OpenAPI) {
	if spec.Info == nil {
		spec.Info = &openvalidatespec.Info{}
	}

	spec.Info.Description = "Restful APIs to access GPUStack."
}

func decoratePaths(spec *openspec3.OpenAPI) {
	if spec.Paths == nil {
		spec.Paths = &openspec3.Paths{
			Paths: map[string]*openspec3.Path{},
		}
	}

	var prefix string
	pResM := map[string]string{}
	for _, path := range sets.List(sets.KeySet(spec.Paths.Paths)) {
		if strings.HasSuffix(path, "/") {
			prefix = path
			delete(spec.Paths.Paths, path)
			continue
		}
		if prefix == "" || !strings.HasPrefix(path, prefix) {
			continue
		}

		var pRes string
		{
			var pPath string
			pRes, pPath, _ = strings.Cut(path[len(prefix):], "/")
			for ; pPath != "" && pPath != "{name}"; {
				if pRes != "namespaces" && pRes != "{namespace}" && pRes != "watch" {
					break
				}
				pRes, pPath, _ = strings.Cut(pPath, "/")
			}
		}
		if pRes == "" {
			continue
		}

		pathV := spec.Paths.Paths[path]
		decoratePathOperation(pathV.Get, pResM, pRes)
		decoratePathOperation(pathV.Put, pResM, pRes)
		decoratePathOperation(pathV.Post, pResM, pRes)
		decoratePathOperation(pathV.Delete, pResM, pRes)
		decoratePathOperation(pathV.Options, pResM, pRes)
		decoratePathOperation(pathV.Head, pResM, pRes)
		decoratePathOperation(pathV.Patch, pResM, pRes)
		decoratePathOperation(pathV.Trace, pResM, pRes)
		decoratePathParameters(pathV.Parameters)
	}
}

func decoratePathOperation(spec *openspec3.Operation, pResM map[string]string, pRes string) {
	if spec == nil {
		return
	}

	// Secure operations.
	spec.SecurityRequirement = []map[string][]string{
		{"BearerAuth": {}},
	}

	// Replace request body.
	if spec.RequestBody != nil && spec.RequestBody.Content != nil {
		if v, ok := spec.RequestBody.Content["*/*"]; ok {
			spec.RequestBody.Content["application/json"] = v
			delete(spec.RequestBody.Content, "*/*")
		}
	}

	// Parameters.
	decoratePathParameters(spec.Parameters)

	// Retag operations.
	if kind := pResM[pRes]; kind != "" {
		spec.Tags = []string{kind}
		return
	}
	{
		var gvk _GroupVersionKind
		if spec.Extensions["x-kubernetes-group-version-kind"] != nil {
			_ = spec.Extensions.GetObject("x-kubernetes-group-version-kind", &gvk)
			if gvk.Validate() {
				kind := gvk.Kind
				res := strings.ToLower(stringx.Pluralize(kind))
				if res == pRes {
					pResM[pRes] = kind
					spec.Tags = []string{kind}
					return
				}
			}
		}
	}
	spec.Tags = []string{stringx.Capitalize(pRes)}
}

func decoratePathParameters(params []*openspec3.Parameter) {
	for i := range params {
		if params[i] == nil || params[i].Schema == nil {
			continue
		}
		param := params[i]
		switch param.Name {
		default:
			continue
		case "fieldManager":
			param.Schema.Default = "gpustack-swagger-ui"
		case "fieldValidation":
			param.Schema.Default = "Ignore"
			param.Schema.Enum = []any{
				"Ignore",
				"Warn",
				"Strict",
			}
		case "propagationPolicy":
			param.Schema.Default = "Background"
			param.Schema.Enum = []any{
				"Background",
				"Foreground",
				"Orphan",
			}
		}
	}
}

func decorateComponents(spec *openspec3.OpenAPI) {
	if spec.Components == nil {
		spec.Components = &openspec3.Components{}
	}

	decorateComponentsSchemas(spec.Components)
	decorateComponentsSecuritySchemes(spec.Components)
}

func decorateComponentsSchemas(spec *openspec3.Components) {
	if spec.Schemas == nil {
		spec.Schemas = map[string]*openvalidatespec.Schema{}
	}

	for loc := range spec.Schemas {
		if !strings.HasPrefix(loc, "ai.gpustack.gpustack.") {
			continue
		}
		spec := spec.Schemas[loc]
		var gvkl _GroupVersionKindList
		{
			if spec.Extensions["x-kubernetes-group-version-kind"] == nil {
				continue
			}
			_ = spec.Extensions.GetObject("x-kubernetes-group-version-kind", &gvkl)
			if !gvkl.Validate() {
				continue
			}
		}
		if s, ok := spec.Properties["apiVersion"]; ok {
			s.Default = gvkl[0].APIVersion()
			spec.Properties["apiVersion"] = s
		}
		if s, ok := spec.Properties["kind"]; ok {
			s.Default = gvkl[0].Kind
			spec.Properties["kind"] = s
		}
	}
}

func decorateComponentsSecuritySchemes(spec *openspec3.Components) {
	if spec.SecuritySchemes == nil {
		spec.SecuritySchemes = map[string]*openspec3.SecurityScheme{}
	}

	spec.SecuritySchemes["BearerAuth"] = &openspec3.SecurityScheme{
		SecuritySchemeProps: openspec3.SecuritySchemeProps{
			Type:        "http",
			In:          "header",
			Scheme:      "bearer",
			Description: "Bearer Authentication, the token must be a valid GPUStack token.",
		},
	}
}

type _GroupVersionKind struct {
	Group   string `json:"group"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
}

func (gvk _GroupVersionKind) APIVersion() string {
	if gvk.Group == "" || gvk.Group == "core" {
		return gvk.Version
	}
	return gvk.Group + "/" + gvk.Version
}

func (gvk _GroupVersionKind) Validate() bool {
	return gvk.Group != "" && gvk.Version != "" && gvk.Kind != ""
}

type _GroupVersionKindList []_GroupVersionKind

func (gvkl _GroupVersionKindList) Validate() bool {
	return len(gvkl) == 1 && gvkl[0].Validate()
}

func mergeOpenAPI(src, dst *openspec3.OpenAPI) {
	if src.Paths != nil {
		for path, pathV := range src.Paths.Paths {
			if dst.Paths.Paths == nil {
				dst.Paths.Paths = make(map[string]*openspec3.Path)
			}
			if _, ok := dst.Paths.Paths[path]; !ok {
				dst.Paths.Paths[path] = pathV
			}
		}
		for ext, extV := range src.Paths.Extensions {
			if dst.Paths.Extensions == nil {
				dst.Paths.Extensions = make(map[string]any)
			}
			if _, ok := dst.Paths.Extensions[ext]; !ok {
				dst.Paths.Extensions[ext] = extV
			}
		}
	}
	if src.Components != nil {
		for loc, schema := range src.Components.Schemas {
			if dst.Components.Schemas == nil {
				dst.Components.Schemas = make(map[string]*openvalidatespec.Schema)
			}
			if _, ok := dst.Components.Schemas[loc]; !ok {
				dst.Components.Schemas[loc] = schema
			}
		}
		for name, sec := range src.Components.SecuritySchemes {
			if dst.Components.SecuritySchemes == nil {
				dst.Components.SecuritySchemes = make(map[string]*openspec3.SecurityScheme)
			}
			if _, ok := dst.Components.SecuritySchemes[name]; !ok {
				dst.Components.SecuritySchemes[name] = sec
			}
		}
		for name, resp := range src.Components.Responses {
			if dst.Components.Responses == nil {
				dst.Components.Responses = make(map[string]*openspec3.Response)
			}
			if _, ok := dst.Components.Responses[name]; !ok {
				dst.Components.Responses[name] = resp
			}
		}
		for name, param := range src.Components.Parameters {
			if dst.Components.Parameters == nil {
				dst.Components.Parameters = make(map[string]*openspec3.Parameter)
			}
			if _, ok := dst.Components.Parameters[name]; !ok {
				dst.Components.Parameters[name] = param
			}
		}
		for name, example := range src.Components.Examples {
			if dst.Components.Examples == nil {
				dst.Components.Examples = make(map[string]*openspec3.Example)
			}
			if _, ok := dst.Components.Examples[name]; !ok {
				dst.Components.Examples[name] = example
			}
		}
		for name, req := range src.Components.RequestBodies {
			if dst.Components.RequestBodies == nil {
				dst.Components.RequestBodies = make(map[string]*openspec3.RequestBody)
			}
			if _, ok := dst.Components.RequestBodies[name]; !ok {
				dst.Components.RequestBodies[name] = req
			}
		}
		for name, link := range src.Components.Links {
			if dst.Components.Links == nil {
				dst.Components.Links = make(map[string]*openspec3.Link)
			}
			if _, ok := dst.Components.Links[name]; !ok {
				dst.Components.Links[name] = link
			}
		}
		for name, header := range src.Components.Headers {
			if dst.Components.Headers == nil {
				dst.Components.Headers = make(map[string]*openspec3.Header)
			}
			if _, ok := dst.Components.Headers[name]; !ok {
				dst.Components.Headers[name] = header
			}
		}
	}
}
