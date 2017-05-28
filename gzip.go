package gziphandler

import (
	"bufio"
	"compress/gzip"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/golang/gddo/httputil/header"
)

const defaultMinSize = 512

var bufferPool = &sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 0, defaultMinSize)
		return &buf
	},
}

// These constants are copied from the gzip package, so
// that code that imports "github.com/tmthrgd/gziphandler"
// does not also have to import "compress/gzip".
const (
	NoCompression      = gzip.NoCompression
	BestSpeed          = gzip.BestSpeed
	BestCompression    = gzip.BestCompression
	DefaultCompression = gzip.DefaultCompression
	HuffmanOnly        = gzip.HuffmanOnly
)

// responseWriter provides an http.ResponseWriter interface,
// which gzips bytes before writing them to the underlying
// response. This doesn't close the writers, so don't forget
// to do that. It can be configured to skip response smaller
// than minSize.
type responseWriter struct {
	http.ResponseWriter

	h *handler

	gw *gzip.Writer

	// Saves the WriteHeader value.
	code int

	// Holds the first part of the write before reaching
	// the minSize or the end of the write.
	buf *[]byte
}

// WriteHeader just saves the response code until close or
// GZIP effective writes.
func (w *responseWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}

// Write appends data to the gzip writer.
func (w *responseWriter) Write(b []byte) (int, error) {
	// GZIP responseWriter is initialized. Use the GZIP
	// responseWriter.
	if w.gw != nil {
		return w.gw.Write(b)
	}

	if w.code == 0 {
		w.code = http.StatusOK
	}

	// If the global writes are bigger than the minSize,
	// compression is enable.
	if buf := *w.buf; len(buf)+len(b) < w.h.minSize {
		// Save the write into a buffer for later
		// use in GZIP responseWriter (if content
		// is long enough) or at close with regular
		// responseWriter.
		*w.buf = append(buf, b...)
		return len(b), nil
	}

	w.inferContentType(b)

	if err := w.startGzip(); err != nil {
		return 0, err
	}

	return w.gw.Write(b)
}

// startGzip initialize any GZIP specific informations.
func (w *responseWriter) startGzip() error {
	h := w.Header()

	// Set the GZIP header.
	h["Content-Encoding"] = []string{"gzip"}

	// if the Content-Length is already set, then calls
	// to Write on gzip will fail to set the
	// Content-Length header since its already set
	// See: https://github.com/golang/go/issues/14975.
	delete(h, "Content-Length")

	// Write the header to gzip response.
	w.ResponseWriter.WriteHeader(w.code)

	// Bytes written during ServeHTTP are redirected to
	// this gzip writer before being written to the
	// underlying response.
	w.gw = w.h.pool.Get().(*gzip.Writer)
	w.gw.Reset(w.ResponseWriter)

	buf := *w.buf

	var err error
	if len(buf) != 0 {
		// Flush the buffer into the gzip response.
		_, err = w.gw.Write(buf)
	}

	// Empty the buffer.
	*w.buf = buf[:0]
	bufferPool.Put(w.buf)
	w.buf = nil

	return err
}

func (w *responseWriter) inferContentType(b []byte) {
	h := w.Header()

	// If content type is not set.
	if _, ok := h["Content-Type"]; ok {
		return
	}

	if buf := *w.buf; len(buf) != 0 {
		const sniffLen = 512
		if len(buf) >= sniffLen {
			b = buf
		} else if len(buf)+len(b) > sniffLen {
			b = append(buf, b[:sniffLen-len(buf)]...)
		} else {
			b = append(buf, b...)
		}
	}

	// It infer it from the uncompressed body.
	h["Content-Type"] = []string{http.DetectContentType(b)}
}

// Close will close the gzip.Writer and will put it back in
// the gzipWriterPool.
func (w *responseWriter) Close() error {
	// Buffer not nil means the regular response must
	// be returned.
	if w.buf != nil {
		buf := *w.buf

		if len(buf) != 0 {
			w.inferContentType(nil)
		}

		if w.code == 0 {
			w.code = http.StatusOK
		}

		w.ResponseWriter.WriteHeader(w.code)

		// Make the write into the regular response.
		_, err := w.ResponseWriter.Write(buf)

		*w.buf = buf[:0]
		bufferPool.Put(w.buf)
		w.buf = nil

		if err != nil {
			return err
		}
	}

	// If the GZIP responseWriter is not set no needs
	// to close it.
	if w.gw == nil {
		return nil
	}

	err := w.gw.Close()

	w.h.pool.Put(w.gw)
	w.gw = nil

	return err
}

// Flush flushes the underlying *gzip.Writer and then the
// underlying http.ResponseWriter if it is an http.Flusher.
// This makes GzipResponseWriter an http.Flusher.
func (w *responseWriter) Flush() {
	if w.gw != nil {
		w.gw.Flush()
	}

	if fw, ok := w.ResponseWriter.(http.Flusher); ok {
		fw.Flush()
	}
}

type handler struct {
	http.Handler

	pool *sync.Pool

	minSize int
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hdr := w.Header()
	hdr["Vary"] = append(hdr["Vary"], "Accept-Encoding")

	var acceptsGzip bool
	for _, spec := range header.ParseAccept(r.Header, "Accept-Encoding") {
		if len(spec.Value) != len("gzip") {
			continue
		}

		if spec.Value == "gzip" || strings.ToLower(spec.Value) == "gzip" {
			acceptsGzip = spec.Q > 0
			break
		}
	}

	if !acceptsGzip {
		h.Handler.ServeHTTP(w, r)
		return
	}

	gw := &responseWriter{
		ResponseWriter: w,

		h: h,

		buf: bufferPool.Get().(*[]byte),
	}
	defer gw.Close()

	var rw http.ResponseWriter = gw

	_, cok := w.(http.CloseNotifier)
	_, hok := w.(http.Hijacker)
	_, pok := w.(http.Pusher)

	switch {
	case cok && hok:
		rw = closeNotifyHijackResponseWriter{gw}
	case cok && pok:
		rw = closeNotifyPusherResponseWriter{gw}
	case cok:
		rw = closeNotifyResponseWriter{gw}
	case hok:
		rw = hijackResponseWriter{gw}
	case pok:
		rw = pusherResponseWriter{gw}
	}

	h.Handler.ServeHTTP(rw, r)
}

// Gzip wraps an HTTP handler, to transparently gzip the
// response body if the client supports it (via the
// Accept-Encoding header). This will compress at the
// default compression level. The resource will not be
// compressed unless it exceeds 512 bytes.
func Gzip(h http.Handler) http.Handler {
	return GzipWithLevel(h, gzip.DefaultCompression)
}

// GzipWithLevel wraps an HTTP handler, to transparently
// gzip the response body if the client supports it (via
// the Accept-Encoding header). This will compress at the
// given gzip compression level. The resource will not be
// compressed unless it exceeds 512 bytes.
func GzipWithLevel(h http.Handler, level int) http.Handler {
	return GzipWithLevelAndMinSize(h, level, defaultMinSize)
}

// GzipWithLevelAndMinSize wraps an HTTP handler, to
// transparently gzip the response body if the client
// supports it (via the Accept-Encoding header). This will
// compress at the given gzip compression level. The
// resource will not be compressed unless it is larger than
// minSize.
func GzipWithLevelAndMinSize(h http.Handler, level, minSize int) http.Handler {
	return GzipWithOptions(h, &Options{level, minSize})
}

// GzipWithOptions wraps an HTTP handler, to transparently
// gzip the response body if the client supports it (via
// the Accept-Encoding header). The provided Options struct
// allows the behaviour of this package to be customised.
func GzipWithOptions(h http.Handler, opts *Options) http.Handler {
	if opts == nil {
		panic("GzipWithOptions used with nil *Options argument")
	}

	if opts.Level < gzip.HuffmanOnly || opts.Level > gzip.BestCompression {
		panic("invalid compression level requested")
	}

	if opts.MinSize < 0 {
		panic("minimum size must be more than zero")
	}

	level := opts.Level
	return &handler{
		Handler: h,

		pool: &sync.Pool{
			New: func() interface{} {
				w, err := gzip.NewWriterLevel(nil, level)
				if err != nil {
					panic(err)
				}

				return w
			},
		},

		minSize: opts.MinSize,
	}
}

// Options is a struct that defines options to customise
// the behaviour of the gzip handler.
type Options struct {
	// Level is the gzip compression level to apply.
	// See the level constants defined in this package.
	//
	// The default value adds gzip framing but performs
	// no compression.
	Level int

	// MinSize specifies the minimum size of a response
	// before it will be compressed. Responses smaller
	// than this value will not be compressed.
	//
	// If MinSize is zero, all responses will be
	// compressed.
	MinSize int
}

type (
	// Each of these structs is intentionally small (1 pointer wide) so
	// as to fit inside an interface{} without causing an allocaction.
	closeNotifyResponseWriter       struct{ *responseWriter }
	hijackResponseWriter            struct{ *responseWriter }
	pusherResponseWriter            struct{ *responseWriter }
	closeNotifyHijackResponseWriter struct{ *responseWriter }
	closeNotifyPusherResponseWriter struct{ *responseWriter }
)

var (
	_ http.CloseNotifier = closeNotifyResponseWriter{}
	_ http.CloseNotifier = closeNotifyHijackResponseWriter{}
	_ http.CloseNotifier = closeNotifyPusherResponseWriter{}
	_ http.Hijacker      = hijackResponseWriter{}
	_ http.Hijacker      = closeNotifyHijackResponseWriter{}
	_ http.Pusher        = pusherResponseWriter{}
	_ http.Pusher        = closeNotifyPusherResponseWriter{}
)

func (w closeNotifyResponseWriter) CloseNotify() <-chan bool {
	return w.ResponseWriter.(http.CloseNotifier).CloseNotify()
}

func (w closeNotifyHijackResponseWriter) CloseNotify() <-chan bool {
	return w.ResponseWriter.(http.CloseNotifier).CloseNotify()
}

func (w closeNotifyPusherResponseWriter) CloseNotify() <-chan bool {
	return w.ResponseWriter.(http.CloseNotifier).CloseNotify()
}

func (w hijackResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.ResponseWriter.(http.Hijacker).Hijack()
}

func (w closeNotifyHijackResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.ResponseWriter.(http.Hijacker).Hijack()
}

func (w pusherResponseWriter) Push(target string, opts *http.PushOptions) error {
	return w.ResponseWriter.(http.Pusher).Push(target, opts)
}

func (w closeNotifyPusherResponseWriter) Push(target string, opts *http.PushOptions) error {
	return w.ResponseWriter.(http.Pusher).Push(target, opts)
}
