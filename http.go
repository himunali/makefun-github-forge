// Raw TCP HTTP/1.1 server + routing. Port of entry/app.ts: same auth,
// same route order, same JSON/404/500 behaviour, no node:http overhead.
// One global mutex serializes request handling (the store is not
// thread-safe, mirroring Node's single-threaded semantics); the win
// comes from cheaper per-request work, not from parallelism.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

const jsonType = "application/json; charset=utf-8"

type httpRequest struct {
	Method    string
	RawPath   string
	Path      string
	Query     map[string][]string
	Headers   map[string]string
	Body      []byte
	Version   string
	KeepAlive bool
}

type httpResponse struct {
	Status      int
	ContentType string
	Body        []byte
	Extra       map[string]string
}

var statusText = map[int]string{
	200: "OK", 201: "Created", 202: "Accepted", 204: "No Content",
	400: "Bad Request", 401: "Unauthorized", 404: "Not Found",
	405: "Method Not Allowed", 409: "Conflict", 422: "Unprocessable Entity",
	500: "Internal Server Error",
}

func statusLine(code int) string {
	text, ok := statusText[code]
	if !ok {
		text = "Status"
	}
	return fmt.Sprintf("HTTP/1.1 %d %s\r\n", code, text)
}

func sendJSONBody(status int, body any, extra map[string]string) httpResponse {
	if body == nil {
		return httpResponse{Status: status, ContentType: jsonType, Body: []byte{}, Extra: extra}
	}
	b := jsonBytes(body)
	b = append(b, '\n')
	return httpResponse{Status: status, ContentType: jsonType, Body: b, Extra: extra}
}

func sendBufferBody(status int, contentType string, body []byte, extra map[string]string) httpResponse {
	return httpResponse{Status: status, ContentType: contentType, Body: body, Extra: extra}
}

// readRequest reads one HTTP/1.1 request from the buffered reader.
func readRequest(r *bufio.Reader) (*httpRequest, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("bad request line %q", line)
	}
	req := &httpRequest{Method: parts[0], RawPath: parts[1], Version: parts[2], Headers: map[string]string{}}
	path, queryStr, _ := strings.Cut(parts[1], "?")
	req.Path = path
	if q, err := url.ParseQuery(queryStr); err == nil {
		req.Query = q
	} else {
		req.Query = map[string][]string{}
	}
	for {
		hline, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		hline = strings.TrimRight(hline, "\r\n")
		if hline == "" {
			break
		}
		idx := strings.Index(hline, ":")
		if idx < 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(hline[:idx]))
		value := strings.TrimSpace(hline[idx+1:])
		if _, ok := req.Headers[name]; !ok {
			req.Headers[name] = value
		}
	}
	// Keep-alive decision.
	connHeader := strings.ToLower(req.Headers["connection"])
	if req.Version == "HTTP/1.0" {
		req.KeepAlive = connHeader == "keep-alive"
	} else {
		req.KeepAlive = connHeader != "close"
	}
	// Expect: 100-continue — caller writes the interim response; signal
	// via a sentinel in Headers handled by readBody below.
	return req, nil
}

func readBody(r *bufio.Reader, req *httpRequest, w *bufio.Writer) error {
	// 100-continue.
	if strings.Contains(strings.ToLower(req.Headers["expect"]), "100-continue") {
		_, _ = w.WriteString("HTTP/1.1 100 Continue\r\n\r\n")
		_ = w.Flush()
	}
	te := strings.ToLower(req.Headers["transfer-encoding"])
	if strings.Contains(te, "chunked") {
		var body []byte
		for {
			sizeLine, err := r.ReadString('\n')
			if err != nil {
				return err
			}
			sizeLine = strings.TrimSpace(sizeLine)
			if idx := strings.Index(sizeLine, ";"); idx >= 0 {
				sizeLine = sizeLine[:idx]
			}
			size, err := strconv.ParseInt(sizeLine, 16, 64)
			if err != nil {
				return fmt.Errorf("bad chunk size %q", sizeLine)
			}
			if size == 0 {
				// Trailers until an empty line.
				for {
					t, err := r.ReadString('\n')
					if err != nil {
						return err
					}
					if strings.TrimRight(t, "\r\n") == "" {
						break
					}
				}
				break
			}
			if int64(len(body))+size > 64*1024*1024 {
				return fmt.Errorf("request body too large")
			}
			chunk := make([]byte, size)
			if _, err := io.ReadFull(r, chunk); err != nil {
				return err
			}
			body = append(body, chunk...)
			// Trailing CRLF after the chunk.
			if _, err := r.ReadString('\n'); err != nil {
				return err
			}
		}
		req.Body = body
		return nil
	}
	cl := req.Headers["content-length"]
	if cl == "" {
		req.Body = []byte{}
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(cl))
	if err != nil || n < 0 {
		return fmt.Errorf("bad content-length %q", cl)
	}
	if n > 64*1024*1024 {
		return fmt.Errorf("request body too large")
	}
	if n == 0 {
		req.Body = []byte{}
		return nil
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return err
	}
	req.Body = body
	return nil
}

func writeResponse(w *bufio.Writer, resp httpResponse) error {
	var buf bytes.Buffer
	buf.WriteString(statusLine(resp.Status))
	if resp.ContentType != "" {
		buf.WriteString("Content-Type: " + resp.ContentType + "\r\n")
	}
	buf.WriteString("Content-Length: " + strconv.Itoa(len(resp.Body)) + "\r\n")
	for k, v := range resp.Extra {
		buf.WriteString(k + ": " + v + "\r\n")
	}
	buf.WriteString("\r\n")
	buf.Write(resp.Body)
	if _, err := w.Write(buf.Bytes()); err != nil {
		return err
	}
	return w.Flush()
}

// ---------------------------------------------------------------------------
// routing (mirrors app.ts)
// ---------------------------------------------------------------------------

type server struct {
	store *Store
	mu    sync.Mutex
}

func (s *server) handle(req *httpRequest) (resp httpResponse, keepAlive bool) {
	keepAlive = req.KeepAlive
	requestId := s.store.NextRequestId()
	defer func() {
		if rec := recover(); rec != nil {
			msg := fmt.Sprintf("%v", rec)
			resp = sendJSONBody(500, map[string]any{"message": msg}, map[string]string{"x-github-request-id": requestId})
		}
	}()
	method := req.Method
	pathname := req.Path
	host := req.Headers["host"]
	if host == "" {
		host = "127.0.0.1"
	}
	if method == "GET" && pathname == "/healthz" {
		return sendJSONBody(200, map[string]any{"ok": true}, map[string]string{"x-github-request-id": requestId}), keepAlive
	}
	auth := req.Headers["authorization"]
	tokenOk := s.store.Token == "" || auth == "Bearer "+s.store.Token
	if !tokenOk {
		return sendJSONBody(401, map[string]any{"message": "Unauthorized"}, map[string]string{"x-github-request-id": requestId}), keepAlive
	}
	body := req.Body
	bodyText := func() string { return string(body) }

	// Git smart HTTP.
	if strings.Contains(pathname, ".git/") || strings.HasSuffix(pathname, ".git") {
		gitReq := &GitRequest{Method: method, Path: pathname, Query: req.Query, Headers: req.Headers, Body: body}
		if gitRes := handleGit(s.store, gitReq); gitRes != nil {
			extra := map[string]string{"x-github-request-id": requestId}
			for k, v := range gitRes.ExtraHeaders {
				extra[k] = v
			}
			return sendBufferBody(gitRes.Status, gitRes.ContentType, gitRes.Body, extra), keepAlive
		}
	}

	// GraphQL.
	if pathname == "/api/graphql" {
		if method != "POST" {
			return sendJSONBody(405, map[string]any{"message": "Method Not Allowed"}, map[string]string{"x-github-request-id": requestId}), keepAlive
		}
		var parsed any
		parseFailed := false
		if strings.TrimSpace(bodyText()) != "" {
			if err := json.Unmarshal(body, &parsed); err != nil {
				parseFailed = true
			}
		}
		if parseFailed {
			return sendJSONBody(400, map[string]any{"errors": []any{map[string]any{"message": "Request body is not valid JSON"}}}, map[string]string{"x-github-request-id": requestId}), keepAlive
		}
		result := handleGraphQL(s.store, parsed)
		return sendJSONBody(result.Status, result.Body, map[string]string{"x-github-request-id": requestId}), keepAlive
	}

	// MCP.
	if pathname == "/mcp" {
		result := handleMcp(s.store, &McpRequest{Method: method, BodyText: bodyText(), Headers: req.Headers})
		extra := map[string]string{"x-github-request-id": requestId}
		for k, v := range result.Headers {
			extra[k] = v
		}
		if result.Body == nil {
			return httpResponse{Status: result.Status, Body: []byte{}, Extra: extra}, keepAlive
		}
		return sendJSONBody(result.Status, result.Body, extra), keepAlive
	}

	// REST v3.
	if pathname == "/api/v3" || strings.HasPrefix(pathname, "/api/v3/") {
		var parsed any
		parseFailed := false
		needsJson := method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE"
		if needsJson && strings.TrimSpace(bodyText()) != "" {
			if err := json.Unmarshal(body, &parsed); err != nil {
				parseFailed = true
			}
		}
		if parseFailed {
			return sendJSONBody(400, map[string]any{"message": "Invalid JSON body"}, map[string]string{"x-github-request-id": requestId}), keepAlive
		}
		restReq := &RestRequest{Method: method, Path: pathname, Query: req.Query, Headers: req.Headers, BodyText: bodyText(), Body: parsed, Host: host}
		if restRes := handleRest(s.store, restReq); restRes != nil {
			extra := map[string]string{"x-github-request-id": requestId}
			for k, v := range restRes.Headers {
				extra[k] = v
			}
			if restRes.Status == 204 {
				return httpResponse{Status: 204, ContentType: jsonType, Body: []byte{}, Extra: extra}, keepAlive
			}
			if restRes.BodyRaw != nil {
				return sendBufferBody(restRes.Status, jsonType, []byte(*restRes.BodyRaw), extra), keepAlive
			}
			return sendJSONBody(restRes.Status, restRes.Body, extra), keepAlive
		}
	}

	return sendJSONBody(404, map[string]any{"message": "Not Found"}, map[string]string{"x-github-request-id": requestId}), keepAlive
}

func (s *server) serveConn(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	for {
		req, err := readRequest(r)
		if err != nil {
			return
		}
		if err := readBody(r, req, w); err != nil {
			return
		}
		s.mu.Lock()
		resp, keepAlive := s.handle(req)
		s.mu.Unlock()
		if err := writeResponse(w, resp); err != nil {
			return
		}
		if !keepAlive {
			return
		}
	}
}
