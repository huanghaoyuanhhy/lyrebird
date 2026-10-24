package esserver

import "net/http"

// Index-level DDL routes. Lyrebird's collections come from the control
// plane, so creating/deleting an index and changing its mapping or settings
// cannot be honored (docs/design.md, write path: DDL is Phase 4b). The calls
// answer by name instead of falling through to the 404 catch-all, the way
// _bulk does: a client learns the capability is missing, not the route.

// installDDLRoutes registers the schema-management endpoints ES clients send
// at bootstrap and after schema changes.
func (s *Server) installDDLRoutes() {
	refuse := func(what string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			writeErrorEnvelope(w, http.StatusBadRequest, "unsupported_exception",
				what+" is not supported: collections are created through lyrebird's control plane, not through index DDL (Phase 4b)", "")
		}
	}
	s.mux.HandleFunc("PUT /{index}", refuse("index creation"))
	s.mux.HandleFunc("DELETE /{index}", refuse("index deletion"))
	s.mux.HandleFunc("PUT /{index}/_mapping", refuse("mapping updates"))
	s.mux.HandleFunc("PUT /{index}/_settings", refuse("settings updates"))
}
