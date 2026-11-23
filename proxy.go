package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
)

// hopHeaders must not be copied between client and backend requests.
var hopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Host":                true,
	"Content-Length":      true,
}

func copyRequestHeaders(dst *http.Request, src http.Header) {
	for k, vs := range src {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			dst.Header.Add(k, v)
		}
	}
}

// HandleV1 serves the whole OpenAI-compatible surface as a transparent
// proxy. Any path under /v1 (or at the root, without the /v1 prefix) is
// routed to a backend; the backend decides which endpoints it actually
// supports. The backend is chosen by the request's "model" field when one is
// present (alias, "backend/model", or a bare name); otherwise the first
// backend is used. The model list is aggregated across all backends.
func (r *Router) HandleV1(w http.ResponseWriter, req *http.Request) {
	if !r.authOK(req) {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid or missing API key")
		return
	}
	path := strings.TrimPrefix(req.URL.Path, "/v1")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	subpath := path
	if req.URL.RawQuery != "" {
		subpath = path + "?" + req.URL.RawQuery
	}

	switch {
	case req.Method == http.MethodGet && path == "/models":
		// Aggregate the model list across all backends.
		r.handleModels(w)
	case req.Method == http.MethodGet && strings.HasPrefix(path, "/models/"):
		// Single model: forward to the backend that lists it.
		r.handleModelRetrieve(w, req, subpath, strings.TrimPrefix(path, "/models/"))
	default:
		// Everything else: transparent pass-through with model-based routing.
		r.handleGeneric(w, req, subpath)
	}
}

func (r *Router) handleModels(w http.ResponseWriter) {
	type modelOut struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	data := []modelOut{}
	for _, name := range r.order {
		for _, m := range r.backends[name].Models() {
			data = append(data, modelOut{ID: m.Name, Object: "model", OwnedBy: m.Backend})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// handleModelRetrieve serves GET /v1/models/{id}: it resolves the id to the
// backend that lists it and forwards the request (with its query string)
// there.
func (r *Router) handleModelRetrieve(w http.ResponseWriter, req *http.Request, subpath, id string) {
	b, _, err := r.Resolve(id)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return
	}
	b.Touch()
	resp, err := forwardWithRetry(req.Context(), b, http.MethodGet, subpath, nil, req.Header)
	if err != nil {
		log.Printf("forward to %s: %v", b.Name(), err)
		writeOpenAIError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	copyResponse(w, resp)
}

// handleGeneric forwards an arbitrary OpenAI request to a backend. When the
// body is JSON carrying a "model" field, the model is resolved to pick the
// backend and rewritten to the backend's real name; otherwise the first
// backend is used and the body is passed through unchanged. The method, path,
// query string and headers are preserved.
// defaultMaxBodyBytes is the request-body cap used when the config does not
// set max_body_bytes. It stops a single (malicious or misbehaving) client from
// exhausting the router's memory. LLM request bodies are text and small;
// 100 MiB is far beyond any real prompt.
const defaultMaxBodyBytes = 100 << 20

func (r *Router) handleGeneric(w http.ResponseWriter, req *http.Request, subpath string) {
	var body []byte
	if req.Body != nil {
		limit := r.cfg.MaxBodyBytes
		if limit <= 0 {
			limit = defaultMaxBodyBytes
		}
		req.Body = http.MaxBytesReader(w, req.Body, limit)
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			status := http.StatusBadRequest
			var mb *http.MaxBytesError
			if errors.As(err, &mb) {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, "reading body: "+err.Error(), status)
			return
		}
	}

	b := r.defaultBackend()
	if b == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no backends configured")
		return
	}

	// Resolve and rewrite the model when the request carries one.
	if len(body) > 0 {
		var payload map[string]any
		if json.Unmarshal(body, &payload) == nil {
			if model, ok := payload["model"].(string); ok && model != "" {
				resolved, realModel, err := r.Resolve(model)
				if err != nil {
					writeOpenAIError(w, http.StatusNotFound, err.Error())
					return
				}
				b = resolved
				payload["model"] = realModel
				body, _ = json.Marshal(payload)
				log.Printf("routing %q -> %s/%s (%s %s)", model, b.Name(), logSafe(realModel), req.Method, logSafe(subpath))
			}
		}
	}

	if err := b.EnsureLoaded(req.Context(), ""); err != nil {
		log.Printf("ensure loaded %s: %v", b.Name(), err)
		writeOpenAIError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	b.Touch()

	resp, err := forwardWithRetry(req.Context(), b, req.Method, subpath, body, req.Header)
	if err != nil {
		log.Printf("forward to %s: %v", b.Name(), err)
		writeOpenAIError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	copyResponse(w, resp)
}

// forwardWithRetry sends the request to the backend, retrying once when the
// connection fails at the transport level — typically the backend having
// closed an idle keep-alive connection just before we reused it. A non-nil
// error from Forward means no response was received, so the backend did not
// process the request and a retry is safe (the body is held in memory).
func forwardWithRetry(ctx context.Context, b Backend, method, subpath string, body []byte, headers http.Header) (*http.Response, error) {
	resp, err := b.Forward(ctx, method, subpath, body, headers)
	if err == nil || !isTransientConnErr(err) {
		return resp, err
	}
	log.Printf("transient connection error to %s (%v); retrying once", b.Name(), err)
	return b.Forward(ctx, method, subpath, body, headers)
}

// isTransientConnErr reports whether err is a transport-level connection
// failure (dead keep-alive connection, reset, broken pipe, EOF) in which the
// backend did not receive the request, so it is safe to retry.
func isTransientConnErr(err error) bool {
	var uerr *url.Error
	if !errors.As(err, &uerr) {
		return false
	}
	msg := uerr.Error()
	return errors.Is(uerr.Err, io.EOF) ||
		strings.Contains(msg, "server closed idle connection") ||
		strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection reset by peer")
}

// logSafe replaces control characters (newlines, etc.) in a value before it
// is written to the log, so a client-influenced string cannot forge log lines.
func logSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r < 0x20 {
			return ' '
		}
		return r
	}, s)
}

// defaultBackend returns the first backend in config order, used for requests
// that carry no resolvable model (list endpoints, files, ...).
func (r *Router) defaultBackend() Backend {
	if len(r.order) == 0 {
		return nil
	}
	return r.backends[r.order[0]]
}

func writeOpenAIError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": "router_error", "code": status},
	})
}

// copyResponse streams the backend response to the client, flushing after
// each chunk so SSE streams pass through in real time.
func copyResponse(w http.ResponseWriter, resp *http.Response) {
	for k, vs := range resp.Header {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
