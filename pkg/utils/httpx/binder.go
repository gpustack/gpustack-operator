package httpx

import (
	"errors"
	"net/http"
	"reflect"
	"strings"

	"github.com/gorilla/mux"

	"gpustack.ai/gpustack/pkg/utils/json"
)

// _32MiB is the largest request body a multipart form is read from.
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
	case "multipart/form-data":
		return bindMultipartForm(r, rc)
	case "application/x-www-form-urlencoded":
	}

	// The body of an urlencoded form is read by the request's own parser,
	// which caps its size. The cap is left in place: a smaller one here would
	// only replace it.
	if err := r.ParseForm(); err != nil {
		return err
	}

	return MapFormWithTag(rc, r.Form, "form")
}

// bindMultipartForm binds the passed struct pointer using the values of a
// multipart form.
//
// The parser reads the whole body before it hands any of it over, and the body
// is of no known size, so the body it reads is bounded. The parser reads a
// request built from the one it was given, carrying the state that request
// already holds, and the state it finds is handed back.
func bindMultipartForm(r *http.Request, rc any) error {
	// The helper has no response writer to hand, so the reader cannot ask the
	// server to close the connection; the limit is what bounds the work.
	body := r.Body
	if body == nil {
		body = http.NoBody
	}
	body = http.MaxBytesReader(nil, body, _32MiB)

	parsed := &http.Request{
		Method:        r.Method,
		Header:        r.Header,
		URL:           r.URL,
		Body:          body,
		ContentLength: r.ContentLength,
		Form:          r.Form,
		PostForm:      r.PostForm,
		MultipartForm: r.MultipartForm,
	}

	if err := parsed.ParseForm(); err != nil {
		r.Form = parsed.Form
		r.PostForm = parsed.PostForm
		return err
	}
	if err := parsed.ParseMultipartForm(_32MiB); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		r.Form = parsed.Form
		r.PostForm = parsed.PostForm
		r.MultipartForm = parsed.MultipartForm
		return err
	}

	r.Form = parsed.Form
	r.PostForm = parsed.PostForm
	r.MultipartForm = parsed.MultipartForm

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
	if rcv.Kind() == reflect.Pointer {
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
