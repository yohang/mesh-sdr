package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestKeyLines(t *testing.T) {
	doc := `# comment
schema_version = 1 # trailing

[hub]
url = "https://x" # url
"quoted key" = 'lit'
multi = """
line = 2
"""
lit = '''
[not.a.table]
'''

[db]
dsn = "sqlite:///x.db"
backup = { schedule = "daily", keep = 7, nested = { a = 1 } }
list = [
  "a", # c
  { b = 1 },
]
a.b.c = true

[[nodes]]
name = "x"

[ dotted . "tbl" ]
k = 1
esc = "a\"b = c"
`

	if _, err := toml.Decode(doc, &map[string]any{}); err != nil {
		t.Fatalf("test document is not valid TOML: %v", err)
	}

	want := map[string]int{
		"schema_version":     2,
		"hub":                4,
		"hub.url":            5,
		"hub.quoted key":     6,
		"hub.multi":          7,
		"hub.lit":            10,
		"db":                 14,
		"db.dsn":             15,
		"db.backup":          16,
		"db.backup.schedule": 16,
		"db.backup.keep":     16,
		"db.backup.nested.a": 16,
		"db.list":            17,
		"db.a.b.c":           21,
		"nodes":              23,
		"nodes.name":         24,
		"dotted.tbl":         26,
		"dotted.tbl.k":       27,
		"dotted.tbl.esc":     28,
	}

	got := keyLines(doc)
	for k, line := range want {
		if got[k] != line {
			t.Errorf("%s: line %d, want %d", k, got[k], line)
		}
	}

	for k := range got {
		if strings.HasPrefix(k, "not") || strings.HasPrefix(k, "line") {
			t.Errorf("key %q found inside a string", k)
		}
	}
}

func TestKeyLinesDatesAndEscapedKeys(t *testing.T) {
	doc := `odt = 1979-05-27 07:32:00Z
after = 1
ldt = 1979-05-27T07:32:00
date = 1979-05-27
inline = { at = 1979-05-27 07:32:00-07:00, n = 2 }
"a\"b" = 1
"café" = 2
"tab\there" = 3
[tbl]
"x\\y" = 4
`

	var decoded map[string]any

	md, err := toml.Decode(doc, &decoded)
	if err != nil {
		t.Fatalf("test document is not valid TOML: %v", err)
	}

	want := map[string]int{
		"odt":       1,
		"after":     2,
		"ldt":       3,
		"date":      4,
		"inline":    5,
		"inline.at": 5,
		"inline.n":  5,
		`a"b`:       6,
		"café":      7,
		"tab\there": 8,
		"tbl":       9,
		`tbl.x\y`:   10,
	}

	got := keyLines(doc)
	for k, line := range want {
		if got[k] != line {
			t.Errorf("%q: line %d, want %d", k, got[k], line)
		}
	}

	// Every key the decoder reports is found.
	for _, k := range md.Keys() {
		if key := strings.Join(k, "."); got[key] == 0 {
			t.Errorf("decoder key %q not found by the scanner", key)
		}
	}
}

func TestKeyLinesMalformed(t *testing.T) {
	// Must not hang or panic.
	for _, doc := range []string{"[", "a = ", "a = \"unterminated", "= 1", "a = [1, ", "a = {", `a = """x`} {
		_ = keyLines(doc)
	}
}
