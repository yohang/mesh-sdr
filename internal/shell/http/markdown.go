package http

import (
	"bytes"
	"fmt"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// markdown renders admin-set Markdown (CommonMark) to HTML. goldmark's default
// renderer drops raw HTML and dangerous link URLs (javascript:, vbscript:,
// file:, data: except images), so the output is safe to embed as is.
// The page owns the h1: headings are shifted so the top-level one is an h2.
type markdown struct {
	md goldmark.Markdown
}

func newMarkdown() *markdown {
	return &markdown{md: goldmark.New(
		goldmark.WithParserOptions(parser.WithASTTransformers(util.Prioritized(demoteHeadings{}, 100))),
	)}
}

// HTML converts src.
func (m *markdown) HTML(src string) (string, error) {
	var buf bytes.Buffer

	if err := m.md.Convert([]byte(src), &buf); err != nil {
		return "", fmt.Errorf("render markdown: %w", err)
	}

	return buf.String(), nil
}

type demoteHeadings struct{}

func (demoteHeadings) Transform(doc *ast.Document, _ text.Reader, _ parser.Context) {
	var headings []*ast.Heading

	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if h, ok := n.(*ast.Heading); ok && entering {
			headings = append(headings, h)
		}

		return ast.WalkContinue, nil
	})

	top := 6
	for _, h := range headings {
		top = min(top, h.Level)
	}

	if top >= 2 {
		return
	}

	for _, h := range headings {
		h.Level = min(h.Level+2-top, 6)
	}
}
