package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/mailquay/mailquay/internal/httpapi/oapi"
)

func (s *Server) StreamEvents(w http.ResponseWriter, r *http.Request, params oapi.StreamEventsParams) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.error(w, r, http.StatusInternalServerError, "stream_unsupported", "Streaming is not supported.")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	writeEvent := func(id uint64, typ string, data []byte) bool {
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", id, typ, data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	user, _ := s.user(r.Context())
	hello, _ := json.Marshal(map[string]any{"userId": user.ID.String(), "cursor": s.hub.LatestID()})
	if !writeEvent(s.hub.LatestID(), "hello", hello) {
		return
	}
	var lastID uint64
	rawLastID := ""
	if params.LastEventID != nil {
		rawLastID = *params.LastEventID
	} else if params.LastEventId != nil {
		rawLastID = *params.LastEventId
	}
	if rawLastID != "" {
		if parsed, err := strconv.ParseUint(rawLastID, 10, 64); err == nil {
			lastID = parsed
		}
	}
	if lastID > 0 {
		for _, event := range s.hub.Since(lastID) {
			data, err := json.Marshal(event.Data)
			if err != nil {
				continue
			}
			if !writeEvent(event.ID, event.Type, data) {
				return
			}
			lastID = event.ID
		}
	}
	ch, cancel := s.hub.Subscribe(r.Context())
	defer cancel()
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case event, ok := <-ch:
			if !ok {
				return
			}
			if event.ID <= lastID {
				continue
			}
			data, err := json.Marshal(event.Data)
			if err != nil {
				continue
			}
			if !writeEvent(event.ID, event.Type, data) {
				return
			}
			lastID = event.ID
		}
	}
}
