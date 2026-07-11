package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateStackOrdering(t *testing.T) {
	var order []string

	tag := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}

	handler := CreateStack(tag("first"), tag("second"), tag("third"))(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			order = append(order, "handler")
		}),
	)

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"first", "second", "third", "handler"}

	if len(order) != len(want) {
		t.Fatalf("got call order %v, want %v", order, want)
	}

	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("got call order %v, want %v", order, want)
		}
	}
}

func TestCreateStackEmpty(t *testing.T) {
	handler := CreateStack()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusTeapot {
		t.Errorf("got status %d, want %d", rec.Code, http.StatusTeapot)
	}
}

func TestRequestLoggerPassesResponseThrough(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "ok", status: http.StatusOK, body: "ok\n"},
		{name: "created", status: http.StatusCreated, body: "made"},
		{name: "not found", status: http.StatusNotFound, body: "missing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := RequestLogger(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)

				if _, err := w.Write([]byte(tt.body)); err != nil {
					t.Errorf("write: %v", err)
				}
			}))

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

			if rec.Code != tt.status {
				t.Errorf("got status %d, want %d", rec.Code, tt.status)
			}

			if got := rec.Body.String(); got != tt.body {
				t.Errorf("got body %q, want %q", got, tt.body)
			}
		})
	}
}

// A handler that never calls WriteHeader still sends a 200, and the logged line
// has to report it as such rather than as a zero status.
func TestRequestLoggerDefaultsToOK(t *testing.T) {
	var buf bytes.Buffer

	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(old)

	handler := RequestLogger(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte("implicit")); err != nil {
			t.Errorf("write: %v", err)
		}
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	var line struct {
		Status int `json:"status"`
		Bytes  int `json:"bytes"`
	}

	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("parse log line %q: %v", buf.String(), err)
	}

	if line.Status != http.StatusOK {
		t.Errorf("got logged status %d, want %d", line.Status, http.StatusOK)
	}

	if line.Bytes != len("implicit") {
		t.Errorf("got %d bytes logged, want %d", line.Bytes, len("implicit"))
	}
}
