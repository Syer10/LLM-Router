package main

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// authOK reports whether the request carries a valid API key.
// When no keys are configured the router is open (local use).
// Accepted: "Authorization: Bearer <key>" or "x-api-key: <key>".
func (r *Router) authOK(req *http.Request) bool {
	if len(r.cfg.APIKeys) == 0 {
		return true
	}
	key := req.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(key) > len(prefix) && strings.EqualFold(key[:len(prefix)], prefix) {
		key = strings.TrimSpace(key[len(prefix):])
	} else {
		key = req.Header.Get("X-Api-Key")
	}
	if key == "" {
		return false
	}
	for _, k := range r.cfg.APIKeys {
		if subtle.ConstantTimeCompare([]byte(key), []byte(k)) == 1 {
			return true
		}
	}
	return false
}
