package ui

import (
	"io"
	"net/http"
	"net/url"
	"os"

	"gpustack.ai/gpustack/pkg/server/settings"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/funcx"
	"gpustack.ai/gpustack/pkg/utils/httpx"
)

func Index() http.Handler {
	dir := system.SubDataDir("ui")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uiUrl := funcx.NoError(settings.ServeUiUrl.ValueURL(r.Context()))
		if uiUrl == nil {
			uiUrl = &url.URL{
				Scheme: "file",
				Path:   dir,
			}
		}

		serve(uiUrl).ServeHTTP(w, r)
	})
}

func serve(uri *url.URL) http.Handler {
	if uri.Scheme == "file" {
		return local(uri.Path)
	}
	return remote(uri.String())
}

func local(dir string) http.Handler {
	return http.FileServer(httpx.FS(os.DirFS(dir)))
}

func remote(uri string) http.HandlerFunc {
	cli := httpx.DefaultClient

	return func(w http.ResponseWriter, r *http.Request) {
		req, err := httpx.NewGetRequestWithContext(r.Context(), uri)
		if err != nil {
			httpx.Error(w, http.StatusNotFound)
			return
		}

		resp, err := cli.Do(req)
		if err != nil {
			httpx.Error(w, http.StatusGone)
			return
		}
		defer httpx.Close(resp)

		_, _ = io.Copy(w, resp.Body)
	}
}
