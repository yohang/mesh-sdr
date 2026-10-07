package api

import (
	"context"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/yohang/mesh-sdr/internal/presets"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// PresetService is the preset use cases (ADR 0020).
type PresetService interface {
	List(ctx context.Context) ([]*presets.Preset, error)
	Get(ctx context.Context, id string) (*presets.Preset, error)
	Create(ctx context.Context, d presets.Draft) (*presets.Preset, error)
	Replace(ctx context.Context, id string, expectedVersion int, d presets.Draft) (presets.Replaced, error)
	Delete(ctx context.Context, id string) error
	Compatible(ctx context.Context, limits presets.DeviceLimits) ([]*presets.Preset, error)
}

// DeviceLimits reads the reported limits of a device of the registry.
type DeviceLimits interface {
	Limits(ctx context.Context, device string) (presets.DeviceLimits, bool, error)
}

// PresetHandlers serve /presets.
type PresetHandlers struct {
	presets PresetService
	devices DeviceLimits
}

// NewPresetHandlers returns the handlers.
func NewPresetHandlers(presets PresetService, devices DeviceLimits) PresetHandlers {
	return PresetHandlers{presets: presets, devices: devices}
}

func apiUUID(u shared.UUID) openapi_types.UUID {
	var out openapi_types.UUID
	copy(out[:], u.Bytes())

	return out
}

func presetDTO(p *presets.Preset) Preset {
	out := Preset{
		Id: apiUUID(p.ID()), Slug: p.Slug(), Name: p.Name(), Description: optString(p.Description()),
		Tags: p.Tags(), CenterFreq: p.CenterFreq(), SampRate: p.SampRate(), StartFreq: p.StartFreq(),
		StartMod: p.StartMod(), TuningStep: p.TuningStep(), SortOrder: p.SortOrder(), CreatedAt: p.CreatedAt(),
		UpdatedAt: p.UpdatedAt(), Version: p.Version(),
	}

	if v, ok := p.InitialSquelchLevel(); ok {
		out.InitialSquelchLevel = &v
	}

	if v, ok := p.InitialNRLevel(); ok {
		out.InitialNrLevel = &v
	}

	if w, ok := p.WaterfallLevels(); ok {
		out.WaterfallLevels = &WaterfallLevels{Min: w.Min(), Max: w.Max()}
	}

	return out
}

// presetDraft converts the fields shared by PresetInput and PresetReplace.
func presetDraft(slug *string, name string, desc *string, tags *[]string, center, rate int64, start *int64, mod *string,
	step *int64, squelch, nr *int, waterfall *WaterfallLevels,
) presets.Draft {
	d := presets.Draft{
		Name: name, CenterFreq: center, SampRate: rate, StartFreq: start, TuningStep: step,
		InitialSquelchLevel: squelch, InitialNRLevel: nr,
	}

	if slug != nil {
		d.Slug = *slug
	}

	if desc != nil {
		d.Description = *desc
	}

	if tags != nil {
		d.Tags = *tags
	}

	if mod != nil {
		d.StartMod = *mod
	}

	if waterfall != nil {
		d.WaterfallLevels = &[2]int{waterfall.Min, waterfall.Max}
	}

	return d
}

// ListPresets implements StrictServerInterface.
func (h PresetHandlers) ListPresets(ctx context.Context, req ListPresetsRequestObject) (ListPresetsResponseObject, error) {
	var (
		list []*presets.Preset
		err  error
	)

	if req.Params.DeviceId != nil {
		limits, ok, lerr := h.devices.Limits(ctx, *req.Params.DeviceId)

		switch {
		case lerr != nil:
			return nil, lerr
		case !ok:
			return nil, presets.ErrUnknownDevice
		}

		list, err = h.presets.Compatible(ctx, limits)
	} else {
		list, err = h.presets.List(ctx)
	}

	if err != nil {
		return nil, err
	}

	out := ListPresets200JSONResponse{Items: make([]Preset, 0, len(list))}
	for _, p := range list {
		out.Items = append(out.Items, presetDTO(p))
	}

	return out, nil
}

// CreatePreset implements StrictServerInterface.
func (h PresetHandlers) CreatePreset(ctx context.Context, req CreatePresetRequestObject) (CreatePresetResponseObject, error) {
	b := req.Body

	p, err := h.presets.Create(ctx, presetDraft(b.Slug, b.Name, b.Description, b.Tags, b.CenterFreq, b.SampRate, b.StartFreq,
		b.StartMod, b.TuningStep, b.InitialSquelchLevel, b.InitialNrLevel, b.WaterfallLevels))
	if err != nil {
		return nil, err
	}

	return CreatePreset201JSONResponse(presetDTO(p)), nil
}

// GetPreset implements StrictServerInterface.
func (h PresetHandlers) GetPreset(ctx context.Context, req GetPresetRequestObject) (GetPresetResponseObject, error) {
	p, err := h.presets.Get(ctx, req.Id)
	if err != nil {
		return nil, err
	}

	return GetPreset200JSONResponse(presetDTO(p)), nil
}

// ReplacePreset implements StrictServerInterface.
func (h PresetHandlers) ReplacePreset(ctx context.Context, req ReplacePresetRequestObject) (ReplacePresetResponseObject, error) {
	b := req.Body

	r, err := h.presets.Replace(ctx, req.Id, b.Version, presetDraft(b.Slug, b.Name, b.Description, b.Tags, b.CenterFreq, b.SampRate,
		b.StartFreq, b.StartMod, b.TuningStep, b.InitialSquelchLevel, b.InitialNrLevel, b.WaterfallLevels))
	if err != nil {
		return nil, err
	}

	out := ReplacePreset200JSONResponse{Preset: presetDTO(r.Preset), DisabledSchedules: make([]openapi_types.UUID, 0, len(r.Disabled))}
	for _, id := range r.Disabled {
		out.DisabledSchedules = append(out.DisabledSchedules, apiUUID(id))
	}

	return out, nil
}

// DeletePreset implements StrictServerInterface.
func (h PresetHandlers) DeletePreset(ctx context.Context, req DeletePresetRequestObject) (DeletePresetResponseObject, error) {
	if err := h.presets.Delete(ctx, req.Id); err != nil {
		return nil, err
	}

	return DeletePreset204Response{}, nil
}
