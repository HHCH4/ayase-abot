package httpapi

import (
	"net/http"
	"time"
)

// getArtifactUsage reports the local, de-duplicated artifact footprint together
// with the policy that is currently enforced. It exposes no object keys, user
// supplied paths or content.
func (s *Server) getArtifactUsage(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireArtifacts()
	if err != nil {
		writeError(writer, err)
		return
	}
	usage, err := service.Usage(request.Context())
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, usage)
}

// runArtifactMaintenance performs one bounded maintenance pass: close uploads
// that never completed, retry durable object deletions and collect objects that
// no artifact references any more. Stages whose repository or store extension
// is absent are no-ops, so an older embedder keeps working.
func (s *Server) runArtifactMaintenance(writer http.ResponseWriter, request *http.Request) {
	service, err := s.requireArtifacts()
	if err != nil {
		writeError(writer, err)
		return
	}
	result, err := service.RunMaintenance(request.Context(), time.Time{})
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}
