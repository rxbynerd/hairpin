package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/rxbynerd/hairpin/internal/store"
)

const jobListLimit = 50

func (h *handler) index(w http.ResponseWriter, r *http.Request) {
	h.renderIndex(w, r, http.StatusOK, formValues{}, "")
}

func (h *handler) renderIndex(w http.ResponseWriter, r *http.Request, status int, form formValues, errMsg string) {
	jobs, _, err := h.svc.List(r.Context(), jobListLimit, "")
	if err != nil {
		h.log.Error("list jobs", "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "failed to list jobs")
		return
	}
	rows := make([]jobRow, 0, len(jobs))
	for _, j := range jobs {
		rows = append(rows, newJobRow(j))
	}
	h.render(w, h.tmplIndex, status, indexData{
		Title: "Jobs",
		Jobs:  rows,
		Form:  form,
		Error: errMsg,
	})
}

func (h *handler) submit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.renderIndex(w, r, http.StatusBadRequest, formValues{}, "invalid form submission")
		return
	}
	form := formValues{
		Prompt:        r.PostFormValue("prompt"),
		Profile:       r.PostFormValue("profile"),
		RunConfigJSON: r.PostFormValue("run_config_json"),
	}
	if strings.TrimSpace(form.Prompt) == "" && strings.TrimSpace(form.RunConfigJSON) == "" {
		h.renderIndex(w, r, http.StatusBadRequest, form, "prompt or run config JSON is required")
		return
	}

	j, err := h.svc.Submit(r.Context(), SubmitParams{
		Prompt:        form.Prompt,
		Profile:       form.Profile,
		RunConfigJSON: form.RunConfigJSON,
	})
	if err != nil {
		h.renderIndex(w, r, http.StatusBadRequest, form, err.Error())
		return
	}
	http.Redirect(w, r, "/jobs/"+j.ID, http.StatusSeeOther)
}

func (h *handler) detail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validJobID(id) {
		h.renderError(w, r, http.StatusBadRequest, "invalid job id")
		return
	}
	j, err := h.svc.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, "job not found")
			return
		}
		h.log.Error("get job", "id", id, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "failed to load job")
		return
	}
	perms, err := h.svc.ListPermissions(r.Context(), id)
	if err != nil {
		h.log.Error("list permissions", "id", id, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "failed to load permission requests")
		return
	}
	h.render(w, h.tmplDetail, http.StatusOK, detailData{
		Title:       j.ID,
		Job:         newJobDetail(j),
		Permissions: newPermissionRows(perms),
	})
}

func (h *handler) cancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validJobID(id) {
		h.renderError(w, r, http.StatusBadRequest, "invalid job id")
		return
	}
	if _, err := h.svc.Cancel(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, "job not found")
			return
		}
		h.log.Error("cancel job", "id", id, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "failed to cancel job")
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

func (h *handler) answerPermission(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	requestID := r.PathValue("requestID")
	if !validJobID(id) || requestID == "" {
		h.renderError(w, r, http.StatusBadRequest, "invalid permission request")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderError(w, r, http.StatusBadRequest, "invalid form submission")
		return
	}
	allow := r.PostFormValue("allow") == "true"
	reason := r.PostFormValue("reason")
	if _, err := h.svc.AnswerPermission(r.Context(), id, requestID, allow, reason); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, "permission request not found")
			return
		}
		h.log.Error("answer permission", "id", id, "request_id", requestID, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "failed to answer permission request")
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}
