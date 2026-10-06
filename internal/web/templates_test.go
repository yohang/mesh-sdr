package web_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// templateRules are the template rules of ADR 0003 §3 (CSP nonce-only
// script-src, style-src 'self'). Breaking them fails silently in the browser
// (a CSP violation in the console), so they are enforced here.
var templateRules = []struct {
	name string
	re   *regexp.Regexp
}{
	{"hx-on attribute (eval, blocked by the CSP)", regexp.MustCompile(`\bhx-on\b|\bhx-on[:-]`)},
	{"js: expression (eval, blocked by the CSP)", regexp.MustCompile(`["'{\s,]js:`)},
	{"style attribute (blocked by style-src 'self')", regexp.MustCompile(`\sstyle=`)},
	{"<style> element (blocked by style-src 'self')", regexp.MustCompile(`<style[\s>]`)},
	{"inline <script> (use a module under /static/js or templ.JSONScript)", regexp.MustCompile(`<script(?:\s[^>]*)?>`)},
	{"templ css component", regexp.MustCompile(`(?m)^css\s+\w+\(`)},
	{"templ script component", regexp.MustCompile(`(?m)^script\s+\w+\(`)},
}

// externalScript is the only allowed <script> tag: an external file with the
// request nonce.
var externalScript = regexp.MustCompile(`^<script\s+src="/static/[^"]+"[^>]*\snonce=\{\s*templ\.GetNonce\(ctx\)\s*\}[^>]*>$`)

// violations returns the rule violations of a template source, as
// "line: rule: match". Rules match across lines (a tag may span several);
// // comment lines are ignored.
func violations(src string) []string {
	// Blank out comment lines, keeping offsets and line numbers.
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			lines[i] = strings.Repeat(" ", len(line))
		}
	}

	code := strings.Join(lines, "\n")

	var out []string

	for _, rule := range templateRules {
		for _, loc := range rule.re.FindAllStringIndex(code, -1) {
			m := code[loc[0]:loc[1]]
			if strings.HasPrefix(m, "<script") && externalScript.MatchString(m) {
				continue
			}

			line := strings.Count(code[:loc[0]], "\n") + 1
			out = append(out, fmt.Sprintf("%d: %s: %s", line, rule.name, strings.Join(strings.Fields(m), " ")))
		}
	}

	return out
}

func TestTemplateRules(t *testing.T) {
	var files int

	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".templ") {
			return err
		}

		files++

		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		for _, v := range violations(string(src)) {
			t.Errorf("%s:%s", path, v)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if files == 0 {
		t.Fatal("no .templ file found")
	}
}

func TestTemplateRulesDetect(t *testing.T) {
	bad := []struct {
		src  string
		line int
	}{
		{`<button hx-on:click="x()">`, 1},
		{`<div hx-vals='js:{a: 1}'>`, 1},
		{`<p style="color: red">`, 1},
		{`<style>p{}</style>`, 1},
		{`<script>alert(1)</script>`, 1},
		{`<script src="/static/js/x.js"></script>`, 1},
		{"css red() {", 1},
		{"script hello() {", 1},
		{"<div>\n\t<p\n\t\tclass=\"x\"\n\t\tstyle=\"color: red\"\n\t>", 4},
		{"<div>\n<script\n\ttype=\"module\"\n>alert(1)</script>", 2},
		{"<button\n\thx-on::after-request=\"x()\"\n>", 2},
	}

	for _, tt := range bad {
		v := violations(tt.src)
		if len(v) == 0 {
			t.Errorf("rules miss %q", tt.src)

			continue
		}

		if want := fmt.Sprintf("%d: ", tt.line); !strings.HasPrefix(v[0], want) {
			t.Errorf("%q: violation %q, want line %d", tt.src, v[0], tt.line)
		}
	}

	ok := []string{
		`<script src="/static/js/shell.js" type="module" nonce={ templ.GetNonce(ctx) }></script>`,
		"<script\n\tsrc=\"/static/vendor/htmx.min.js\"\n\tnonce={ templ.GetNonce(ctx) }\n\tdefer\n></script>",
		"// a comment about style=\"\" and hx-on",
	}

	for _, src := range ok {
		if v := violations(src); len(v) != 0 {
			t.Errorf("rules reject %q: %v", src, v)
		}
	}
}
