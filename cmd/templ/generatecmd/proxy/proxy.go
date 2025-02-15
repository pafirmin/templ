package proxy

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ/cmd/templ/generatecmd/sse"

	_ "embed"
)

//go:embed script.js
var script string

const scriptTag = `<script src="/_templ/reload/script.js"></script>`

type Handler struct {
	URL    string
	Target *url.URL
	p      *httputil.ReverseProxy
	sse    *sse.Handler
}

func updateGzipResponse(r *http.Response) error {
	plainr, err := gzip.NewReader(r.Body)
	if err != nil {
		return err
	}
	defer plainr.Close()
	body, err := io.ReadAll(plainr)
	if err != nil {
		return err
	}
	updated := insertScriptTagIntoBody(string(body))
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	defer gzw.Close()
	_, err = gzw.Write([]byte(updated))
	if err != nil {
		return err
	}
	err = gzw.Close()
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(&buf)
	r.ContentLength = int64(buf.Len())
	r.Header.Set("Content-Length", strconv.Itoa(buf.Len()))
	return nil
}

func updatePlainResponse(r *http.Response) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	updated := insertScriptTagIntoBody(string(body))
	r.Body = io.NopCloser(strings.NewReader(updated))
	r.ContentLength = int64(len(updated))
	r.Header.Set("Content-Length", strconv.Itoa(len(updated)))
	return nil
}

func insertScriptTagIntoBody(body string) (updated string) {
	return strings.Replace(body, "</body>", scriptTag+"</body>", -1)
}

func modifyResponse(r *http.Response) error {
	if r.Header.Get("templ-skip-modify") == "true" {
		return nil
	}
	if contentType := r.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
		return nil
	}
	modifier := updatePlainResponse
	if r.Header.Get("Content-Encoding") == "gzip" {
		modifier = updateGzipResponse
	}
	return modifier(r)
}

func New(bind string, port int, target *url.URL) *Handler {
	p := httputil.NewSingleHostReverseProxy(target)
	p.ErrorLog = log.New(os.Stderr, "Proxy to target error: ", 0)
	p.Transport = &roundTripper{
		maxRetries:      10,
		initialDelay:    100 * time.Millisecond,
		backoffExponent: 1.5,
	}
	p.ModifyResponse = modifyResponse
	return &Handler{
		URL:    fmt.Sprintf("http://%s:%d", bind, port),
		Target: target,
		p:      p,
		sse:    sse.New(),
	}
}

func (p *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_templ/reload/script.js" {
		// Provides a script that reloads the page.
		w.Header().Add("Content-Type", "text/javascript")
		_, err := io.WriteString(w, script)
		if err != nil {
			fmt.Printf("failed to write script: %v\n", err)
		}
		return
	}
	if r.URL.Path == "/_templ/reload/events" {
		switch r.Method {
		case http.MethodGet:
			// Provides a list of messages including a reload message.
			p.sse.ServeHTTP(w, r)
			return
		case http.MethodPost:
			// Send a reload message to all connected clients.
			p.sse.Send("message", "reload")
			return
		}
		http.Error(w, "only GET or POST method allowed", http.StatusMethodNotAllowed)
		return
	}
	p.p.ServeHTTP(w, r)
}

func (p *Handler) SendSSE(eventType string, data string) {
	p.sse.Send(eventType, data)
}

type roundTripper struct {
	maxRetries      int
	initialDelay    time.Duration
	backoffExponent float64
}

func (rt *roundTripper) setShouldSkipResponseModificationHeader(r *http.Request, resp *http.Response) {
	// Instruct the modifyResponse function to skip modifying the response if the
	// HTTP request has come from HTMX.
	if r.Header.Get("HX-Request") != "true" {
		return
	}
	resp.Header.Set("templ-skip-modify", "true")
}

func (rt *roundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	// Read and buffer the body.
	var bodyBytes []byte
	if r.Body != nil && r.Body != http.NoBody {
		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body.Close()
	}

	// Retry logic.
	var resp *http.Response
	var err error
	for retries := 0; retries < rt.maxRetries; retries++ {
		// Clone the request and set the body.
		req := r.Clone(r.Context())
		if bodyBytes != nil {
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}

		// Execute the request.
		resp, err = http.DefaultTransport.RoundTrip(req)
		if err != nil {
			time.Sleep(rt.initialDelay * time.Duration(math.Pow(rt.backoffExponent, float64(retries))))
			continue
		}

		rt.setShouldSkipResponseModificationHeader(r, resp)

		return resp, nil
	}

	return nil, fmt.Errorf("max retries reached")
}

func NotifyProxy(host string, port int) error {
	urlStr := fmt.Sprintf("http://%s:%d/_templ/reload/events", host, port)
	req, err := http.NewRequest(http.MethodPost, urlStr, nil)
	if err != nil {
		return err
	}
	_, err = http.DefaultClient.Do(req)
	return err
}
