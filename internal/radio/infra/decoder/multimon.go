package decoder

import (
	"encoding/json"
	"regexp"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/app"
	"github.com/yohang/mesh-sdr/internal/shared/process"
)

// SelCall decoders of multimon-ng (DEC-034, DEC-035).
var (
	selcallDecoders = []string{"DTMF", "EEA", "EIA", "CCIR"}
	zveiDecoders    = []string{"ZVEI1", "ZVEI2", "ZVEI3", "DZVEI", "PZVEI"}
)

// multimonArgv runs one multimon-ng per decoder, each reading raw 16-bit
// signed samples at 22 050 Hz from stdin (-c removes the default set):
// decoders of one process write their digits in pieces as they come, so
// two decoders hearing the same tones would mix them on one line.
func multimonArgv(decoders []string) [][]string {
	out := make([][]string, 0, len(decoders))
	for _, d := range decoders {
		out = append(out, []string{"-c", "-v", "0", "-t", "raw", "-a", d, "-"})
	}

	return out
}

// multimonRules classify the stderr of multimon-ng.
var multimonRules = []process.Rule{
	{Pattern: regexp.MustCompile(`(?i)(invalid option|unknown (demodulator|option)|usage:)`), Class: process.ClassFatalConfig},
	{Pattern: regexp.MustCompile(`(?i)(cannot|can't|unable to) (open|read)`), Class: process.ClassInputError},
	{Pattern: regexp.MustCompile(`(?i)^(multimon-ng|\(c\)|available demodulators|enabled demodulators)`), Class: process.ClassInfo},
	{Pattern: regexp.MustCompile(`.`), Class: process.ClassInfo},
}

// SelCallRecord is the selcall.v1 payload: the decoder that recognised a
// tone sequence and its digits.
type SelCallRecord struct {
	Decoder string `json:"decoder"`
	Digits  string `json:"digits"`
}

// SelCallSchema names the selcall.v1 payload.
const SelCallSchema = "selcall.v1"

var selcallLine = regexp.MustCompile(`^(DTMF|EEA|EIA|CCIR|ZVEI1|ZVEI2|ZVEI3|DZVEI|PZVEI):\s*([0-9A-Fa-f*#]{1,64})\s*$`)

// parseSelCall turns a multimon-ng line ("ZVEI1: 12345") into a record
// rendered "[ZVEI1] 12345" (DEC-034). Other lines are ignored.
func parseSelCall(line string, at time.Time) (app.DecodeRecord, bool) {
	m := selcallLine.FindStringSubmatch(line)
	if m == nil {
		return app.DecodeRecord{}, false
	}

	payload, err := json.Marshal(SelCallRecord{Decoder: m[1], Digits: m[2]})
	if err != nil {
		return app.DecodeRecord{}, false
	}

	return app.DecodeRecord{Time: at, Schema: SelCallSchema, Text: "[" + m[1] + "] " + m[2], Payload: payload}, true
}
