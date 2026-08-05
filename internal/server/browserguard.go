package server

import (
	"mime"
	"net/http"
	"net/url"
	"strings"
)

// guardBrowserRequests blocks state-changing requests that a web page made on
// your behalf.
//
// The threat is cross-site request forgery, and it survives every network
// control in this project. A tunnel or a firewall keeps *other machines* away
// from the control plane; it does nothing about the browser running on the
// same machine. Any page you visit can make your browser POST to
// 127.0.0.1:7337. The page cannot read the reply — but the task still gets
// created.
//
// Two cheap checks separate a real client from a browser being used as one:
//
//  1. Require Content-Type: application/json. Browsers may send plain-text and
//     form-encoded bodies cross-origin without asking, but a JSON body forces a
//     CORS preflight first. Nothing here answers preflights, so the browser
//     never sends the real request.
//
//  2. Reject a cross-origin Origin header. Browsers stamp Origin on cross-site
//     requests; the CLI and the worker never send one. So an Origin that is not
//     ours means a browser is acting for somebody else.
//
// GET requests are left alone: they change nothing, and a browser cannot read
// their responses cross-origin anyway. That also keeps the API browsable by
// hand while debugging.
func (s *Server) guardBrowserRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
			writeErr(w, http.StatusForbidden,
				"cross-origin request refused: this API is not reachable from a web page. "+
					"If you are a script, omit the Origin header.")
			return
		}

		if !isJSONContentType(r.Header.Get("Content-Type")) {
			writeErr(w, http.StatusUnsupportedMediaType,
				"Content-Type: application/json is required for state-changing requests. "+
					"This blocks web pages from driving the control plane through your browser.")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// isJSONContentType accepts application/json with or without parameters, such
// as "application/json; charset=utf-8".
func isJSONContentType(header string) bool {
	if header == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return false
	}
	return strings.EqualFold(mediaType, "application/json")
}

// sameOrigin reports whether an Origin header refers to the host serving the
// request, which is what a first-party page (a future dashboard) would send.
func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, host)
}
