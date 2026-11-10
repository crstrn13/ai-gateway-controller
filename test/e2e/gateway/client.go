// Package gateway is a tiny SDK for driving the AI Gateway's OpenAI-compatible
// endpoints over HTTP in e2e tests. Construct a Client from the gateway's address
// (New), set defaults with Options, and call ChatCompletion / ChatCompletionStream
// / Post. It is a thin wrapper over *http.Client — no Kubernetes knowledge; the
// caller resolves the gateway address (see funcs.GatewayClient).
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"
)

// Client talks to one gateway base URL.
type Client struct {
	baseURL string
	http    *http.Client
	header  http.Header
}

// Option configures a Client at construction.
type Option func(*Client)

// New builds a Client for baseURL (e.g. "http://172.19.0.200").
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 30 * time.Second},
		header:  http.Header{},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// WithHTTPClient supplies a custom *http.Client (TLS, transport, timeout).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithTimeout sets the request timeout on the default client.
func WithTimeout(d time.Duration) Option { return func(c *Client) { c.http.Timeout = d } }

// WithBearer sets a default Authorization: Bearer token on every request.
func WithBearer(token string) Option {
	return func(c *Client) { c.header.Set("Authorization", "Bearer "+token) }
}

// WithHeader sets a default header on every request.
func WithHeader(k, v string) Option { return func(c *Client) { c.header.Set(k, v) } }

// Message is one chat message.
type Message struct{ Role, Content string }

// User is the common single user-message case.
func User(content string) []Message { return []Message{{Role: "user", Content: content}} }

// ChatRequest is an OpenAI-style chat-completions request. Extra merges arbitrary
// top-level fields (tools, temperature, …).
type ChatRequest struct {
	Model    string
	Messages []Message
	Stream   bool
	Extra    map[string]any
}

func (r ChatRequest) payload() map[string]any {
	msgs := make([]map[string]any, len(r.Messages))
	for i, m := range r.Messages {
		msgs[i] = map[string]any{"role": m.Role, "content": m.Content}
	}
	p := map[string]any{"model": r.Model, "messages": msgs}
	if r.Stream {
		p["stream"] = true
	}
	maps.Copy(p, r.Extra)
	return p
}

// Response is a buffered HTTP response.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// JSON unmarshals the body into v.
func (r *Response) JSON(v any) error { return json.Unmarshal(r.Body, v) }

// String returns the body as a string.
func (r *Response) String() string { return string(r.Body) }

// RequestOption tweaks a single request (per-call headers).
type RequestOption func(*http.Request)

// Header sets a per-request header.
func Header(k, v string) RequestOption { return func(req *http.Request) { req.Header.Set(k, v) } }

// ModelHeader sets the BBR routing header for a single request (bypasses body
// promotion — useful for stack smoke tests).
func ModelHeader(model string) RequestOption { return Header("X-Gateway-Model-Name", model) }

// ChatCompletion sends a buffered (non-streaming) chat completion.
func (c *Client) ChatCompletion(ctx context.Context, r ChatRequest, opts ...RequestOption) (*Response, error) {
	r.Stream = false
	return c.Post(ctx, "/v1/chat/completions", r.payload(), opts...)
}

// Post marshals payload as JSON and POSTs it to path.
func (c *Client) Post(ctx context.Context, path string, payload any, opts ...RequestOption) (*Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, path, bytes.NewReader(body), opts...)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: b}, nil
}

// Stream is an SSE reader over chat-completion chunks.
type Stream struct {
	resp *http.Response
	sc   *bufio.Scanner
}

// ChatCompletionStream opens a streaming chat completion (stream=true). The caller
// must Close the returned Stream.
func (c *Client) ChatCompletionStream(ctx context.Context, r ChatRequest, opts ...RequestOption) (*Stream, error) {
	r.Stream = true
	body, err := json.Marshal(r.payload())
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/chat/completions", bytes.NewReader(body), opts...)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("stream: status %d: %s", resp.StatusCode, b)
	}
	return &Stream{resp: resp, sc: bufio.NewScanner(resp.Body)}, nil
}

// Recv returns the next SSE `data:` payload, or io.EOF at `[DONE]` / end of stream.
func (s *Stream) Recv() ([]byte, error) {
	for s.sc.Scan() {
		line := strings.TrimSpace(s.sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			return nil, io.EOF
		}
		return []byte(data), nil
	}
	if err := s.sc.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

// Collect drains the stream, returning every data payload until EOF.
func (s *Stream) Collect() ([][]byte, error) {
	var out [][]byte
	for {
		chunk, err := s.Recv()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, chunk)
	}
}

// Close closes the underlying response body.
func (s *Stream) Close() error { return s.resp.Body.Close() }

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader, opts ...RequestOption) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	for k, vs := range c.header {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(req)
	}
	return req, nil
}
