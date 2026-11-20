package main

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
)

func managementRoutes(mux *http.ServeMux, m *manager, authed func(http.ResponseWriter, *http.Request) bool) {
	mux.HandleFunc("/api/management", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if m == nil {
			json.NewEncoder(w).Encode(map[string]bool{"enabled": false})
			return
		}
		b, err := m.snapshot(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write(b)
	})
	for _, action := range []string{"preview", "apply", "reconcile"} {
		mux.HandleFunc("/api/management/"+action, func(w http.ResponseWriter, r *http.Request) {
			if !authed(w, r) {
				return
			}
			if m == nil {
				http.Error(w, "service management is not enabled", http.StatusConflict)
				return
			}
			media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || media != "application/json" {
				http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)
				return
			}
			b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
			if err != nil {
				http.Error(w, "invalid/oversized request", http.StatusBadRequest)
				return
			}
			var result any
			switch action {
			case "preview":
				var plan WorkPlan
				plan, err = decodeWorkPlan(b)
				if err == nil {
					result, err = m.preview(r.Context(), plan)
				}
			case "apply":
				var request struct {
					Digest string `json:"digest"`
				}
				if err = decodeStrict(b, &request); err == nil {
					err = m.apply(r.Context(), request.Digest)
				}
			case "reconcile":
				var request struct{}
				if err = decodeStrict(b, &request); err == nil {
					err = m.resume(r.Context())
					if err == nil {
						err = m.reconcile(r.Context(), 0, true)
					}
				}
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if result != nil {
				json.NewEncoder(w).Encode(result)
				return
			}
			state, err := m.snapshot(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Write(state)
		})
	}
}
