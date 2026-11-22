package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testSchema = `{"columns":[{"name":"id","type":"integer","required":true}]}`

func request(h http.Handler, body, format, schema string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/validate?format="+format, strings.NewReader(body))
	r.Header.Set("X-Schema", schema)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestAPI(t *testing.T) {
	c := DefaultConfig()
	c.MaxBytes = 100
	c.MaxRecordBytes = 30
	h, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		body, format, schema string
		code                 int
	}{{"id\n1\nbad\n", "csv", testSchema, 200}, {"{}", "xml", testSchema, 400}, {"{}", "jsonl", "bad", 400}, {strings.Repeat("x", 31), "jsonl", testSchema, 422}, {strings.Repeat("{\"id\":1}\n", 20), "jsonl", testSchema, 413}} {
		w := request(h, tc.body, tc.format, tc.schema)
		if w.Code != tc.code {
			t.Fatalf("want %d got %d %s", tc.code, w.Code, w.Body.String())
		}
		var v any
		if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
			t.Fatal(e)
		}
	}
	for _, path := range []string{"/", "/app.js", "/style.css", "/healthz", "/metrics"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatal(path, w.Code)
		}
	}
}
func TestToken(t *testing.T) {
	c := DefaultConfig()
	c.Token = "secret"
	h, _ := New(c)
	if request(h, "id\n1", "csv", testSchema).Code != 401 {
		t.Fatal("missing auth")
	}
	r := httptest.NewRequest("POST", "/api/validate?format=csv", strings.NewReader("id\n1"))
	r.Header.Set("X-Schema", testSchema)
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}
func TestConcurrencyAndHealth(t *testing.T) {
	c := DefaultConfig()
	c.Concurrency = 1
	h, _ := New(c)
	srv := httptest.NewServer(h)
	defer srv.Close()
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/api/validate?format=jsonl", pr)
	r.Header.Set("X-Schema", testSchema)
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, e := srv.Client().Do(r)
		if e == nil {
			res.Body.Close()
		}
	}()
	defer func() { pw.Close(); cancel(); <-done }()
	go func() { _, _ = io.WriteString(pw, "{\"id\":1}\n") }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		res, e := srv.Client().Get(srv.URL + "/metrics")
		if e != nil {
			t.Fatal(e)
		}
		data, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if strings.Contains(string(data), "streamcheck_active 1") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request never occupied slot")
		}
		time.Sleep(5 * time.Millisecond)
	}
	w := request(h, "{}", "jsonl", testSchema)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal(w.Code)
	}
	res, e := srv.Client().Get(srv.URL + "/healthz")
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
}
func TestRequestTimeoutReleasesSlot(t *testing.T) {
	c := DefaultConfig()
	c.Concurrency = 1
	c.Timeout = 50 * time.Millisecond
	h, _ := New(c)
	srv := httptest.NewServer(h)
	defer srv.Close()
	pr, pw := io.Pipe()
	defer pw.Close()
	r, _ := http.NewRequest("POST", srv.URL+"/api/validate?format=jsonl", pr)
	r.Header.Set("X-Schema", testSchema)
	go func() { _, _ = io.WriteString(pw, "{\"id\":1}\n") }()
	res, e := srv.Client().Do(r)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 408 {
		t.Fatal(res.StatusCode)
	}
	if w := request(h, "{\"id\":1}", "jsonl", testSchema); w.Code != 200 {
		t.Fatal("slot leaked", w.Code)
	}
}
