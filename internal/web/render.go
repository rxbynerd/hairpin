package web

import (
	"html/template"
	"net/http"
)

func (h *handler) render(w http.ResponseWriter, tmpl *template.Template, status int, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		h.log.Error("render template", "error", err)
	}
}

func (h *handler) renderError(w http.ResponseWriter, r *http.Request, status int, message string) {
	h.render(w, h.tmplError, status, errorData{
		Title:   "Error",
		Status:  status,
		Message: message,
	})
}
