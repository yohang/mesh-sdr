package config

import (
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func TestParseDuration(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
		str  string
		ok   bool
	}{
		{"15s", 15 * time.Second, "15s", true},
		{"7d", 7 * 24 * time.Hour, "1w", true},
		{"1h30m", 90 * time.Minute, "1h30m", true},
		{"250ms", 250 * time.Millisecond, "250ms", true},
		{"2w", 14 * 24 * time.Hour, "2w", true},
		{"0s", 0, "0s", true},
		{"15", 0, "", false},
		{"1.5h", 0, "", false},
		{"-1s", 0, "", false},
		{"1 h", 0, "", false},
		{"", 0, "", false},
		{"99999999999999w", 0, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			d, err := ParseDuration(tt.in)
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v, ok = %v", err, tt.ok)
			}

			if tt.ok && (d.Duration() != tt.want || d.String() != tt.str) {
				t.Fatalf("got %v (%s), want %v (%s)", d.Duration(), d, tt.want, tt.str)
			}
		})
	}
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"32MiB", 32 << 20, true},
		{"1KiB", 1024, true},
		{"2GB", 2_000_000_000, true},
		{"1kB", 1000, true},
		{"512", 512, true},
		{"512B", 512, true},
		{"1.5MiB", 0, false},
		{"1mib", 0, false},
		{"-1", 0, false},
		{"99999999999TiB", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			s, err := ParseSize(tt.in)
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v, ok = %v", err, tt.ok)
			}

			if tt.ok && s.Bytes() != tt.want {
				t.Fatalf("got %d, want %d", s.Bytes(), tt.want)
			}
		})
	}

	if s := MustSize("32MiB").String(); s != "32MiB" {
		t.Errorf("String() = %q", s)
	}
}

func TestParseFrequency(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"145.800MHz", 145_800_000, true},
		{"7074000", 7_074_000, true},
		{"7.074MHz", 7_074_000, true},
		{"1.2GHz", 1_200_000_000, true},
		{"14kHz", 14_000, true},
		{"100Hz", 100, true},
		{"145.8000000MHz", 145_800_000, true},
		{"1.5Hz", 0, false},
		{"145.8000001MHz", 0, false},
		{"145.8 MHz", 0, false},
		{"-5", 0, false},
		{"MHz", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			f, err := ParseFrequency(tt.in)
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v, ok = %v", err, tt.ok)
			}

			if tt.ok && f.Hz() != tt.want {
				t.Fatalf("got %d, want %d", f.Hz(), tt.want)
			}
		})
	}
}

func TestUnitsFromTOML(t *testing.T) {
	var v struct {
		D  Duration  `toml:"d"`
		S  Size      `toml:"s"`
		SI Size      `toml:"si"`
		F  Frequency `toml:"f"`
		FI Frequency `toml:"fi"`
	}

	doc := `d = "7d"
s = "16MiB"
si = 1024
f = "145.800MHz"
fi = 7074000
`
	md, err := toml.Decode(doc, &v)
	if err != nil {
		t.Fatal(err)
	}

	if len(md.Undecoded()) > 0 {
		t.Fatalf("undecoded: %v", md.Undecoded())
	}

	if v.D.Duration() != 7*24*time.Hour || v.S.Bytes() != 16<<20 || v.SI.Bytes() != 1024 || v.F.Hz() != 145_800_000 || v.FI.Hz() != 7_074_000 {
		t.Fatalf("decoded %+v", v)
	}

	if _, err := toml.Decode(`d = 15`, &v); err == nil {
		t.Error("integer duration accepted")
	}
}
