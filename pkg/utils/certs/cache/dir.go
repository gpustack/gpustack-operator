package cache

import (
	"golang.org/x/crypto/acme/autocert"

	"gpustack.ai/gpustack/pkg/utils/certs"
)

// NewDirCache returns a new DirCache instance with the given directory.
func NewDirCache(dir string) certs.Cache {
	return autocert.DirCache(dir)
}
