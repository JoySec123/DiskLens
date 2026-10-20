package main

import (
	"net/http"
	"net/url"
	"strings"
)

func mutationAllowed(r *http.Request, gateway bool) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	// fnOS drops the external port from Host. A non-safelisted request header
	// protects the port-normalized comparison even on HTTP without Fetch Metadata.
	// Do not enable CORS: other origins must not be able to send this header.
	if gateway && r.Header.Get("X-DiskLens-Request") != "1" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	source, ok := parseRequestOrigin(origin)
	if !ok {
		return false
	}
	target, ok := parseRequestOrigin("http://" + r.Host)
	if !ok {
		return false
	}
	if strings.EqualFold(source.Host, target.Host) {
		return true
	}
	return gateway && target.Port() == "" && strings.EqualFold(source.Hostname(), target.Hostname())
}

func parseRequestOrigin(value string) (*url.URL, bool) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, false
	}
	// An Origin is exactly a scheme and authority, never a path or a URL list.
	if value != parsed.Scheme+"://"+parsed.Host || strings.ContainsAny(parsed.Host, " ,\\\t\r\n") {
		return nil, false
	}
	return parsed, true
}
