package ui

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gpustack.ai/gpustack/pkg/utils/httpx"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

// ResponseErrorWithCode responds in JSON with the given status code and error message.
func ResponseErrorWithCode(w http.ResponseWriter, code int, err error) {
	s := meta.Status{
		TypeMeta: meta.TypeMeta{
			APIVersion: "meta.k8s.io/v1",
			Kind:       "Status",
		},
		Status: meta.StatusFailure,
		Reason: meta.StatusReason(stringx.TrimAllSpace(http.StatusText(code))),
		Code:   int32(code),
	}
	if err != nil {
		s.Message = err.Error()
	}

	httpx.JSON(w, code, s)
}

// ResponseError is similar to ResponseErrorWithCode,
// but it analyzes the error and responds with the appropriate status code and error message.
func ResponseError(w http.ResponseWriter, err error) {
	rerr := errors.Unwrap(err)
	switch {
	case kerrors.IsInvalid(rerr):
		ResponseErrorWithCode(w, http.StatusBadRequest, err)
	case kerrors.IsBadRequest(rerr):
		ResponseErrorWithCode(w, http.StatusBadRequest, err)
	case kerrors.IsNotFound(rerr):
		ResponseErrorWithCode(w, http.StatusBadRequest, err)
	case kerrors.IsUnauthorized(rerr):
		ResponseErrorWithCode(w, http.StatusUnauthorized, err)
	default:
		ResponseErrorWithCode(w, http.StatusInternalServerError, err)
	}
}

// RedirectErrorWithCode redirects to the error page with the given status code and error message.
func RedirectErrorWithCode(w http.ResponseWriter, r *http.Request, c int, err error) {
	errMsg := http.StatusText(c)
	if err != nil {
		errMsg += ": "
		errMsg += err.Error()
	}

	u := "/#/redirect?"
	q := (url.Values{"code": {strconv.Itoa(c)}, "err": {errMsg}}).Encode()

	var sb strings.Builder
	sb.Grow(len(u) + len(q))
	sb.WriteString(u)
	sb.WriteString(q)

	// Redirect to the error page with the status code and error message.
	http.Redirect(w, r, sb.String(), http.StatusSeeOther)
}

// RedirectError is similar to RedirectErrorWithCode,
// but it analyzes the error and redirects to the error page with the appropriate status code and error message.
func RedirectError(w http.ResponseWriter, r *http.Request, err error) {
	rerr := errors.Unwrap(err)
	switch {
	case kerrors.IsInvalid(rerr):
		RedirectErrorWithCode(w, r, http.StatusBadRequest, err)
	case kerrors.IsBadRequest(rerr):
		RedirectErrorWithCode(w, r, http.StatusBadRequest, err)
	case kerrors.IsNotFound(rerr):
		RedirectErrorWithCode(w, r, http.StatusBadRequest, err)
	case kerrors.IsUnauthorized(rerr):
		RedirectErrorWithCode(w, r, http.StatusUnauthorized, err)
	default:
		RedirectErrorWithCode(w, r, http.StatusInternalServerError, err)
	}
}
