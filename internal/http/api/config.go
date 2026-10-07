package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/yohang/mesh-sdr/internal/settings"
)

// ConfigViewer returns the effective configuration (ADM-010).
type ConfigViewer interface {
	View() settings.ConfigView
}

// ConfigHandlers serve /config/effective (ADM-010): the download of
// Admin › System.
type ConfigHandlers struct {
	config ConfigViewer
}

// NewConfigHandlers returns the handlers.
func NewConfigHandlers(config ConfigViewer) ConfigHandlers {
	return ConfigHandlers{config: config}
}

// GetEffectiveConfig implements StrictServerInterface.
func (h ConfigHandlers) GetEffectiveConfig(_ context.Context, req GetEffectiveConfigRequestObject) (GetEffectiveConfigResponseObject, error) {
	v := h.config.View()
	doc := EffectiveConfig{GeneratedAt: v.GeneratedAt, Revision: v.Revision, Entries: make([]ConfigEntry, 0, len(v.Entries))}

	for _, e := range v.Entries {
		c := ConfigEntry{
			Key: e.Key, Class: ConfigEntryClass(e.Class), Source: ConfigEntrySource(e.Source),
			Origin: e.Origin, Locked: e.Locked, Secret: e.Secret,
		}

		if e.Secret {
			set := e.Set
			c.Set = &set
		} else if e.Value != nil {
			c.Value = e.Value
		}

		doc.Entries = append(doc.Entries, c)
	}

	download := req.Params.Download != nil && *req.Params.Download

	return effectiveConfigResponse{doc: doc, download: download}, nil
}

type effectiveConfigResponse struct {
	doc      EffectiveConfig
	download bool
}

func (r effectiveConfigResponse) VisitGetEffectiveConfigResponse(w http.ResponseWriter) error {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")

	if r.download {
		w.Header().Set("Content-Disposition",
			`attachment; filename="meshsdr-config-`+r.doc.GeneratedAt.Format(time.DateOnly)+`.json"`)
	}

	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(r.doc)
}
