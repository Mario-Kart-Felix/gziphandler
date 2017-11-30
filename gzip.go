package gziphandler

import (
	"bufio"
	"compress/gzip"
	"net"
	"net/http"
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
func (w *responseWriter) startGzip() (err error) {
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

	if len(b) == 0 {
		return
	}

	// It infer it from the uncompressed body.
	h["Content-Type"] = []string{http.DetectContentType(b)}
}

// Close will close the gzip.Writer and will put it back in
// the gzipWriterPool.
func (w *responseWriter) Close() error {
	switch {
	case w.buf != nil && w.gw != nil:
		panic("both buf and gw are non nil in call to Close")
	// Buffer not nil means the regular response must
	// be returned.
	case w.buf != nil:
		return w.closeNonGzipped()
	// If the GZIP responseWriter is not set no need
	// to close it.
	case w.gw != nil:
		return w.closeGzipped()
	default:
		return nil
	}
}

func (w *responseWriter) closeGzipped() error {
	err := w.gw.Close()

	w.h.pool.Put(w.gw)
	w.gw = nil

	return err
}

func (w *responseWriter) closeNonGzipped() (err error) {
	w.inferContentType(nil)

	if w.code == 0 {
		w.code = http.StatusOK
	}

	w.ResponseWriter.WriteHeader(w.code)

	// Make the write into the regular response.
	buf := *w.buf
	if len(buf) != 0 {
		_, err = w.ResponseWriter.Write(buf)
	}

	*w.buf = buf[:0]
	bufferPool.Put(w.buf)
	w.buf = nil

	return err
}

// Flush flushes the underlying *gzip.Writer and then the
// underlying http.ResponseWriter if it is an http.Flusher.
// This makes GzipResponseWriter an http.Flusher.
func (w *responseWriter) Flush() {
	if w.gw == nil {
		// Fix for NYTimes/gziphandler#58:
		//  Only flush once startGzip has been
		//  called.
		//
		// Flush is thus a no-op until the written
		// body exceeds minSize.
		return
	}

	w.gw.Flush()

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
		if spec.Value == "gzip" && spec.Q > 0 {
			acceptsGzip = true
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
	defer func() {
		if err := gw.Close(); err != nil {
			srv, _ := r.Context().Value(http.ServerContextKey).(*http.Server)
			if srv != nil && srv.ErrorLog != nil {
				srv.ErrorLog.Printf("gziphandler: %v", err)
			}
		}
	}()

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
// the Accept-Encoding header).
func Gzip(h http.Handler, opts ...Option) http.Handler {
	c := config{
		level:   DefaultCompression,
		minSize: defaultMinSize,
	}

	for _, opt := range opts {
		opt(&c)
	}

	level := c.level
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

		minSize: c.minSize,
	}
}

type config struct {
	level   int
	minSize int
}

// Option customizes the behaviour of the gzip handler.
type Option func(c *config)

// CompressionLevel is the gzip compression level to apply.
// See the level constants defined in this package.
//
// The default value adds gzip framing but performs no
// compression.
func CompressionLevel(level int) Option {
	if level < gzip.HuffmanOnly || level > gzip.BestCompression {
		panic("gziphandler: invalid compression level requested")
	}

	return func(c *config) {
		c.level = level
	}
}

// MinSize specifies the minimum size of a response before
// it will be compressed. Responses smaller than this value
// will not be compressed.
//
// If size is zero, all responses will be compressed.
//
// The default minimum size is 512 bytes.
func MinSize(size int) Option {
	if size < 0 {
		panic("gziphandler: minimum size must not be negative")
	}

	return func(c *config) {
		c.minSize = size
	}
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
