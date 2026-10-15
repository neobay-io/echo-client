package core

import "net/http"

// NoRedirectHTTPClient prevents credential-bearing discovery requests from
// following redirects. Callers receive the original 3xx response.
func NoRedirectHTTPClient() *http.Client {
	return &http.Client{
		Transport: http.DefaultTransport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
