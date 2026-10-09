package api

import (
	"context"
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

	res := jsonOK{v: doc, indent: true}
	if req.Params.Download != nil && *req.Params.Download {
		res.download = "meshsdr-config-" + doc.GeneratedAt.Format(time.DateOnly) + ".json"
	}

	return res, nil
}

func (j jsonOK) VisitGetEffectiveConfigResponse(w http.ResponseWriter) error { return j.write(w) }
