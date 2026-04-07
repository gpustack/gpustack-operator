package httpx

import (
	"errors"
	"net/http"
	"reflect"
	"strings"

	"github.com/gorilla/mux"

	"gpustack.ai/gpustack/pkg/utils/json"
)

const _32MiB = 32 << 20

var (
	ErrBindNilRequest = errors.New("http: nil request")
	ErrBindFuncNil    = errors.New("http: nil bind function")
)

// BindForm binds the passed struct pointer using the request form params.
func BindForm(r *http.Request, rc any) error {
	if r == nil {
		return ErrBindNilRequest
	}

	switch r.Method {
	default:
		return nil
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	}

	switch strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]) {
	default:
		return nil
	case "multipart/form-data", "application/x-www-form-urlencoded":
	}

	if err := r.ParseForm(); err != nil {
		return err
	}
	if err := r.ParseMultipartForm(_32MiB); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		return err
	}

	return MapFormWithTag(rc, r.Form, "form")
}

// BindJSON binds the passed struct pointer using the request json params.
func BindJSON(r *http.Request, rc any) error {
	if r == nil {
		return ErrBindNilRequest
	}

	switch r.Method {
	default:
		return nil
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	}

	switch strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]) {
	default:
		return nil
	case "application/json":
	}

	return json.NewDecoder(r.Body).Decode(rc)
}

// BindHeader binds the passed struct pointer using the request header params.
func BindHeader(r *http.Request, rc any) error {
	if r == nil {
		return ErrBindNilRequest
	}

	return MapFormWithTag(rc, r.Header, "header")
}

// BindQuery binds the passed struct pointer using the request query params.
func BindQuery(r *http.Request, rc any) error {
	if r == nil {
		return ErrBindNilRequest
	}

	return MapFormWithTag(rc, r.URL.Query(), "query")
}

// BindPath binds the passed struct pointer using the request path params.
func BindPath(r *http.Request, rc any) error {
	if r == nil {
		return ErrBindNilRequest
	}

	m := make(map[string][]string)
	for k, v := range mux.Vars(r) {
		m[k] = []string{v}
	}

	return MapFormWithTag(rc, m, "path")
}

// Bind binds the passed struct pointer using the request params.
//
// The binding process is based on the given struct tags.
func Bind(r *http.Request, rc any) error {
	if r == nil {
		return ErrBindNilRequest
	}

	rcv := reflect.ValueOf(rc)
	if rcv.Kind() == reflect.Ptr {
		rcv = rcv.Elem()
	}
	if rcv.Kind() == reflect.Map {
		return BindForm(r, rc)
	}

	bs := map[string]bool{
		"form":   false,
		"json":   false,
		"header": false,
		"query":  false,
		"path":   false,
	}

	rct := rcv.Type()
	for i := 0; i < rcv.NumField(); i++ {
		sf := rct.Field(i)
		if sf.PkgPath != "" && !sf.Anonymous { // Unexported.
			continue
		}

		_, ok := sf.Tag.Lookup("form")
		if ok {
			bs["form"] = true
			continue
		}

		_, ok = sf.Tag.Lookup("json")
		if ok {
			bs["json"] = true
			continue
		}

		_, ok = sf.Tag.Lookup("header")
		if ok {
			bs["header"] = true
			continue
		}

		_, ok = sf.Tag.Lookup("query")
		if ok {
			bs["query"] = true
			continue
		}

		_, ok = sf.Tag.Lookup("path")
		if ok {
			bs["path"] = true
			continue
		}
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodDelete:
		bs["json"] = false
		bs["form"] = false
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		switch strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]) {
		case "application/json":
			bs["json"] = true
			bs["form"] = false
		case "multipart/form-data", "application/x-www-form-urlencoded":
			bs["json"] = false
			bs["form"] = true
		}
	}

	if bs["json"] {
		if err := BindJSON(r, rc); err != nil {
			return err
		}
	}
	if bs["form"] {
		if err := BindForm(r, rc); err != nil {
			return err
		}
	}
	if bs["header"] {
		if err := BindHeader(r, rc); err != nil {
			return err
		}
	}
	if bs["query"] {
		if err := BindQuery(r, rc); err != nil {
			return err
		}
	}
	if bs["path"] {
		if err := BindPath(r, rc); err != nil {
			return err
		}
	}

	return nil
}

// BindFunc is a function that binds the passed struct pointer using the request params.
type BindFunc func(r *http.Request, rc any) error

// BindWith binds the passed struct pointer using the request params.
//
// The binding process is based on the given BindFunc.
func BindWith(r *http.Request, rc any, b BindFunc, bs ...BindFunc) error {
	if b == nil {
		return ErrBindFuncNil
	}

	if err := b(r, rc); err != nil {
		return err
	}

	for _, b := range bs {
		if b == nil {
			return ErrBindFuncNil
		}

		if err := b(r, rc); err != nil {
			return err
		}
	}

	return nil
}
