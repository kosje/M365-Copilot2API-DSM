package web

import (
	"encoding/json"
	"m365-copilot2api/internal/auth"
	"net/http"
	"time"
)

// exportAccounts streams the whole account list as a downloadable
// accounts.json in the same format the import endpoint accepts, so an
// instance can be migrated through the web console without SSH.
//
// The store keeps refresh tokens decrypted in memory and only encrypts at
// save time, so the exported file contains plaintext tokens; importing into
// another instance re-encrypts them with that instance's master key, which
// keeps the file portable across instances.
func (s *Server) exportAccounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	list := s.tokens.List()
	out := make([]auth.AccountToken, 0, len(list))
	for _, a := range list {
		a.Status = "" // runtime state is not part of the migration format
		out = append(out, a)
	}
	b, err := json.MarshalIndent(struct {
		Accounts []auth.AccountToken `json:"accounts"`
	}{Accounts: out}, "", "  ")
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to serialize accounts")
		return
	}
	filename := "accounts-export-" + time.Now().Format("20060102-150405") + ".json"
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b)
}
