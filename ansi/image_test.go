package ansi

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
)

func TestImageProtocol(t *testing.T) {
	tests := []struct {
		name     string
		protocol ImageProtocol
	}{
		{"kitty", ImageProtocolKitty},
		{"sixel", ImageProtocolSixel},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			imgPath, err := filepath.Abs(filepath.Join("testdata", "TestImageProtocol", "test.png"))
			if err != nil {
				t.Fatal(err)
			}

			b, err := os.ReadFile("../styles/dark.json")
			if err != nil {
				t.Fatal(err)
			}

			options := Options{
				WordWrap:      80,
				ImageProtocol: tc.protocol,
			}
			if err := json.Unmarshal(b, &options.Styles); err != nil {
				t.Fatal(err)
			}

			md := goldmark.New(
				goldmark.WithExtensions(
					extension.GFM,
					extension.DefinitionList,
				),
				goldmark.WithParserOptions(
					parser.WithAutoHeadingID(),
				),
			)

			ar := NewRenderer(options)
			md.SetRenderer(
				renderer.NewRenderer(
					renderer.WithNodeRenderers(util.Prioritized(ar, 1000))))

			in := "![Test Image](" + imgPath + ")"
			var buf bytes.Buffer
			if err := md.Convert([]byte(in), &buf); err != nil {
				t.Fatal(err)
			}

			out := buf.String()
			if tc.protocol == ImageProtocolKitty && !bytes.Contains(buf.Bytes(), []byte("\x1b_G")) {
				t.Errorf("expected kitty graphics sequence, got: %q", out)
			}
			if tc.protocol == ImageProtocolSixel && !bytes.Contains(buf.Bytes(), []byte("\x1bP")) {
				t.Errorf("expected sixel graphics sequence, got: %q", out)
			}
		})
	}
}
