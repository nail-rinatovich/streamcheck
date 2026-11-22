package server

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"streamcheck/internal/validator"
)

//go:embed web/*
var assets embed.FS

type Config struct {
	MaxBytes       int64
	MaxRecordBytes int
	Concurrency    int
	Timeout        time.Duration
	Token          string
}

func DefaultConfig() Config {
	return Config{MaxBytes: 10 << 30, MaxRecordBytes: 1 << 20, Concurrency: 2, Timeout: 10 * time.Minute}
}

type Handler struct {
	config                                    Config
	slots                                     chan struct{}
	requests, rejected, rows, bytes, failures atomic.Int64
}

func New(c Config) (http.Handler, error) {
	if c.MaxBytes < 1 || c.MaxRecordBytes < 1 || c.MaxRecordBytes > 16<<20 || c.Concurrency < 1 || c.Concurrency > 64 || c.Timeout <= 0 {
		return nil, errors.New("invalid server limits")
	}
	h := &Handler{config: c, slots: make(chan struct{}, c.Concurrency)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("POST /api/validate", h.validate)
	mux.HandleFunc("GET /metrics", h.metrics)
	web, _ := fs.Sub(assets, "web")
	mux.Handle("GET /", http.FileServer(http.FS(web)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		mux.ServeHTTP(w, r)
	}), nil
}
func (h *Handler) validate(w http.ResponseWriter, r *http.Request) {
	if h.config.Token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+h.config.Token)) != 1 {
		writeJSON(w, 401, map[string]string{"error": "invalid API token"})
		return
	}
	format := r.URL.Query().Get("format")
	if format != "csv" && format != "jsonl" {
		writeJSON(w, 400, map[string]string{"error": "format must be csv or jsonl"})
		return
	}
	schema, err := validator.ParseSchema([]byte(r.Header.Get("X-Schema")))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		h.rejected.Add(1)
		w.Header().Set("Retry-After", "2")
		writeJSON(w, 429, map[string]string{"error": "validator busy; retry later"})
		return
	}
	h.requests.Add(1)
	deadline := time.Now().Add(h.config.Timeout)
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline.Add(5 * time.Second))
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	r.Body = http.MaxBytesReader(w, r.Body, h.config.MaxBytes)
	defer r.Body.Close()
	report, err := validator.Validate(ctx, r.Body, format, schema, validator.Options{MaxRecordBytes: h.config.MaxRecordBytes, SampleLimit: 20})
	h.rows.Add(report.Rows)
	h.bytes.Add(report.Bytes)
	if err != nil {
		h.failures.Add(1)
		code := http.StatusUnprocessableEntity
		var sizeErr *http.MaxBytesError
		if errors.As(err, &sizeErr) {
			code = 413
		} else if ctx.Err() != nil || time.Now().After(deadline) {
			code = 408
		}
		writeJSON(w, code, struct {
			Error  string           `json:"error"`
			Report validator.Report `json:"report"`
		}{err.Error(), report})
		return
	}
	writeJSON(w, 200, report)
}
func (h *Handler) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# TYPE streamcheck_requests_total counter\nstreamcheck_requests_total %d\n# TYPE streamcheck_rejected_total counter\nstreamcheck_rejected_total %d\n# TYPE streamcheck_failures_total counter\nstreamcheck_failures_total %d\n# TYPE streamcheck_rows_total counter\nstreamcheck_rows_total %d\n# TYPE streamcheck_bytes_total counter\nstreamcheck_bytes_total %d\n# TYPE streamcheck_active gauge\nstreamcheck_active %d\n", h.requests.Load(), h.rejected.Load(), h.failures.Load(), h.rows.Load(), h.bytes.Load(), len(h.slots))
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func Run(ctx context.Context) error {
	c := DefaultConfig()
	c.Token = os.Getenv("API_TOKEN")
	for _, setting := range []struct {
		name   string
		target *int64
	}{{"MAX_UPLOAD_BYTES", &c.MaxBytes}} {
		if s := os.Getenv(setting.name); s != "" {
			n, e := strconv.ParseInt(s, 10, 64)
			if e != nil {
				return fmt.Errorf("%s: %w", setting.name, e)
			}
			*setting.target = n
		}
	}
	for _, setting := range []struct {
		name   string
		target *int
	}{{"MAX_RECORD_BYTES", &c.MaxRecordBytes}, {"MAX_CONCURRENT", &c.Concurrency}} {
		if s := os.Getenv(setting.name); s != "" {
			n, e := strconv.Atoi(s)
			if e != nil {
				return fmt.Errorf("%s: %w", setting.name, e)
			}
			*setting.target = n
		}
	}
	if s := os.Getenv("REQUEST_TIMEOUT"); s != "" {
		v, e := time.ParseDuration(s)
		if e != nil {
			return e
		}
		c.Timeout = v
	}
	handler, err := New(c)
	if err != nil {
		return err
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "10000"
	}
	srv := &http.Server{Addr: "0.0.0.0:" + port, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 64 << 10}
	done := make(chan error, 1)
	go func() {
		slog.Info("streamcheck listening", "address", srv.Addr, "concurrency", c.Concurrency)
		done <- srv.ListenAndServe()
	}()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			_ = srv.Close()
			return err
		}
		return nil
	}
}
