package shell

import (
	"strings"
	"testing"
)

func TestMarkdown(t *testing.T) {
	tests := []struct {
		name, src string
		want      []string
		banned    []string
	}{
		{"headings demoted", "# Title\n\n## Sub\n\n###### Deep", []string{"<h2>Title</h2>", "<h3>Sub</h3>", "<h6>Deep</h6>"}, []string{"<h1"}},
		{"h2 top level kept", "## A\n\n### B", []string{"<h2>A</h2>", "<h3>B</h3>"}, []string{"<h1"}},
		{"lists and links", "- one\n- [site](https://example.org)", []string{"<li>one</li>", `<a href="https://example.org">site</a>`}, nil},
		{"raw HTML dropped", "<script>alert(1)</script>\n\nhi <b onclick=\"x()\">b</b>", []string{"hi "}, []string{"<script", "onclick", "<b"}},
		{"dangerous link dropped", "[x](javascript:alert(1)) [y](vbscript:z)", nil, []string{"javascript:", "vbscript:"}},
		{"text escaped", "a < b & c", []string{"a &lt; b &amp; c"}, nil},
	}

	md := newMarkdown()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := md.HTML(tt.src)
			if err != nil {
				t.Fatal(err)
			}

			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}

			for _, b := range tt.banned {
				if strings.Contains(out, b) {
					t.Errorf("output contains %q:\n%s", b, out)
				}
			}
		})
	}
}
