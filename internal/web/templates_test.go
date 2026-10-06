package web_test

import (
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

		for i, line := range strings.Split(string(src), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}

			for _, rule := range templateRules {
				for _, m := range rule.re.FindAllString(line, -1) {
					if strings.HasPrefix(m, "<script") && externalScript.MatchString(m) {
						continue
					}

					t.Errorf("%s:%d: %s: %s", path, i+1, rule.name, strings.TrimSpace(line))
				}
			}
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
	bad := []string{
		`<button hx-on:click="x()">`,
		`<div hx-vals='js:{a: 1}'>`,
		`<p style="color: red">`,
		`<style>p{}</style>`,
		`<script>alert(1)</script>`,
		`<script src="/static/js/x.js"></script>`,
		"css red() {",
		"script hello() {",
	}

	for _, line := range bad {
		matched := false

		for _, rule := range templateRules {
			for _, m := range rule.re.FindAllString(line, -1) {
				if !strings.HasPrefix(m, "<script") || !externalScript.MatchString(m) {
					matched = true
				}
			}
		}

		if !matched {
			t.Errorf("rules miss %q", line)
		}
	}

	ok := `<script src="/static/js/shell.js" type="module" nonce={ templ.GetNonce(ctx) }>`
	if !externalScript.MatchString(ok) {
		t.Errorf("rules reject %q", ok)
	}
}
