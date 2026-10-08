package decoder

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/shared/process"
)

// The ISM and Wireless M-Bus decoders (DEC-039, DEC-040): rtl_433 reads
// the wide IQ tap as cf32 (250 kHz, or 1.2 MS/s with only its Wireless
// M-Bus decoders) and prints one JSON object per message. -M level adds
// the signal levels (kept in the payload when the ism_report_levels
// setting is on) and -M stats its periodic statistics (not messages).

// ISMSchema names the ism.v1 payload: the rtl_433 object, in its order,
// after the mode (ISM or WMBUS).
const ISMSchema = "ism.v1"

// rtl433Args are the arguments of every rtl_433 session.
func rtl433Args(rate int) []string {
	return []string{"-r", "cf32:-", "-s", strconv.Itoa(rate), "-F", "json", "-M", "time:unix", "-M", "level", "-M", "stats", "-Y", "autolevel"}
}

// ismArgs runs rtl_433 with its default decoders at 250 kHz.
func ismArgs(sessionConfig) []string { return rtl433Args(250_000) }

// wmbusArgs runs rtl_433 at 1.2 MS/s with the Wireless M-Bus decoders
// only (modes C&T, S, T, R, F of rtl_433 25.02).
func wmbusArgs(sessionConfig) []string {
	return append(rtl433Args(1_200_000), "-R", "104", "-R", "105", "-R", "106", "-R", "107", "-R", "238")
}

// rtl433Rules classify the stderr of rtl_433.
var rtl433Rules = []process.Rule{
	{Pattern: regexp.MustCompile(`(?i)(unknown|invalid|bad) (option|argument|protocol)|^usage`), Class: process.ClassFatalConfig},
	{Pattern: regexp.MustCompile(`(?i)(failed|unable) to (open|read)`), Class: process.ClassInputError},
	{Pattern: regexp.MustCompile(`.`), Class: process.ClassInfo},
}

// levelKeys are the -M level fields.
var levelKeys = []string{"rssi", "snr", "noise"}

// textSkip are the fields left out of the text rendering.
var textSkip = []string{"time", "model", "mic", "mod", "rssi", "snr", "noise", "freq", "freq1", "freq2"}

// newISMParser returns the parser factory of an rtl_433 mode.
func newISMParser(mode string) func(sessionConfig) lineParser {
	return func(c sessionConfig) lineParser {
		levels := c.settings.ISMReportLevels

		return func(line string, at time.Time) []app.DecodeRecord {
			rec, ok := parseISM(line, mode, levels, at)
			if !ok {
				return nil
			}

			return []app.DecodeRecord{rec}
		}
	}
}

// jsonField is one member of an rtl_433 object.
type jsonField struct {
	key   string
	value json.RawMessage
}

// parseISM turns an rtl_433 JSON line into an ism.v1 record; statistics
// and other lines without a model are skipped.
func parseISM(line, mode string, levels bool, at time.Time) (app.DecodeRecord, bool) {
	// Lines are capped at 4 KiB by the stdout reader: a longer message is
	// cut and fails to parse.
	if !strings.HasPrefix(line, "{") {
		return app.DecodeRecord{}, false
	}

	fields, ok := jsonFields(line)
	if !ok {
		return app.DecodeRecord{}, false
	}

	var model string

	i := slices.IndexFunc(fields, func(f jsonField) bool { return f.key == "model" })
	if i < 0 || json.Unmarshal(fields[i].value, &model) != nil || model == "" {
		return app.DecodeRecord{}, false
	}

	var payload, text bytes.Buffer

	payload.WriteString(`{"mode":`)
	m, _ := json.Marshal(mode)
	payload.Write(m)

	text.WriteString(model)

	for _, f := range fields {
		if f.key == "mode" || (!levels && slices.Contains(levelKeys, f.key)) {
			continue
		}

		k, _ := json.Marshal(cleanRF(f.key))
		payload.WriteByte(',')
		payload.Write(k)
		payload.WriteByte(':')
		payload.Write(f.value)

		if !slices.Contains(textSkip, f.key) {
			text.WriteString(" " + f.key + "=" + scalar(f.value))
		}
	}

	payload.WriteByte('}')

	return app.DecodeRecord{Time: at, Schema: ISMSchema, Text: cleanRF(text.String()), Payload: payload.Bytes()}, true
}

// jsonFields reads the members of a JSON object in order.
func jsonFields(s string) ([]jsonField, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()

	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}

	var out []jsonField

	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, false
		}

		key, ok := t.(string)
		if !ok {
			return nil, false
		}

		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}

		// RF-derived strings are cleaned (§8.4 rule 4).
		raw, err := json.Marshal(cleanValue(v))
		if err != nil {
			return nil, false
		}

		out = append(out, jsonField{key: key, value: raw})
	}

	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, false
	}

	return out, true
}

// cleanValue cleans the strings of a decoded JSON value.
func cleanValue(v any) any {
	switch x := v.(type) {
	case string:
		return cleanRF(x)
	case []any:
		for i := range x {
			x[i] = cleanValue(x[i])
		}
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[cleanRF(k)] = cleanValue(e)
		}

		return out
	}

	return v
}

// scalar renders a value: a string without its quotes, anything else as
// JSON.
func scalar(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}

	return string(v)
}
