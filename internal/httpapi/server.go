// Package httpapi exposes a single-owner API. Uploads contain raw file bytes.
package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/omidalighadr/media-job-api/internal/jobs"
)

type API struct {
	Store      *jobs.Store
	Pool       *jobs.Pool
	Token      string
	MaxUpload  int64
	QueueLimit int
	uploads    chan struct{}
}

func New(s *jobs.Store, p *jobs.Pool, token string, maxUpload int64, queueLimit int) http.Handler {
	a := &API{Store: s, Pool: p, Token: token, MaxUpload: maxUpload, QueueLimit: queueLimit, uploads: make(chan struct{}, 2)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { write(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if s.Ping() != nil {
			problem(w, 503, "database_unavailable")
			return
		}
		write(w, 200, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("POST /v1/jobs", a.create)
	mux.HandleFunc("GET /v1/jobs/{id}", a.get)
	mux.HandleFunc("POST /v1/jobs/{id}/cancel", a.cancel)
	mux.HandleFunc("DELETE /v1/jobs/{id}", a.delete)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		// Health probes carry no user data. Every /v1 route shares the same token.
		if strings.HasPrefix(r.URL.Path, "/v1/") && token != "" {
			provided := sha256.Sum256([]byte(r.Header.Get("Authorization")))
			expected := sha256.Sum256([]byte("Bearer " + token))
			if subtle.ConstantTimeCompare(provided[:], expected[:]) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				problem(w, 401, "unauthorized")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func (a *API) create(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") != "application/octet-stream" {
		problem(w, 415, "use_application_octet_stream")
		return
	}
	if enc := r.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		problem(w, 415, "content_encoding_unsupported")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) > 128 || strings.IndexFunc(key, func(c rune) bool { return c < 33 || c > 126 }) >= 0 {
		problem(w, 400, "invalid_idempotency_key")
		return
	}
	select {
	case a.uploads <- struct{}{}:
		defer func() { <-a.uploads }()
	default:
		problem(w, 503, "upload_capacity_reached")
		return
	}
	body := http.MaxBytesReader(w, r.Body, a.MaxUpload)
	defer body.Close()
	input, err := io.ReadAll(body)
	if err != nil {
		var size *http.MaxBytesError
		if errors.As(err, &size) {
			problem(w, 413, "upload_too_large")
		} else {
			problem(w, 400, "upload_incomplete")
		}
		return
	}
	if len(input) == 0 {
		problem(w, 400, "empty_upload")
		return
	}
	idBytes := make([]byte, 16)
	if _, err = rand.Read(idBytes); err != nil {
		problem(w, 500, "id_generation_failed")
		return
	}
	id := hex.EncodeToString(idBytes)
	digest := sha256.Sum256(input)
	j, created, err := a.Store.Create(id, key, hex.EncodeToString(digest[:]), input, a.QueueLimit)
	if err != nil {
		switch {
		case errors.Is(err, jobs.ErrConflict):
			problem(w, 409, "idempotency_conflict")
		case errors.Is(err, jobs.ErrBusy):
			problem(w, 503, "queue_full")
		default:
			problem(w, 500, "storage_error")
		}
		return
	}
	w.Header().Set("Location", "/v1/jobs/"+j.ID)
	status := 200
	if created {
		status = 202
	}
	write(w, status, j)
}
func (a *API) get(w http.ResponseWriter, r *http.Request) {
	j, err := a.Store.Get(r.PathValue("id"))
	if err != nil {
		storeError(w, err)
		return
	}
	write(w, 200, j)
}
func (a *API) cancel(w http.ResponseWriter, r *http.Request) {
	j, err := a.Store.Cancel(r.PathValue("id"))
	if err != nil {
		storeError(w, err)
		return
	}
	a.Pool.Cancel(j.ID)
	write(w, 200, j)
}
func (a *API) delete(w http.ResponseWriter, r *http.Request) {
	if err := a.Store.Delete(r.PathValue("id")); err != nil {
		if errors.Is(err, jobs.ErrActive) {
			problem(w, 409, "cancel_job_first")
		} else {
			storeError(w, err)
		}
		return
	}
	w.WriteHeader(204)
}
func storeError(w http.ResponseWriter, err error) {
	if errors.Is(err, jobs.ErrNotFound) {
		problem(w, 404, "job_not_found")
	} else {
		problem(w, 500, "storage_error")
	}
}
func problem(w http.ResponseWriter, status int, code string) {
	write(w, status, map[string]string{"error": code})
}
func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
