package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rxbynerd/hairpin/internal/store"
)

// heartbeatInterval keeps intermediate proxies from closing an idle SSE
// connection.
const heartbeatInterval = 15 * time.Second

// events streams a job's timeline as Server-Sent Events: recorded
// history first, then live events, until the job reaches a terminal
// status (signalled by the watch channel closing) or the client
// disconnects. Resumes from the Last-Event-ID request header, which
// browsers set automatically on reconnect.
func (h *handler) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validJobID(id) {
		h.renderError(w, r, http.StatusBadRequest, "invalid job id")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.renderError(w, r, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	afterID := r.Header.Get("Last-Event-ID")
	if afterID == "" {
		afterID = r.URL.Query().Get("after")
	}

	ctx := r.Context()
	ch, err := h.svc.Watch(ctx, id, afterID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.renderError(w, r, http.StatusNotFound, "job not found")
			return
		}
		h.log.Error("watch job", "id", id, "error", err)
		h.renderError(w, r, http.StatusInternalServerError, "failed to watch job")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				fmt.Fprint(w, "event: eof\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			writeSSEEvent(w, ev)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		case <-ctx.Done():
			return
		}
	}
}

// writeSSEEvent writes one store.Event in SSE wire format. Multi-line
// payloads are split across repeated "data:" lines per the SSE spec.
func writeSSEEvent(w http.ResponseWriter, ev store.Event) {
	fmt.Fprintf(w, "id: %s\n", ev.ID)
	fmt.Fprintf(w, "event: %s\n", ev.Type)
	payload := ev.PayloadJSON
	if payload == "" {
		payload = "{}"
	}
	for _, line := range strings.Split(payload, "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
}
