-- name: GetPreset :one
SELECT * FROM presets WHERE id = ?;

-- name: ListPresets :many
SELECT * FROM presets ORDER BY sort_order, name, id;

-- name: PresetSlugTaken :one
SELECT EXISTS (SELECT 1 FROM presets WHERE slug = ? AND id <> ?);

-- name: NextPresetSortOrder :one
SELECT CAST(COALESCE(MAX(sort_order) + 1, 0) AS INTEGER) FROM presets;

-- name: InsertPreset :exec
INSERT INTO presets (
    id, slug, name, description, tags, center_freq, samp_rate, start_freq, start_mod, tuning_step,
    initial_squelch_level, initial_nr_level, waterfall_levels, sort_order, created_at, updated_at, version
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdatePreset :execrows
UPDATE presets SET
    slug = sqlc.arg(slug), name = sqlc.arg(name), description = sqlc.arg(description), tags = sqlc.arg(tags),
    center_freq = sqlc.arg(center_freq), samp_rate = sqlc.arg(samp_rate), start_freq = sqlc.arg(start_freq),
    start_mod = sqlc.arg(start_mod), tuning_step = sqlc.arg(tuning_step),
    initial_squelch_level = sqlc.arg(initial_squelch_level), initial_nr_level = sqlc.arg(initial_nr_level),
    waterfall_levels = sqlc.arg(waterfall_levels), sort_order = sqlc.arg(sort_order), updated_at = sqlc.arg(updated_at),
    version = sqlc.arg(version)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(expected_version);

-- name: DeletePreset :execrows
DELETE FROM presets WHERE id = ?;
