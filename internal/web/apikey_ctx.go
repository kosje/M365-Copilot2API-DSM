package web

import (
	"context"
	"net/http"
)

// apiKeyRecord carries the validated API key record from adminMiddleware into
// /v1 handlers so they can enforce per-key model whitelists without a second
// store lookup.
type apiKeyCtxKey struct{}

func withAPIKeyRecord(ctx context.Context, rec apiKeyRecord) context.Context {
	return context.WithValue(ctx, apiKeyCtxKey{}, rec)
}

// apiKeyFromRequest returns the validated key record for this request, if the
// middleware stored one (only /v1/ requests with a resolvable key do).
func apiKeyFromRequest(r *http.Request) (apiKeyRecord, bool) {
	rec, ok := r.Context().Value(apiKeyCtxKey{}).(apiKeyRecord)
	return rec, ok
}

// requestModelAllowed checks the per-key model whitelist for the resolved
// model. Returns true when no record or no whitelist is configured.
func requestModelAllowed(r *http.Request, model string) bool {
	rec, ok := apiKeyFromRequest(r)
	if !ok {
		return true
	}
	return modelAllowed(model, rec.ModelWhitelist)
}
