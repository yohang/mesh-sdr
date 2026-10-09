package config

import "testing"

// The slot decoder settings accept only the valid periods, combinations
// and speeds (DEC-021…024, DEC-029).
func TestDecoderSlotSettings(t *testing.T) {
	for _, tc := range []struct {
		key, value string
		ok         bool
	}{
		{"decoders.fst4_enabled_intervals", `["15","1800"]`, true},
		{"decoders.fst4_enabled_intervals", `["45"]`, false},
		{"decoders.fst4_enabled_intervals", `[]`, false},
		{"decoders.fst4w_enabled_intervals", `["15"]`, false},
		{"decoders.q65_enabled_combinations", `["A30","E300"]`, true},
		{"decoders.q65_enabled_combinations", `["E15"]`, false},
		{"decoders.js8_enabled_profiles", `["turbo"]`, true},
		{"decoders.js8_enabled_profiles", `["warp"]`, false},
		{"decoders.wsjt_decoding_depth", `3`, true},
		{"decoders.wsjt_decoding_depth", `0`, false},
		{"decoders.wsjt_decoding_depths.jt65", `0`, true},
		{"decoders.wsjt_decoding_depths.jt65", `4`, false},
		{"decoders.js8_decoding_depth", `4`, false},
	} {
		if _, vs, err := DecodeSetting(tc.key, []byte(tc.value)); err != nil || (len(vs) == 0) != tc.ok {
			t.Errorf("%s = %s: %v %v", tc.key, tc.value, vs, err)
		}
	}
}
