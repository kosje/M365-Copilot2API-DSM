package web

import (
	"context"
	"net/http"
	"strings"
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

// internalAuthKey marks a request already authenticated in-process, by a
// handler that resolved the key record itself. The /chat UI proxy uses this so
// it can call /v1 on a chat user's behalf without storing that user's
// cleartext key anywhere.
//
// A remote client cannot set request context values, so this cannot be forged
// over the network. adminMiddleware honours it only for /v1/ paths.
type internalAuthKey struct{}

func withInternalAuth(ctx context.Context, rec apiKeyRecord) context.Context {
	return context.WithValue(ctx, internalAuthKey{}, rec)
}

func internalAuthFrom(r *http.Request) (apiKeyRecord, bool) {
	rec, ok := r.Context().Value(internalAuthKey{}).(apiKeyRecord)
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

// requestAutoModel resolves the per-key "auto" (smart routing) model pool.
// When the client requests model="auto" and the authenticated key has an
// AutoModels pool configured, the highest-priority entry is returned so the
// gateway routes deterministically instead of delegating to the upstream
// magic tone. Models outside the pool can never be selected by "auto".
// Everything else is returned unchanged.
func requestAutoModel(r *http.Request, model string) string {
	if !strings.EqualFold(strings.TrimSpace(model), "auto") {
		return model
	}
	rec, ok := apiKeyFromRequest(r)
	if !ok || len(rec.AutoModels) == 0 {
		return model
	}
	for _, m := range rec.AutoModels {
		if v := strings.TrimSpace(m); v != "" && !strings.EqualFold(v, "auto") {
			return v
		}
	}
	return model
}
