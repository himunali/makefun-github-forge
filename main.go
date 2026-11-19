// forge: the Go core of the github-core-clone entry. Listens on an
// ephemeral port, registers it with the env, serves the forge over raw
// TCP until the env says the trial is done, then exits 0.
// Lifecycle mirrors entry/main.ts (TypeScript). The binary writes no
// files at runtime and needs no GOCACHE, so it runs from a read-only
// /bot directory.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const requestTimeout = 30 * time.Second
const retryInitialMs = 250
const retryMaxMs = 2000

func sleepMs(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

func retryDelay(attempt int) int {
	d := retryInitialMs
	for i := 0; i < attempt && i < 8; i++ {
		d *= 2
	}
	if d > retryMaxMs {
		return retryMaxMs
	}
	return d
}

func register(envURL string, port int, client *http.Client) error {
	for attempt := 0; ; attempt++ {
		body, _ := json.Marshal(map[string]any{"port": port})
		req, err := http.NewRequest("POST", envURL+"/register", bytes.NewReader(body))
		if err == nil {
			req.Header.Set("content-type", "application/json")
			resp, err := client.Do(req)
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode == 200 || resp.StatusCode == 409 {
					return nil
				}
				if resp.StatusCode == 400 || resp.StatusCode == 405 {
					return fmt.Errorf("register answered HTTP %d", resp.StatusCode)
				}
			}
		}
		sleepMs(retryDelay(attempt))
	}
}

func waitForDone(envURL string, client *http.Client) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest("GET", envURL+"/done", nil)
		if err == nil {
			resp, err := client.Do(req)
			if err == nil {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					var parsed map[string]any
					if json.Unmarshal(body, &parsed) == nil {
						if done, ok := parsed["done"].(bool); ok && done {
							return
						}
					}
				}
			}
		}
		sleepMs(retryDelay(attempt))
	}
}

func main() {
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "[entry] MAKEFUN_ENV_URL is not set: there is no env to register with")
		os.Exit(1)
	}
	store := NewStore()
	srv := &server{store: store}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "[entry] listen:", err)
		os.Exit(1)
	}
	addr := ln.Addr().(*net.TCPAddr)
	fmt.Fprintf(os.Stdout, "[entry] listening on port %d\n", addr.Port)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.serveConn(conn)
		}
	}()
	client := &http.Client{Timeout: requestTimeout}
	if err := register(envURL, addr.Port, client); err != nil {
		fmt.Fprintln(os.Stderr, "[entry]", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "[entry] registered; waiting for the env to say done")
	waitForDone(envURL, client)
	ln.Close()
	fmt.Fprintln(os.Stdout, "[entry] done")
}
