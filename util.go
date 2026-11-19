// Shared helpers: JSON encoding matching JS JSON.stringify (no HTML
// escaping), and generic decoded-JSON accessors. jsonBytes strips the
// trailing newline encoding/json appends; callers add it back exactly
// where the TypeScript sendJson does.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

func jsonBytes(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	b := buf.Bytes()
	// Encoder appends "\n"; callers add their own newline where the TS
	// sendJson does, so strip it here.
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b
}

func jsonString(v any) string { return string(jsonBytes(v)) }

func isRecord(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func getStr(v any, fallback ...string) string {
	if s, ok := v.(string); ok {
		return s
	}
	if len(fallback) > 0 {
		return fallback[0]
	}
	return ""
}

func getBool(v any, fallback ...bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	if len(fallback) > 0 {
		return fallback[0]
	}
	return false
}

// getFloat mirrors JS Number(): strings that parse, numbers as-is.
func getFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

func getInt(v any) (int, bool) {
	f, ok := getFloat(v)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return int(f), true
}

func b64Encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func b64Decode(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }

func globalIdEncode(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func globalIdDecode(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
