package web

import (
	"html/template"
	"time"
	"unicode/utf8"

	"github.com/rxbynerd/hairpin/internal/job"
	"github.com/rxbynerd/hairpin/internal/store"
)

const promptTruncateLen = 140

var funcMap = template.FuncMap{
	"statusClass": statusClass,
}

// statusClass maps a job status to a CSS badge modifier class.
func statusClass(status string) string {
	switch job.Status(status) {
	case job.StatusQueued, job.StatusLaunching, job.StatusAwaitingHarness:
		return "pending"
	case job.StatusRunning:
		return "running"
	case job.StatusSucceeded:
		return "succeeded"
	case job.StatusFailed:
		return "failed"
	case job.StatusCancelled:
		return "cancelled"
	default:
		return "pending"
	}
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func formatAge(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return time.Since(t).Round(time.Second).String() + " ago"
}

// indexData is the template data for GET / and the re-render on a
// failed POST /jobs.
type indexData struct {
	Title string
	Jobs  []jobRow
	Form  formValues
	Error string
}

// formValues carries submitted (or default) values for the submit form.
type formValues struct {
	Prompt        string
	Profile       string
	RunConfigJSON string
}

// jobRow is one row of the job list table.
type jobRow struct {
	ID         string
	Status     string
	Prompt     string
	CreatedAt  string
	FinishedAt string
	StopReason string
}

func newJobRow(j *job.Job) jobRow {
	return jobRow{
		ID:         j.ID,
		Status:     string(j.Status),
		Prompt:     truncate(j.Prompt, promptTruncateLen),
		CreatedAt:  formatTime(j.CreatedAt),
		FinishedAt: formatTime(j.FinishedAt),
		StopReason: j.StopReason,
	}
}

// detailData is the template data for GET /jobs/{id}.
type detailData struct {
	Title       string
	Job         jobDetail
	Permissions []permissionRow
}

// jobDetail is the full status card and content for one job.
type jobDetail struct {
	ID           string
	Status       string
	Terminal     bool
	Prompt       string
	Profile      string
	FinalText    string
	StopReason   string
	Error        string
	CreatedAt    string
	StartedAt    string
	FinishedAt   string
	LastEventAge string
}

func newJobDetail(j *job.Job) jobDetail {
	return jobDetail{
		ID:           j.ID,
		Status:       string(j.Status),
		Terminal:     j.Status.Terminal(),
		Prompt:       j.Prompt,
		Profile:      j.Profile,
		FinalText:    j.FinalText,
		StopReason:   j.StopReason,
		Error:        j.Error,
		CreatedAt:    formatTime(j.CreatedAt),
		StartedAt:    formatTime(j.StartedAt),
		FinishedAt:   formatTime(j.FinishedAt),
		LastEventAge: formatAge(j.LastEventAt),
	}
}

// permissionRow is one permission request in the detail page.
type permissionRow struct {
	RequestID   string
	ToolName    string
	InputJSON   string
	State       string
	Reason      string
	RequestedAt string
	AnsweredAt  string
}

func newPermissionRow(p store.PermissionRequest) permissionRow {
	return permissionRow{
		RequestID:   p.RequestID,
		ToolName:    p.ToolName,
		InputJSON:   p.InputJSON,
		State:       string(p.State),
		Reason:      p.Reason,
		RequestedAt: formatTime(p.RequestedAt),
		AnsweredAt:  formatTime(p.AnsweredAt),
	}
}

func newPermissionRows(perms []store.PermissionRequest) []permissionRow {
	rows := make([]permissionRow, 0, len(perms))
	for _, p := range perms {
		rows = append(rows, newPermissionRow(p))
	}
	return rows
}

// errorData is the template data for the shared error page.
type errorData struct {
	Title   string
	Status  int
	Message string
}
