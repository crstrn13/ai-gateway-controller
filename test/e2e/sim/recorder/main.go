// Command recorder is a minimal, self-identifying OpenAI-compatible sim backend
// for the e2e harness. Unlike a plain echo sim, every response carries the
// backend's own PROVIDER_NAME in a top-level "provider" field (and in the message
// content), so a test can attribute a served request to the exact backend that
// handled it — the signal weighted/equal provider-selection tests need. It also
// records each request to an in-memory log served as NDJSON at
// /__test/observations, for server-side cross-checks.
//
// Deliberately HTTP-only and credential-free: that keeps provider-selection a
// self-contained Tier-1 test (no cert-manager / Authorino). The richer
// TLS/credential/translation recorder in test/kind-env is a separate, heavier
// concern and is not what this replaces.
//
// Config (env): PROVIDER_NAME (required, free-form identity), LISTEN_ADDR
// (default ":8443" with TLS, else ":8080"), TLS_DIR (if it holds tls.crt/tls.key
// the recorder serves HTTPS — required when used as an ExternalProvider backend,
// since the controller wires every provider as HTTPS).
package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

type observation struct {
	At       string `json:"at"`
	Provider string `json:"provider"`
	Method   string `json:"method"`
	Path     string `json:"path"`
	Model    string `json:"model,omitempty"`
	Stream   bool   `json:"stream"`
}

type recorder struct {
	provider string
	mu       sync.Mutex
	log      []observation
}

func main() {
	provider := os.Getenv("PROVIDER_NAME")
	if provider == "" {
		log.Fatal("PROVIDER_NAME is required")
	}
	tlsDir := os.Getenv("TLS_DIR")
	crtFile, keyFile := tlsDir+"/tls.crt", tlsDir+"/tls.key"
	useTLS := tlsDir != "" && fileExists(crtFile) && fileExists(keyFile)

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		if useTLS {
			addr = ":8443"
		} else {
			addr = ":8080"
		}
	}

	rec := &recorder{provider: provider}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/__test/observations", rec.handleObservations)
	mux.HandleFunc("/v1/chat/completions", rec.handleChat)
	mux.HandleFunc("/v1/messages", rec.handleChat)

	log.Printf("recorder ready provider=%s addr=%s tls=%t", provider, addr, useTLS)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	var err error
	if useTLS {
		err = srv.ListenAndServeTLS(crtFile, keyFile)
	} else {
		err = srv.ListenAndServe()
	}
	if err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (rec *recorder) handleObservations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	rec.mu.Lock()
	snapshot := append([]observation(nil), rec.log...)
	rec.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := json.NewEncoder(w)
	for _, o := range snapshot {
		_ = enc.Encode(o)
	}
}

func (rec *recorder) handleChat(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, (12<<20)+1))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	body := map[string]any{}
	_ = json.Unmarshal(raw, &body)
	model, _ := body["model"].(string)
	stream, _ := body["stream"].(bool)

	rec.mu.Lock()
	rec.log = append(rec.log, observation{
		At:       time.Now().UTC().Format(time.RFC3339Nano),
		Provider: rec.provider,
		Method:   r.Method,
		Path:     r.URL.Path,
		Model:    model,
		Stream:   stream,
	})
	rec.mu.Unlock()

	if model == "" {
		model = "sim-model"
	}
	if stream {
		rec.writeStream(w, model)
		return
	}
	resp := map[string]any{
		"id":       "chatcmpl-sim",
		"object":   "chat.completion",
		"model":    model,
		"provider": rec.provider, // the in-band attribution signal
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "reply from " + rec.provider},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (rec *recorder) writeStream(w http.ResponseWriter, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	stop := "stop"
	events := []map[string]any{
		{"id": "chatcmpl-sim", "object": "chat.completion.chunk", "model": model, "provider": rec.provider,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}}}},
		{"id": "chatcmpl-sim", "object": "chat.completion.chunk", "model": model, "provider": rec.provider,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "reply from " + rec.provider}}}},
		{"id": "chatcmpl-sim", "object": "chat.completion.chunk", "model": model, "provider": rec.provider,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": stop}}},
	}
	for _, e := range events {
		b, _ := json.Marshal(e)
		_, _ = io.WriteString(w, "data: "+string(b)+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}
