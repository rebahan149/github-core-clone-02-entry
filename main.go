package main

// Forge: a one-process GitHub-compatible git forge.
//
// A single static Go binary serves git smart HTTP (protocol v2 + v0
// receive-pack), the REST v3 surface, GraphQL and MCP over one in-memory
// store. Lifecycle follows the make.fun agent/v1 contract: bind an ephemeral
// port, POST it to $MAKEFUN_ENV_URL/register, poll $MAKEFUN_ENV_URL/done and
// exit 0 when the env says the trial is over. Without MAKEFUN_ENV_URL it runs
// in standalone dev mode on FORGE_PORT (default 8971) for local testing.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	store     = NewStore()
	shopToken = os.Getenv("MAKEFUN_SHOP_TOKEN")
	reqSeq    int64
)

func main() {
	handler := http.HandlerFunc(route)
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
	}

	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		// standalone dev mode
		port := os.Getenv("FORGE_PORT")
		if port == "" {
			port = "8971"
		}
		ln, err := net.Listen("tcp", "127.0.0.1:"+port)
		if err != nil {
			fmt.Fprintln(os.Stderr, "[forge] listen:", err)
			os.Exit(1)
		}
		fmt.Printf("[forge] standalone on http://127.0.0.1:%s (token auth %s)\n", port, tokenState())
		go func() {
			sig := make(chan os.Signal, 2)
			signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
			<-sig
			os.Exit(0)
		}()
		if err := srv.Serve(ln); err != nil {
			fmt.Fprintln(os.Stderr, "[forge] serve:", err)
			os.Exit(1)
		}
		return
	}

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "[forge] listen:", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, "[forge] serve:", err)
		}
	}()
	fmt.Printf("[forge] listening on port %d\n", port)
	if err := registerWithEnv(envURL, port); err != nil {
		fmt.Fprintln(os.Stderr, "[forge] register:", err)
		os.Exit(1)
	}
	fmt.Println("[forge] registered; waiting for done")
	waitForDone(envURL)
	os.Exit(0)
}

func tokenState() string {
	if shopToken == "" {
		return "disabled (no MAKEFUN_SHOP_TOKEN)"
	}
	return "enabled"
}

func registerWithEnv(envURL string, port int) error {
	body, _ := json.Marshal(map[string]int{"port": port})
	client := &http.Client{Timeout: 30 * time.Second}
	delay := 250 * time.Millisecond
	for attempt := 0; ; attempt++ {
		resp, err := client.Post(envURL+"/register", "application/json", bytes.NewReader(body))
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 409 {
				return nil
			}
			if resp.StatusCode == 400 || resp.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d: %s", resp.StatusCode, string(b))
			}
		}
		time.Sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

func waitForDone(envURL string) {
	client := &http.Client{Timeout: 30 * time.Second}
	delay := 250 * time.Millisecond
	for {
		resp, err := client.Get(envURL + "/done")
		if err == nil {
			var body struct {
				Done bool `json:"done"`
			}
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 && json.Unmarshal(data, &body) == nil && body.Done {
				return
			}
		}
		time.Sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

// ---- shared HTTP plumbing ----

func nextRequestID() string {
	n := atomic.AddInt64(&reqSeq, 1)
	return fmt.Sprintf("%08x", n)
}

// route is the single entry point for every request.
func route(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-GitHub-Request-Id", nextRequestID())
	path := r.URL.Path

	if path == "/healthz" {
		if r.Method != http.MethodGet {
			writeErr(w, 405, "method not allowed")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}

	switch {
	case path == "/api/graphql":
		handleGraphQL(w, r)
		return
	case path == "/mcp":
		handleMCP(w, r)
		return
	case path == "/api/v3" || strings.HasPrefix(path, "/api/v3/"):
		handleREST(w, r)
		return
	}

	// git smart HTTP: /{owner}/{repo}.git/...
	if strings.HasSuffix(path, ".git/info/refs") || strings.HasSuffix(path, ".git/git-upload-pack") || strings.HasSuffix(path, ".git/git-receive-pack") {
		handleGitHTTP(w, r)
		return
	}

	writeErr(w, 404, "Not Found")
}

// authed reports whether the request carries the shop token. When no token is
// configured (standalone dev), every request is accepted.
func authed(r *http.Request) bool {
	if shopToken == "" {
		return true
	}
	h := r.Header.Get("Authorization")
	return h == "Bearer "+shopToken || h == "token "+shopToken
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		w.Write([]byte(`{"message":"internal error"}`))
		return
	}
	writeJSONBytes(w, status, data)
}

func writeJSONBytes(w http.ResponseWriter, status int, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	w.Write(data)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"message": msg})
}

func readRawBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 64<<20))
}

// readJSONBody parses a JSON request body; on failure it answers 400 and returns false.
func readJSONBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeErr(w, 400, "could not read body")
		return nil, false
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, true
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		writeErr(w, 400, "malformed JSON body")
		return nil, false
	}
	if v == nil {
		v = map[string]any{}
	}
	return v, true
}

func strField(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func boolField(m map[string]any, key string) (bool, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

// viewerIdent is the git ident line value for forge-created commits.
func viewerIdent(ts int64) string {
	return fmt.Sprintf("%s <%s> %d +0000", viewerName, viewerEmail, ts)
}
