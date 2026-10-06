package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"time"

	settingsapp "github.com/yohang/mesh-sdr/internal/settings/app"
	settingsdomain "github.com/yohang/mesh-sdr/internal/settings/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// SettingsStore is the settings store used by the settings endpoints.
type SettingsStore interface {
	Snapshot() *settingsapp.Snapshot
	Schema() json.RawMessage
	Apply(ctx context.Context, actor settingsapp.Actor, set settingsdomain.ChangeSet) (*settingsapp.Snapshot, error)
}

// ConfigViewer returns the effective configuration (ADM-010).
type ConfigViewer interface {
	View() settingsapp.ConfigView
}

// ActorFunc returns who makes the request (the signed-in admin).
type ActorFunc func(ctx context.Context) settingsapp.Actor

// SettingsHandlers serve /settings and /config/effective (ADM-002, ADM-010).
type SettingsHandlers struct {
	store  SettingsStore
	config ConfigViewer
	actor  ActorFunc
}

// NewSettingsHandlers returns the handlers.
func NewSettingsHandlers(store SettingsStore, config ConfigViewer, actor ActorFunc) SettingsHandlers {
	return SettingsHandlers{store: store, config: config, actor: actor}
}

// GetSettings implements StrictServerInterface.
func (h SettingsHandlers) GetSettings(context.Context, GetSettingsRequestObject) (GetSettingsResponseObject, error) {
	return settingsResponse(h.store.Snapshot()), nil
}

// PatchSettings implements StrictServerInterface.
func (h SettingsHandlers) PatchSettings(ctx context.Context, req PatchSettingsRequestObject) (PatchSettingsResponseObject, error) {
	versions := map[string]int64{}
	if req.Body.Versions != nil {
		versions = *req.Body.Versions
	}

	keys := make([]string, 0, len(req.Body.Values))
	for k := range req.Body.Values {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	var (
		changes    []settingsdomain.Change
		violations []shared.Violation
	)

	for _, k := range keys {
		key, err := settingsdomain.NewKey(k)
		if err != nil {
			violations = append(violations, shared.NewViolation(k, settingsdomain.ErrUnknownSetting.Code(), "unknown setting"))

			continue
		}

		raw := req.Body.Values[k]
		if raw == nil {
			changes = append(changes, settingsdomain.ResetTo(key, versions[k]))

			continue
		}

		v, err := settingsdomain.ValueOf(raw)
		if err != nil {
			violations = append(violations, shared.NewViolation(k, "invalid_value", "invalid JSON value"))

			continue
		}

		changes = append(changes, settingsdomain.SetTo(key, v, versions[k]))
	}

	if len(violations) > 0 {
		return nil, settingsdomain.ErrInvalidSetting.WithViolations(violations...)
	}

	set, err := settingsdomain.NewChangeSet(changes...)
	if err != nil {
		return nil, err
	}

	snap, err := h.store.Apply(ctx, h.actor(ctx), set)
	if err != nil {
		return nil, err
	}

	return settingsResponse(snap), nil
}

// DeleteSetting implements StrictServerInterface.
func (h SettingsHandlers) DeleteSetting(ctx context.Context, req DeleteSettingRequestObject) (DeleteSettingResponseObject, error) {
	key, err := settingsdomain.NewKey(req.Key)
	if err != nil {
		return nil, settingsdomain.ErrUnknownSetting.WithDetail("unknown setting " + strconv.Quote(req.Key))
	}

	var version int64

	if req.Params.Version != nil {
		version = *req.Params.Version
	} else if e, ok := h.store.Snapshot().Get(key.String()); ok {
		version = e.Version()
	}

	set, err := settingsdomain.NewChangeSet(settingsdomain.ResetTo(key, version))
	if err != nil {
		return nil, err
	}

	if _, err := h.store.Apply(ctx, h.actor(ctx), set); err != nil {
		return nil, err
	}

	return DeleteSetting204Response{}, nil
}

// GetSettingsSchema implements StrictServerInterface.
func (h SettingsHandlers) GetSettingsSchema(context.Context, GetSettingsSchemaRequestObject) (GetSettingsSchemaResponseObject, error) {
	return rawJSONDocument(h.store.Schema()), nil
}

// GetPublicSettings implements StrictServerInterface.
func (h SettingsHandlers) GetPublicSettings(context.Context, GetPublicSettingsRequestObject) (GetPublicSettingsResponseObject, error) {
	out := GetPublicSettings200JSONResponse{}

	for _, e := range h.store.Snapshot().All() {
		if d := e.Definition(); d.Public() && !d.Secret() {
			v := any(e.Value().JSON())
			out[e.Key().String()] = &v
		}
	}

	return out, nil
}

// GetEffectiveConfig implements StrictServerInterface.
func (h SettingsHandlers) GetEffectiveConfig(_ context.Context, req GetEffectiveConfigRequestObject) (GetEffectiveConfigResponseObject, error) {
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

// SettingEntries converts a snapshot to the API representation; secrets
// expose only whether they are set.
func SettingEntries(snap *settingsapp.Snapshot) []Setting {
	all := snap.All()
	out := make([]Setting, 0, len(all))

	for _, e := range all {
		d := e.Definition()
		s := Setting{
			Key: e.Key().String(), Source: SettingSource(e.Source()), Origin: e.Origin(), Locked: e.Locked(),
			Version: e.Version(), Secret: d.Secret(), Public: d.Public(), Apply: SettingApply(d.Apply()), Label: d.Label(),
		}

		if desc := d.Description(); desc != "" {
			s.Description = &desc
		}

		if d.Secret() {
			set := e.IsSet()
			s.Set = &set
		} else {
			s.Value = e.Value().JSON()
			s.Default = d.Default().JSON()

			if v, ok := e.Shadowed(); ok {
				s.ShadowedValue = v.JSON()
			}
		}

		out = append(out, s)
	}

	return out
}

// settingsList is the settings list with the revision as ETag.
type settingsList struct{ body SettingsList }

func settingsResponse(snap *settingsapp.Snapshot) settingsList {
	return settingsList{body: SettingsList{Revision: snap.Revision(), Settings: SettingEntries(snap)}}
}

func (s settingsList) write(w http.ResponseWriter) error {
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(s.body.Revision, 10)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	return json.NewEncoder(w).Encode(s.body)
}

func (s settingsList) VisitGetSettingsResponse(w http.ResponseWriter) error   { return s.write(w) }
func (s settingsList) VisitPatchSettingsResponse(w http.ResponseWriter) error { return s.write(w) }

// rawJSONDocument writes a pre-encoded JSON document.
type rawJSONDocument json.RawMessage

func (b rawJSONDocument) VisitGetSettingsSchemaResponse(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, err := w.Write(b)

	return err
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
