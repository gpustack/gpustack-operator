package httpx

import (
	"net/http"

	"gpustack.ai/gpustack/pkg/utils/bytex"
	"gpustack.ai/gpustack/pkg/utils/json"
)

func PureJSON(w http.ResponseWriter, code int, v any) {
	buf := bytex.GetBuffer()
	defer bytex.Put(buf)

	err := json.NewEncoder(buf).Encode(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(buf.Bytes())
}

func JSON(w http.ResponseWriter, code int, v any) {
	buf := bytex.GetBuffer()
	defer bytex.Put(buf)

	err := json.NewEncoder(buf).Encode(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(buf.Bytes())
}

// Error is similar to http.Error,
// but it can get the error message by the given code.
func Error(w http.ResponseWriter, code int) {
	http.Error(w, http.StatusText(code), code)
}
