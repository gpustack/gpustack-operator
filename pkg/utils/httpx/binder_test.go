// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0.

package httpx

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type bindFormTarget struct {
	Name  string `form:"name"`
	Count int    `form:"count"`
}

func TestBindFormURLEncoded(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("name=alice&count=3"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	var target bindFormTarget
	require.NoError(t, BindForm(r, &target))
	assert.Equal(t, "alice", target.Name)
	assert.Equal(t, 3, target.Count)
}

func TestBindFormMultipart(t *testing.T) {
	r := newMultipartRequest(t)

	var target bindFormTarget
	require.NoError(t, BindForm(r, &target))
	assert.Equal(t, "bob", target.Name)
	assert.Equal(t, 7, target.Count)
}

// newMultipartRequest returns a request carrying a multipart form with two
// values and one file.
func newMultipartRequest(t *testing.T) *http.Request {
	t.Helper()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	require.NoError(t, writer.WriteField("name", "bob"))
	require.NoError(t, writer.WriteField("count", "7"))
	file, err := writer.CreateFormFile("upload", "a.bin")
	require.NoError(t, err)
	_, err = file.Write([]byte("payload"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	r := httptest.NewRequest(http.MethodPost, "/", body)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	return r
}

func TestBindFormMultipartOverTheLimit(t *testing.T) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	require.NoError(t, writer.WriteField("name", "bob"))
	require.NoError(t, writer.WriteField("pad", strings.Repeat("a", _32MiB)))
	require.NoError(t, writer.Close())

	r := httptest.NewRequest(http.MethodPost, "/", body)
	r.Header.Set("Content-Type", writer.FormDataContentType())

	var target bindFormTarget
	require.Error(t, BindForm(r, &target))
}

func TestBindFormMultipartSideEffects(t *testing.T) {
	r := newMultipartRequest(t)

	var target bindFormTarget
	require.NoError(t, BindForm(r, &target))

	// The request carries the values it read, the way the request's own parser
	// leaves them.
	assert.Equal(t, []string{"bob"}, r.Form["name"])
	assert.Equal(t, []string{"bob"}, r.PostForm["name"])
	assert.Equal(t, []string{"7"}, r.Form["count"])
	assert.Equal(t, []string{"7"}, r.PostForm["count"])
	require.NotNil(t, r.MultipartForm)
	assert.Equal(t, []string{"bob"}, r.MultipartForm.Value["name"])
}

func TestBindFormMultipartAlreadyParsed(t *testing.T) {
	r := newMultipartRequest(t)

	// The request's own parser ran before, so the form carries the values.
	require.NoError(t, r.ParseMultipartForm(32<<20))

	var target bindFormTarget
	require.NoError(t, BindForm(r, &target))
	assert.Equal(t, "bob", target.Name)
	assert.Equal(t, 7, target.Count)
}

func TestBindFormMultipartCalledTwice(t *testing.T) {
	r := newMultipartRequest(t)

	var first bindFormTarget
	require.NoError(t, BindForm(r, &first))
	assert.Equal(t, "bob", first.Name)
	assert.Equal(t, 7, first.Count)

	// The second call finds the form the first one read, and binds it again.
	var second bindFormTarget
	require.NoError(t, BindForm(r, &second))
	assert.Equal(t, first, second)
}

func TestBindFormMultipartFormParsedBefore(t *testing.T) {
	r := newMultipartRequest(t)

	// The urlencoded parser ran before, so the request carries a form of its
	// own and the multipart values join it.
	require.NoError(t, r.ParseForm())

	var target bindFormTarget
	require.NoError(t, BindForm(r, &target))
	assert.Equal(t, "bob", target.Name)
	assert.Equal(t, 7, target.Count)
}

func TestBindFormMultipartBodyTakenOver(t *testing.T) {
	r := newMultipartRequest(t)

	// The caller took the body over, so the request says it cannot be read
	// again, and the binder passes that on.
	_, err := r.MultipartReader()
	require.NoError(t, err)

	var target bindFormTarget
	require.ErrorContains(t, BindForm(r, &target), "MultipartReader")
	assert.Equal(t, bindFormTarget{}, target)
}

func TestBindFormIgnoresWhatIsNotAForm(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		contentType string
	}{
		{
			name:   "a method without a body",
			method: http.MethodGet,
		},
		{
			name:        "a content type that is not a form",
			method:      http.MethodPost,
			contentType: "application/json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, "/", strings.NewReader("name=alice"))
			r.Header.Set("Content-Type", tt.contentType)

			var target bindFormTarget
			require.NoError(t, BindForm(r, &target))
			assert.Equal(t, bindFormTarget{}, target)
		})
	}
}
