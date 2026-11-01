package main

import (
	"crypto/sha256"
	"strings"
)

const (
	receiptMaxFiles = 64
	receiptMaxBytes = 1 << 20
	journalMaxBytes = 8 << 20
	traceMaxBytes   = 2 << 20
)

// ---------- small helpers ----------

func sum256(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

func digString(payload map[string]any, path ...string) string {
	var current any = payload
	for _, key := range path {
		next, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = next[key]
	}
	value, _ := current.(string)
	return strings.TrimSpace(value)
}

func digNumber(payload map[string]any, key string) float64 {
	value, ok := payload[key].(float64)
	if !ok {
		return 0
	}
	return value
}

func firstString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
