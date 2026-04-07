package webserver

import (
	"net/http"
	"time"
)

// newHttpServer creates a new http.Server with the given handler and some default settings.
func newHttpServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		MaxHeaderBytes:    1 << 20,
		IdleTimeout:       90 * time.Second,
		ReadHeaderTimeout: 32 * time.Second,
	}
}
