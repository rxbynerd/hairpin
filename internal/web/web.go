// Package web serves hairpin's operator UI: a job list with a submit
// form, a per-job detail page with permission approve/deny controls, and
// a Server-Sent Events live feed. It is dependency-free (standard
// library only) and self-contained, so it can run air-gapped.
package web

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"

	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/service"
	"github.com/rxbynerd/hairpin/internal/store"
)

// SubmitParams carries a job submission from the web form.
type SubmitParams = service.SubmitParams

// Service is the subset of hairpin's core operations the web UI needs,
// satisfied by *service.Service and by test fakes.
type Service interface {
	Submit(ctx context.Context, p SubmitParams) (*job.Job, error)
	Get(ctx context.Context, id string) (*job.Job, error)
	List(ctx context.Context, limit int, pageToken string) ([]*job.Job, string, error)
	Cancel(ctx context.Context, id string) (*job.Job, error)
	ListPermissions(ctx context.Context, jobID string) ([]store.PermissionRequest, error)
	AnswerPermission(ctx context.Context, jobID, requestID string, allow bool, reason string) (store.PermissionRequest, error)
	Watch(ctx context.Context, id, afterID string) (<-chan store.Event, error)
}

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*.css static/*.js
var staticFS embed.FS

// jobIDPattern matches hairpin job IDs: "hp-" plus a lowercase ULID.
var jobIDPattern = regexp.MustCompile(`^hp-[0-9a-z]{26}$`)

func validJobID(id string) bool { return jobIDPattern.MatchString(id) }

type handler struct {
	svc        Service
	log        *slog.Logger
	tmplIndex  *template.Template
	tmplDetail *template.Template
	tmplError  *template.Template
}

// New returns an http.Handler serving the web UI, rooted at "/".
func New(svc Service, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	h := &handler{
		svc:        svc,
		log:        logger,
		tmplIndex:  parsePage("index.html"),
		tmplDetail: parsePage("detail.html"),
		tmplError:  parsePage("error.html"),
	}

	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // embedded FS; only fails if the embed directive is wrong
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.index)
	mux.HandleFunc("POST /jobs", h.submit)
	mux.HandleFunc("GET /jobs/{id}", h.detail)
	mux.HandleFunc("GET /jobs/{id}/events", h.events)
	mux.HandleFunc("POST /jobs/{id}/cancel", h.cancel)
	mux.HandleFunc("POST /jobs/{id}/permissions/{requestID}", h.answerPermission)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticSub)))
	return protectUI(&notFoundInterceptor{next: mux, h: h})
}

// protectUI adds browser-side hardening: standard response headers, and
// a same-origin check on state-changing requests. The UI carries no
// auth of its own (see docs/design.md), so when a deployment fronts it
// with cookie-based SSO, a cross-site form post would otherwise ride
// that cookie straight into submit/cancel/approve.
func protectUI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("X-Frame-Options", "DENY")
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("Content-Security-Policy", "default-src 'self'")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" {
				if u, err := url.Parse(origin); err != nil || u.Host != r.Host {
					http.Error(w, "cross-origin request rejected", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func parsePage(name string) *template.Template {
	return template.Must(template.New("layout.html").Funcs(funcMap).ParseFS(templatesFS, "templates/layout.html", "templates/"+name))
}
