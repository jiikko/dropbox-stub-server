package stub

import (
	"net"
	"net/http"
	"net/url"
)

// Fixed credentials: the stub has a single implicit account and accepts any bearer token.
const (
	stubAuthCode     = "stub-auth-code"
	stubAccessToken  = "stub-access-token"
	stubRefreshToken = "stub-refresh-token"
	stubAccountID    = "dbid:stub-account"
	stubUID          = "1"
)

// handleAuthorize skips the consent screen and redirects straight back with a code.
// Only loopback redirect URIs are accepted, so the stub cannot be used as an open redirect.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || redirect.Scheme != "http" || !isLoopbackHost(redirect.Hostname()) {
		badRequest(w, "redirect_uri must be an http loopback URL")
		return
	}
	rq := redirect.Query()
	rq.Set("code", stubAuthCode)
	if state := q.Get("state"); state != "" {
		rq.Set("state", state)
	}
	redirect.RawQuery = rq.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// handleToken issues the fixed token for both grant types the app uses.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		badRequest(w, "invalid form: %v", err)
		return
	}
	resp := map[string]any{
		"access_token": stubAccessToken,
		"token_type":   "bearer",
		"expires_in":   14400,
		"scope":        "files.content.read files.content.write files.metadata.read files.metadata.write",
		"uid":          stubUID,
		"account_id":   stubAccountID,
	}
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		if r.Form.Get("code") == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "code is missing"})
			return
		}
		resp["refresh_token"] = stubRefreshToken
	case "refresh_token":
		if r.Form.Get("refresh_token") == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "refresh_token is missing"})
			return
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported_grant_type"})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
