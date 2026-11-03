package sdk

import "net/http"

// RefuseRedirect is an http.Client.CheckRedirect that refuses every
// redirect, so a plugin's credentials (headers, HMAC keys) can never be
// replayed to another host. Interpreted plugins should use this instead
// of writing the equivalent function themselves.
func RefuseRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
