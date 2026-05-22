package httpx

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"

	klog "k8s.io/klog/v2"
)

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *loggingResponseWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *loggingResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *loggingResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *loggingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response writer does not support hijacking")
	}
	return hijacker.Hijack()
}

func (w *loggingResponseWriter) Push(target string, opts *http.PushOptions) error {
	pusher, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, opts)
}

func (w *loggingResponseWriter) ReadFrom(r io.Reader) (n int64, err error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if readerFrom, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return readerFrom.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}

func (w *loggingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// AccessLog logs request method/path and response status for each request.
func AccessLog(logger klog.Logger, queryAsValues bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			kvs := []any{
				"client", r.RemoteAddr,
				"method", r.Method,
				"path", r.URL.Path,
			}
			if queryAsValues {
				for k, v := range r.URL.Query() {
					if k == "token" || k == "password" {
						v = []string{"***"}
					}
					if len(v) == 1 {
						kvs = append(kvs, k, v[0])
					} else {
						kvs = append(kvs, k, v)
					}
				}
			}
			slg := logger.WithValues(kvs...)
			slg.Info("request")
			rw := &loggingResponseWriter{ResponseWriter: w}
			next.ServeHTTP(rw, r.WithContext(klog.NewContext(r.Context(), slg)))
			if rw.status == 0 {
				rw.status = http.StatusOK
			}
			if rw.status >= 400 {
				slg.Error(nil, "response", "status", rw.status)
			} else {
				slg.V(2).Info("response", "status", rw.status)
			}
		})
	}
}
