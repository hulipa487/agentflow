// Command record is a development tool, not part of the shipped binary: it is
// the reverse proxy phase 0.3 of the OpenAI-channel plan asks the owner to run
// against a real client. Point the client's base_url at the proxy and the proxy
// at a real OpenAI-compatible upstream; every request/response pair is written
// to the output directory as one JSON file the channel's replay tests can load.
//
//	record -upstream https://api.openai.com/v1 -out cmd/agentflow/testdata/chatapi
//
// The recorded copy redacts credentials — the Authorization header, API-key
// headers, key-shaped strings and common secret fields in JSON bodies — while
// the proxied copy is forwarded unchanged. A capture is committed, so a real key
// in one would be a leaked key.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxCapture bounds one request body the recorder keeps in memory. It matches
// the channel's default max_request_bytes; a bigger capture is refused rather
// than written truncated, because a truncated fixture cannot be replayed.
const maxCapture = 32 << 20

type envelope struct {
	Sequence    int              `json:"sequence"`
	RecordedAt  string           `json:"recorded_at"`
	Handwritten bool             `json:"handwritten,omitempty"`
	Request     recordedRequest  `json:"request"`
	Response    recordedResponse `json:"response"`
}

type recordedRequest struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
}

type recordedResponse struct {
	Status      int                 `json:"status"`
	ContentType string              `json:"content_type"`
	Headers     map[string][]string `json:"headers"`
	Body        string              `json:"body"`
}

var (
	seq     atomic.Uint64
	writeMu sync.Mutex
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8099", "address to listen on")
	upstream := flag.String("upstream", "", "upstream base URL, e.g. https://api.openai.com/v1")
	out := flag.String("out", "cmd/agentflow/testdata/chatapi", "directory to write captures into")
	prefix := flag.String("prefix", "", "file name prefix (default derived from the upstream host)")
	flag.Parse()
	if *upstream == "" {
		log.Fatal("record: -upstream is required")
	}
	target, err := url.Parse(*upstream)
	if err != nil || target.Scheme == "" || target.Host == "" {
		log.Fatalf("record: -upstream must be an absolute URL (got %q)", *upstream)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatalf("record: %v", err)
	}
	name := *prefix
	if name == "" {
		name = strings.NewReplacer(".", "-", ":", "-").Replace(target.Host)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1 // stream SSE chunks as they arrive

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxCapture+1))
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(body) > maxCapture {
			http.Error(w, fmt.Sprintf("request body exceeds %d bytes; raise max_request_bytes or trim the capture", maxCapture), http.StatusRequestEntityTooLarge)
			return
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))

		rec := &responseRecorder{w: w, body: &bytes.Buffer{}}
		proxy.ServeHTTP(rec, r)

		writeCapture(*out, name, recordedRequest{
			Method:  r.Method,
			Path:    r.URL.RequestURI(),
			Headers: redactHeaders(r.Header),
			Body:    redact(body),
		}, recordedResponse{
			Status:      rec.status,
			ContentType: rec.w.Header().Get("Content-Type"),
			Headers:     redactHeaders(rec.w.Header()),
			Body:        redact(rec.body.Bytes()),
		})
	})
	log.Printf("recording %s -> %s into %s", *listen, target, *out)
	if err := http.ListenAndServe(*listen, h); err != nil {
		log.Fatal(err)
	}
}

// responseRecorder tees the upstream response: the client receives it as it
// streams, and the buffer keeps the copy the capture is written from. Flush
// forwards so an SSE capture is not held until the stream ends.
type responseRecorder struct {
	w           http.ResponseWriter
	status      int
	body        *bytes.Buffer
	wroteHeader bool
	mu          sync.Mutex
}

func (r *responseRecorder) Header() http.Header { return r.w.Header() }

func (r *responseRecorder) WriteHeader(status int) {
	r.mu.Lock()
	if !r.wroteHeader {
		r.status = status
		r.wroteHeader = true
	}
	r.mu.Unlock()
	r.w.WriteHeader(status)
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	_, _ = r.body.Write(p)
	r.mu.Unlock()
	return r.w.Write(p)
}

func (r *responseRecorder) Flush() {
	if f, ok := r.w.(http.Flusher); ok {
		f.Flush()
	}
}

func writeCapture(dir, prefix string, req recordedRequest, resp recordedResponse) {
	n := seq.Add(1)
	rec := envelope{
		Sequence:   int(n),
		RecordedAt: time.Now().UTC().Format(time.RFC3339),
		Request:    req,
		Response:   resp,
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	base := fmt.Sprintf("%04d-%s-%s", n, prefix, slug(req.Method+" "+req.Path))
	path := filepath.Join(dir, base+".json")
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		log.Printf("record: encode capture: %v", err)
		return
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		log.Printf("record: write capture: %v", err)
		return
	}
	log.Printf("recorded %s (%d bytes request, %d bytes response)", path, len(req.Body), len(resp.Body))
}

// slug makes a short, filesystem-safe file name fragment from a request line.
func slug(s string) string {
	var b strings.Builder
	lastDash := false
	for _, c := range strings.ToLower(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteRune(c)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 80 {
		out = out[:80]
	}
	if out == "" {
		out = "request"
	}
	return out
}

// secretHeaders are never recorded, and their forwarded value is preserved
// separately: redactHeaders is applied to the captured copy only.
var secretHeaders = map[string]bool{
	"Authorization":       true,
	"Proxy-Authorization": true,
	"X-Api-Key":           true,
	"Api-Key":             true,
	"X-Goog-Api-Key":      true,
	"Cookie":              true,
	"Set-Cookie":          true,
}

func redactHeaders(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, vs := range h {
		if secretHeaders[http.CanonicalHeaderKey(k)] {
			out[k] = []string{"REDACTED"}
			continue
		}
		cp := make([]string, len(vs))
		copy(cp, vs)
		out[k] = cp
	}
	return out
}

var (
	// A provider key: the prefixes used in the wild (OpenAI, Anthropic, Google,
	// Together, Groq) followed by an opaque body. Long enough to avoid eating an
	// ordinary word such as "sk-learn".
	keyRe = regexp.MustCompile(`\b(?:sk|rk|pk|gsk|hf|tv|anthropic|AIza)[-_A-Za-z0-9]{12,}\b`)
	// A bearer or basic credential that survived into a body.
	authRe = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
)

// redact removes credentials from a captured body. JSON is walked so a
// secret-named field is replaced wholesale; everything else gets a textual
// sweep for key-shaped strings, which also covers text/plain and SSE bodies.
func redact(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var v any
	if json.Unmarshal(body, &v) == nil {
		redactValue(v)
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
	}
	return redactText(string(body))
}

func redactText(s string) string {
	s = keyRe.ReplaceAllString(s, "REDACTED")
	return authRe.ReplaceAllString(s, "$1 REDACTED")
}

var secretFields = map[string]bool{
	"api_key":       true,
	"apikey":        true,
	"api-key":       true,
	"authorization": true,
	"password":      true,
	"secret":        true,
	"token":         true,
	"access_token":  true,
	"refresh_token": true,
	"client_secret": true,
}

func redactValue(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if secretFields[strings.ToLower(k)] {
				t[k] = "REDACTED"
				continue
			}
			if s, ok := child.(string); ok {
				t[k] = redactText(s)
				continue
			}
			redactValue(child)
		}
	case []any:
		for i := range t {
			if s, ok := t[i].(string); ok {
				t[i] = redactText(s)
				continue
			}
			redactValue(t[i])
		}
	}
}
