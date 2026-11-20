package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/omidalighadr/media-job-api/internal/jobs"
)

func testAPI(t *testing.T, token string) http.Handler {
	t.Helper()
	s, e := jobs.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return New(s, jobs.NewPool(s, nil, time.Second), token, 16, 1)
}
func request(h http.Handler, method, path, body, key, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	r.Header.Set("Content-Type", "application/octet-stream")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestUploadReplayConflictAndLimit(t *testing.T) {
	h := testAPI(t, "")
	w := request(h, "POST", "/v1/jobs", "audio", "key", "")
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	var j jobs.Job
	json.Unmarshal(w.Body.Bytes(), &j)
	if j.ID == "" || w.Header().Get("Location") != "/v1/jobs/"+j.ID {
		t.Fatal(w)
	}
	for _, c := range []struct {
		body, key string
		status    int
	}{{"audio", "key", 200}, {"different", "key", 409}, {"audio", "other", 503}, {"01234567890123456", "", 413}, {"", "", 400}} {
		w = request(h, "POST", "/v1/jobs", c.body, c.key, "")
		if w.Code != c.status {
			t.Fatalf("%+v: %d %s", c, w.Code, w.Body)
		}
	}
	w = request(h, "GET", "/v1/jobs/"+j.ID, "", "", "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = request(h, "DELETE", "/v1/jobs/"+j.ID, "", "", "")
	if w.Code != 409 {
		t.Fatal(w.Code)
	}
	w = request(h, "POST", "/v1/jobs/"+j.ID+"/cancel", "", "", "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = request(h, "DELETE", "/v1/jobs/"+j.ID, "", "", "")
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
	w = request(h, "GET", "/v1/jobs/"+j.ID, "", "", "")
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
}
func TestAuthorizationAndHealth(t *testing.T) {
	h := testAPI(t, "secret")
	for _, c := range []struct {
		path, token string
		status      int
	}{{"/healthz", "", 200}, {"/readyz", "", 200}, {"/v1/jobs/nope", "", 401}, {"/v1/jobs/nope", "wrong", 401}, {"/v1/jobs/nope", "secret", 404}} {
		w := request(h, "GET", c.path, "", "", c.token)
		if w.Code != c.status {
			t.Fatalf("%+v: %d", c, w.Code)
		}
	}
}
func TestRejectContentTypeAndKey(t *testing.T) {
	h := testAPI(t, "")
	r := httptest.NewRequest("POST", "/v1/jobs", bytes.NewBufferString("audio"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 415 {
		t.Fatal(w.Code)
	}
	w = request(h, "POST", "/v1/jobs", "audio", "has space", "")
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
}
