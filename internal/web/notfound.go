package web

import "net/http"

// notFoundInterceptor lets ServeMux's built-in routing produce the
// automatic 404 (unmatched path) and 405 (path matched, method didn't)
// responses, then swaps the default plain-text body for the styled
// error page.
type notFoundInterceptor struct {
	next http.Handler
	h    *handler
}

func (n *notFoundInterceptor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rw := &errorRewriter{ResponseWriter: w, h: n.h, r: r}
	n.next.ServeHTTP(rw, r)
}

// errorRewriter intercepts WriteHeader calls for 404 and 405 (as issued
// by net/http's default NotFound and MethodNotAllowed handling inside
// ServeMux) and replaces the response with the shared error page.
type errorRewriter struct {
	http.ResponseWriter
	h       *handler
	r       *http.Request
	handled bool
}

func (e *errorRewriter) WriteHeader(status int) {
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		e.handled = true
		msg := "page not found"
		if status == http.StatusMethodNotAllowed {
			msg = "method not allowed"
		}
		e.h.renderError(e.ResponseWriter, e.r, status, msg)
		return
	}
	e.ResponseWriter.WriteHeader(status)
}

func (e *errorRewriter) Write(b []byte) (int, error) {
	if e.handled {
		return len(b), nil
	}
	return e.ResponseWriter.Write(b)
}

// Flush satisfies http.Flusher so the SSE handler's type assertion
// succeeds through this wrapper.
func (e *errorRewriter) Flush() {
	if f, ok := e.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
