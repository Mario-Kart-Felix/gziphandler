package gziphandler

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	smallTestBody = "aaabbcaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbccc"
	testBody      = "aaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbccc"
)

func TestGzipHandler(t *testing.T) {
	// This just exists to provide something for GzipHandler to wrap.
	handler := newTestHandler(testBody)

	// requests without accept-encoding are passed along as-is

	req1 := httptest.NewRequest(http.MethodGet, "/whatever", nil)
	resp1 := httptest.NewRecorder()
	handler.ServeHTTP(resp1, req1)
	res1 := resp1.Result()

	assert.Equal(t, http.StatusOK, res1.StatusCode)
	assert.Equal(t, "", res1.Header.Get("Content-Encoding"))
	assert.Equal(t, "Accept-Encoding", res1.Header.Get("Vary"))
	assert.Equal(t, testBody, resp1.Body.String())

	// but requests with accept-encoding:gzip are compressed if possible

	req2 := httptest.NewRequest(http.MethodGet, "/whatever", nil)
	req2.Header.Set("Accept-Encoding", "gzip")
	resp2 := httptest.NewRecorder()
	handler.ServeHTTP(resp2, req2)
	res2 := resp2.Result()

	assert.Equal(t, http.StatusOK, res2.StatusCode)
	assert.Equal(t, "gzip", res2.Header.Get("Content-Encoding"))
	assert.Equal(t, "Accept-Encoding", res2.Header.Get("Vary"))
	assert.Equal(t, gzipStrLevel(testBody, gzip.DefaultCompression), resp2.Body.Bytes())

	// content-type header is correctly set based on uncompressed body

	req3 := httptest.NewRequest(http.MethodGet, "/whatever", nil)
	req3.Header.Set("Accept-Encoding", "gzip")
	res3 := httptest.NewRecorder()
	handler.ServeHTTP(res3, req3)

	assert.Equal(t, http.DetectContentType([]byte(testBody)), res3.Header().Get("Content-Type"))
}

func TestGzipLevelHandler(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, testBody)
	})

	for lvl := gzip.BestSpeed; lvl <= gzip.BestCompression; lvl++ {
		req := httptest.NewRequest(http.MethodGet, "/whatever", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		resp := httptest.NewRecorder()
		Gzip(handler, CompressionLevel(lvl)).ServeHTTP(resp, req)
		res := resp.Result()

		assert.Equal(t, http.StatusOK, res.StatusCode)
		assert.Equal(t, "gzip", res.Header.Get("Content-Encoding"))
		assert.Equal(t, "Accept-Encoding", res.Header.Get("Vary"))
		assert.Equal(t, gzipStrLevel(testBody, lvl), resp.Body.Bytes())
	}
}

func TestCompressionLevelPanicsForInvalid(t *testing.T) {
	assert.Panics(t, func() {
		CompressionLevel(-42)
	}, "CompressionLevel did not panic on invalid level")

	assert.Panics(t, func() {
		CompressionLevel(42)
	}, "CompressionLevel did not panic on invalid level")
}

func TestGzipHandlerNoBody(t *testing.T) {
	tests := []struct {
		statusCode      int
		contentEncoding string
		bodyLen         int
	}{
		// Body must be empty.
		{http.StatusNoContent, "", 0},
		{http.StatusNotModified, "", 0},
		// Body is going to get gzip'd no matter what.
		{http.StatusOK, "", 0},
	}

	for num, test := range tests {
		handler := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(test.statusCode)
		}))

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		handler.ServeHTTP(rec, req)

		header := rec.Header()
		assert.Equal(t, test.contentEncoding, header.Get("Content-Encoding"), fmt.Sprintf("for test iteration %d", num))
		assert.Equal(t, "Accept-Encoding", header.Get("Vary"), fmt.Sprintf("for test iteration %d", num))
		assert.Equal(t, test.bodyLen, rec.Body.Len(), fmt.Sprintf("for test iteration %d", num))
	}
}

func TestGzipHandlerContentLength(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test: no external network in -short mode")
	}

	b := []byte(testBody)
	handler := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Write(b)
	}))
	// httptest.NewRecorder doesn't give you access to the Content-Length
	// header so instead, we create a server on a random port and make
	// a request to that instead
	ln, err := net.Listen("tcp", "127.0.0.1:")
	if err != nil {
		t.Fatalf("failed creating listen socket: %v", err)
	}
	defer ln.Close()
	srv := &http.Server{
		Handler: handler,
	}
	go srv.Serve(ln)

	req := &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Path: "/", Scheme: "http", Host: ln.Addr().String()},
		Header: make(http.Header),
		Close:  true,
	}
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Unexpected error making http request: %v", err)
	}
	defer res.Body.Close()

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("Unexpected error reading response body: %v", err)
	}

	l, err := strconv.Atoi(res.Header.Get("Content-Length"))
	if err != nil {
		t.Fatalf("Unexpected error parsing Content-Length: %v", err)
	}
	assert.Len(t, body, l)
	assert.Equal(t, "gzip", res.Header.Get("Content-Encoding"))
	assert.NotEqual(t, b, body)
}

func TestGzipHandlerMinSize(t *testing.T) {
	handler := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, _ := ioutil.ReadAll(r.Body)
		w.Write(resp)
		// Call write multiple times to pass through "chosenWriter"
		w.Write(resp)
		w.Write(resp)
	}), MinSize(13))

	// Run a test with size smaller than the limit
	b := bytes.NewBufferString("test")

	req1 := httptest.NewRequest(http.MethodGet, "/whatever", b)
	req1.Header.Add("Accept-Encoding", "gzip")
	resp1 := httptest.NewRecorder()
	handler.ServeHTTP(resp1, req1)
	res1 := resp1.Result()
	assert.Equal(t, "", res1.Header.Get("Content-Encoding"))

	// Run a test with size bigger than the limit
	b = bytes.NewBufferString(smallTestBody)

	req2 := httptest.NewRequest(http.MethodGet, "/whatever", b)
	req2.Header.Add("Accept-Encoding", "gzip")
	resp2 := httptest.NewRecorder()
	handler.ServeHTTP(resp2, req2)
	res2 := resp2.Result()
	assert.Equal(t, "gzip", res2.Header.Get("Content-Encoding"))
}

func TestMinSizePanicsForInvalid(t *testing.T) {
	assert.Panics(t, func() {
		MinSize(-10)
	}, "MinSize did not panic on negative size")
}

func TestGzipDoubleClose(t *testing.T) {
	h := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// call close here and it'll get called again interally by
		// NewGzipLevelHandler's handler defer
		io.WriteString(w, "test")
		w.(io.Closer).Close()
	}), MinSize(0))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	// the second close shouldn't have added the same writer
	// so we pull out 2 writers from the pool and make sure they're different
	w1 := h.(*handler).pool.Get()
	w2 := h.(*handler).pool.Get()
	// assert.NotEqual looks at the value and not the address, so we use regular ==
	assert.False(t, w1 == w2)
}

func TestStatusCodes(t *testing.T) {
	handler := Gzip(http.NotFoundHandler())
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	result := w.Result()
	assert.Equal(t, http.StatusNotFound, result.StatusCode)

	handler = Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	result = w.Result()
	assert.Equal(t, http.StatusNotFound, result.StatusCode)

	handler = Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	result = w.Result()
	assert.Equal(t, http.StatusOK, result.StatusCode)
}

func TestFlushBeforeWrite(t *testing.T) {
	b := []byte(testBody)
	handler := Gzip(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusNotFound)
		rw.(http.Flusher).Flush()
		rw.Write(b)
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	res := w.Result()
	assert.Equal(t, http.StatusNotFound, res.StatusCode)
	assert.Equal(t, "gzip", res.Header.Get("Content-Encoding"))
	assert.NotEqual(t, b, w.Body.Bytes())
}

func TestInferContentType(t *testing.T) {
	handler := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<!doc")
		io.WriteString(w, "type html>")
	}), MinSize(len("<!doctype html")))

	req1 := httptest.NewRequest(http.MethodGet, "/whatever", nil)
	req1.Header.Add("Accept-Encoding", "gzip")
	resp1 := httptest.NewRecorder()
	handler.ServeHTTP(resp1, req1)

	res1 := resp1.Result()
	assert.Equal(t, "text/html; charset=utf-8", res1.Header.Get("Content-Type"))
}

func TestInferContentTypeUncompressed(t *testing.T) {
	handler := newTestHandler("<!doctype html>")

	req1 := httptest.NewRequest(http.MethodGet, "/whatever", nil)
	req1.Header.Add("Accept-Encoding", "gzip")
	resp1 := httptest.NewRecorder()
	handler.ServeHTTP(resp1, req1)

	res1 := resp1.Result()
	assert.Equal(t, "text/html; charset=utf-8", res1.Header.Get("Content-Type"))
}

func TestResponseWriterTypes(t *testing.T) {
	var cok, hok, pok bool

	handler := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, cok = w.(http.CloseNotifier)
		_, hok = w.(http.Hijacker)
		_, pok = w.(http.Pusher)
	}))

	req1 := httptest.NewRequest(http.MethodGet, "/whatever", nil)
	req1.Header.Add("Accept-Encoding", "gzip")

	resp1 := httptest.NewRecorder()

	handler.ServeHTTP(resp1, req1)
	assert.True(t, !cok && !hok && !pok, "expected plain ResponseWriter")

	handler.ServeHTTP(struct {
		http.ResponseWriter
		http.CloseNotifier
	}{resp1, nil}, req1)
	assert.True(t, cok && !hok && !pok, "expected CloseNotifier")

	handler.ServeHTTP(struct {
		http.ResponseWriter
		http.Hijacker
	}{resp1, nil}, req1)
	assert.True(t, !cok && hok && !pok, "expected Hijacker")

	handler.ServeHTTP(struct {
		http.ResponseWriter
		http.Pusher
	}{resp1, nil}, req1)
	assert.True(t, !cok && !hok && pok, "expected Pusher")

	handler.ServeHTTP(struct {
		http.ResponseWriter
		http.CloseNotifier
		http.Hijacker
	}{resp1, nil, nil}, req1)
	assert.True(t, cok && hok && !pok, "expected CloseNotifier and Hijacker")

	handler.ServeHTTP(struct {
		http.ResponseWriter
		http.CloseNotifier
		http.Pusher
	}{resp1, nil, nil}, req1)
	assert.True(t, cok && !hok && pok, "expected CloseNotifier and Pusher")
}

func TestContentTypes(t *testing.T) {
	for _, tt := range []struct {
		name                 string
		contentType          string
		sniffContentType     bool
		acceptedContentTypes []string
		expectedGzip         bool
	}{
		{
			name:                 "Always gzip when content types are empty",
			contentType:          "",
			acceptedContentTypes: []string{},
			expectedGzip:         true,
		},
		{
			name:                 "Exact content-type match",
			contentType:          "application/json",
			acceptedContentTypes: []string{"application/json"},
			expectedGzip:         true,
		},
		{
			name:                 "Non-matching content-type",
			contentType:          "text/xml",
			acceptedContentTypes: []string{"application/json"},
			expectedGzip:         false,
		},
		{
			name:                 "No-subtype content-type match",
			contentType:          "application/json",
			acceptedContentTypes: []string{"application/*"},
			expectedGzip:         true,
		},
		{
			name:                 "content-type with directive match",
			contentType:          "application/json; charset=utf-8",
			acceptedContentTypes: []string{"application/json"},
			expectedGzip:         true,
		},

		{
			name:                 "Always gzip when content types are empty, sniffed",
			sniffContentType:     true,
			acceptedContentTypes: []string{},
			expectedGzip:         true,
		},
		{
			name:                 "Exact content-type match, sniffed",
			sniffContentType:     true,
			acceptedContentTypes: []string{"text/plain"},
			expectedGzip:         true,
		},
		{
			name:                 "Non-matching content-type, sniffed",
			sniffContentType:     true,
			acceptedContentTypes: []string{"application/json"},
			expectedGzip:         false,
		},
		{
			name:                 "No-subtype content-type match, sniffed",
			sniffContentType:     true,
			acceptedContentTypes: []string{"text/*"},
			expectedGzip:         true,
		},
	} {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !tt.sniffContentType {
				w.Header().Set("Content-Type", tt.contentType)
			}

			w.WriteHeader(http.StatusTeapot)
			io.WriteString(w, testBody)
		})

		req := httptest.NewRequest(http.MethodGet, "/whatever", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		resp := httptest.NewRecorder()
		Gzip(handler, ContentTypes(tt.acceptedContentTypes)).ServeHTTP(resp, req)

		res := resp.Result()
		assert.Equal(t, http.StatusTeapot, res.StatusCode)

		if ce := res.Header.Get("Content-Encoding"); tt.expectedGzip {
			assert.Equal(t, "gzip", ce, tt.name)
		} else {
			assert.NotEqual(t, "gzip", ce, tt.name)
		}
	}
}

func TestContentTypesMultiWrite(t *testing.T) {
	handler := Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "example/mismatch")
		io.WriteString(w, testBody)

		w.Header().Set("Content-Type", "example/match")
		io.WriteString(w, testBody)
	}), ContentTypes([]string{"example/match"}))

	req := httptest.NewRequest(http.MethodGet, "/whatever", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	res := resp.Result()
	assert.Equal(t, http.StatusOK, res.StatusCode)
	assert.NotEqual(t, "gzip", res.Header.Get("Content-Encoding"))
	assert.Equal(t, testBody+testBody, resp.Body.String())
}

// --------------------------------------------------------------------

func BenchmarkGzipHandler_S2k(b *testing.B)   { benchmark(b, false, 2048) }
func BenchmarkGzipHandler_S20k(b *testing.B)  { benchmark(b, false, 20480) }
func BenchmarkGzipHandler_S100k(b *testing.B) { benchmark(b, false, 102400) }
func BenchmarkGzipHandler_P2k(b *testing.B)   { benchmark(b, true, 2048) }
func BenchmarkGzipHandler_P20k(b *testing.B)  { benchmark(b, true, 20480) }
func BenchmarkGzipHandler_P100k(b *testing.B) { benchmark(b, true, 102400) }

// --------------------------------------------------------------------

func gzipStrLevel(s string, lvl int) []byte {
	var b bytes.Buffer
	w, _ := gzip.NewWriterLevel(&b, lvl)
	io.WriteString(w, s)
	w.Close()
	return b.Bytes()
}

func benchmark(b *testing.B, parallel bool, size int) {
	bin, err := ioutil.ReadFile("testdata/benchmark.json")
	if err != nil {
		b.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/whatever", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	handler := newTestHandler(string(bin[:size]))

	if parallel {
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				runBenchmark(b, req, handler)
			}
		})
	} else {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			runBenchmark(b, req, handler)
		}
	}
}

func runBenchmark(b *testing.B, req *http.Request, handler http.Handler) {
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if code := res.Code; code != http.StatusOK {
		b.Fatalf("Expected 200 but got %d", code)
	} else if blen := res.Body.Len(); blen < 500 {
		b.Fatalf("Expected complete response body, but got %d bytes", blen)
	}
}

func newTestHandler(body string) http.Handler {
	return Gzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
}
