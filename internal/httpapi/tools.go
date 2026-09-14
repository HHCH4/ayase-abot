package httpapi

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"Abot/internal/agent"
)

func (s *Server) listTools(writer http.ResponseWriter, request *http.Request) {
	if s.toolRegistry == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"tools": []agent.ToolDescriptor{}})
		return
	}
	query := strings.TrimSpace(request.URL.Query().Get("q"))
	source := agent.ToolSource(strings.TrimSpace(request.URL.Query().Get("source")))
	risk := agent.ToolRiskLevel(strings.TrimSpace(request.URL.Query().Get("risk")))
	capabilities := make([]agent.ToolCapability, 0)
	for _, value := range request.URL.Query()["capability"] {
		for _, item := range strings.Split(value, ",") {
			if item = strings.TrimSpace(item); item != "" {
				capabilities = append(capabilities, agent.ToolCapability(item))
			}
		}
	}
	limit := 100
	if value := strings.TrimSpace(request.URL.Query().Get("limit")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 200 {
			writeError(writer, agent.ErrInvalidToolDescriptor)
			return
		}
		limit = parsed
	}
	var items []agent.ToolDescriptor
	if query == "" && source == "" && risk == "" && len(capabilities) == 0 {
		items = s.toolRegistry.List()
	} else {
		items = s.toolRegistry.Search(query, capabilities, source, risk, limit)
	}
	public := make([]agent.ToolDescriptor, 0, len(items))
	for _, item := range items {
		if !item.Public {
			continue
		}
		public = append(public, item)
		if len(public) >= limit {
			break
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"tools": public, "catalog_revision": s.toolRegistry.CatalogRevision()})
}

func (s *Server) getTool(writer http.ResponseWriter, request *http.Request) {
	if s.toolRegistry == nil {
		writeError(writer, agent.ErrToolNotFound)
		return
	}
	items := s.toolRegistry.Find(request.PathValue("id"))
	public := items[:0]
	for _, item := range items {
		if item.Public {
			public = append(public, item)
		}
	}
	if version := strings.TrimSpace(request.URL.Query().Get("version")); version != "" {
		for _, item := range public {
			if item.Version == version {
				writeJSON(writer, http.StatusOK, item)
				return
			}
		}
		writeError(writer, agent.ErrToolNotFound)
		return
	}
	if len(public) == 0 {
		writeError(writer, agent.ErrToolNotFound)
		return
	}
	sort.Slice(public, func(i, j int) bool { return public[i].Version > public[j].Version })
	writeJSON(writer, http.StatusOK, public[0])
}
