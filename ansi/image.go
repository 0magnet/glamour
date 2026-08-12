package ansi

import (
	"bytes"
	"context"
	"fmt"
	"hash/fnv"
	"image"

	// Register image formats for decoding.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi/kitty"
	"github.com/charmbracelet/x/ansi/sixel"
)

const (
	// approxTerminalCellAspect is the approximate height of a terminal cell
	// relative to its width. Used to calculate the display height of an image
	// when its width is constrained to the word wrap width.
	approxTerminalCellAspect = 0.5

	// httpClientTimeout is the timeout used for fetching remote images.
	httpClientTimeout = 30 * time.Second
)

var httpClient = &http.Client{
	Timeout: httpClientTimeout,
}

// imageCache is an in-memory cache of decoded images keyed by URL. It avoids
// re-fetching and re-decoding remote images on every render, which is
// important for interactive applications (e.g. a TUI pager) that re-render
// the same document repeatedly.
var imageCache = struct {
	sync.Mutex
	m map[string]image.Image
}{m: make(map[string]image.Image)}

// sequenceCacheKey uniquely identifies an encoded graphics sequence.
type sequenceCacheKey struct {
	url      string
	protocol ImageProtocol
	cols     int
	rows     int
}

// sequenceCache is an in-memory cache of encoded graphics protocol sequences.
// Encoding an image (especially PNG-encoding for Kitty) is expensive, so
// caching the resulting escape sequence avoids re-encoding on every render
// in interactive applications that re-render the same document repeatedly.
var sequenceCache = struct {
	sync.Mutex
	m map[sequenceCacheKey]string
}{m: make(map[sequenceCacheKey]string)}

// htmlImgRegex matches <img> tags and captures the src attribute.
var htmlImgRegex = regexp.MustCompile(`<img[^>]+src=["']([^"']+)["'][^>]*>`)

// An ImageElement is used to render images elements.
type ImageElement struct {
	Text     string
	BaseURL  string
	URL      string
	Child    ElementRenderer
	TextOnly bool
}

// Render renders an ImageElement.
func (e *ImageElement) Render(w io.Writer, ctx RenderContext) error {
	// Make OSC 8 hyperlink token.
	hyperlink, resetHyperlink, _ := makeHyperlink(e.URL)

	style := ctx.options.Styles.ImageText
	if e.TextOnly {
		style.Format = strings.TrimSuffix(style.Format, " →")
	}

	if len(e.Text) > 0 {
		token := hyperlink + e.Text + resetHyperlink
		el := &BaseElement{
			Token: token,
			Style: style,
		}
		err := el.Render(w, ctx)
		if err != nil {
			return err
		}
	}

	if e.TextOnly || len(e.URL) == 0 {
		return nil
	}

	url := resolveRelativeURL(e.BaseURL, e.URL)
	token := hyperlink + url + resetHyperlink
	el := &BaseElement{
		Token:  token,
		Prefix: " ",
		Style:  ctx.options.Styles.Image,
	}
	err := el.Render(w, ctx)
	if err != nil {
		return err
	}

	if ctx.options.ImageProtocol != ImageProtocolNone {
		seq, err := e.graphicsSequence(ctx, url)
		if err != nil {
			// Silently skip images that fail to load or encode. The
			// alt text and link are already rendered above.
			return nil //nolint:nilerr
		}
		*ctx.pendingImages = append(*ctx.pendingImages, seq)
	}

	return nil
}

// graphicsSequence loads the image and returns the encoded graphics protocol
// sequence. Encoded sequences are cached so repeated renders of the same
// image don't re-encode it.
func (e *ImageElement) graphicsSequence(ctx RenderContext, url string) (string, error) {
	img, err := loadImage(url)
	if err != nil {
		return "", fmt.Errorf("glamour: error loading image: %w", err)
	}

	cols, rows := imageDimensions(img, ctx)
	key := sequenceCacheKey{url: url, protocol: ctx.options.ImageProtocol, cols: cols, rows: rows}

	sequenceCache.Lock()
	if seq, ok := sequenceCache.m[key]; ok {
		sequenceCache.Unlock()
		return seq, nil
	}
	sequenceCache.Unlock()

	seq, err := e.encodeGraphics(ctx, url, img, cols, rows)
	if err != nil {
		return "", err
	}

	sequenceCache.Lock()
	sequenceCache.m[key] = seq
	sequenceCache.Unlock()
	return seq, nil
}

// encodeGraphics encodes img into a graphics protocol escape sequence sized
// to cols x rows terminal cells, along with the newlines needed to reserve
// vertical space for the image in the text output.
func (e *ImageElement) encodeGraphics(ctx RenderContext, url string, img image.Image, cols, rows int) (string, error) {
	var buf bytes.Buffer
	switch ctx.options.ImageProtocol {
	case ImageProtocolKitty:
		opts := &kitty.Options{
			Action:          kitty.TransmitAndPut,
			Format:          kitty.PNG,
			Transmission:    kitty.Direct,
			ID:              imageID(url),
			Columns:         cols,
			Rows:            rows,
			DoNotMoveCursor: true,
			Quite:           2,
		}
		// Transmit local files by path to avoid the cost of decoding and
		// re-encoding them; the terminal reads the file directly.
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			opts.Transmission = kitty.File
			opts.File = strings.TrimPrefix(url, "file://")
		}
		if err := kitty.EncodeGraphics(&buf, img, opts); err != nil {
			return "", fmt.Errorf("glamour: error encoding kitty image: %w", err)
		}
	case ImageProtocolSixel:
		// Sixel draws at pixel resolution, so scale the image down to the
		// target cell dimensions to match the reserved space.
		scaled := scaleToCells(img, cols, rows)
		var sb bytes.Buffer
		enc := sixel.Encoder{}
		if err := enc.Encode(&sb, scaled); err != nil {
			return "", fmt.Errorf("glamour: error encoding sixel image: %w", err)
		}
		buf.WriteString("\x1bPq")
		buf.Write(sb.Bytes())
		buf.WriteString("\x1b\\")
	case ImageProtocolNone:
		return "", nil
	default:
		return "", nil
	}

	// Reserve vertical space for the image in the text output by appending
	// one newline per row the image occupies. Without this, the terminal
	// draws the image over the text that follows it.
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(buf.String())
	for i := 0; i < rows; i++ {
		sb.WriteString("\n")
	}
	return sb.String(), nil
}

// imageID returns a stable, positive identifier for an image URL. Stable IDs
// let terminals replace a previous placement of the same image instead of
// piling up copies when a document is re-rendered.
func imageID(url string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(url))
	id := int(h.Sum32())
	if id == 0 {
		id = 1
	}
	return id
}

// imageDimensions calculates the display size of an image in terminal cells.
// It constrains the image width to the available word wrap width and computes
// the height preserving the aspect ratio.
func imageDimensions(img image.Image, ctx RenderContext) (int, int) {
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()

	maxWidth := ctx.options.WordWrap
	if maxWidth <= 0 {
		maxWidth = width
	}
	if width > maxWidth {
		height = height * maxWidth / width
		width = maxWidth
	}

	rows := int(math.Ceil(float64(height) * approxTerminalCellAspect))
	if rows < 1 {
		rows = 1
	}
	return width, rows
}

// scaleToCells scales img to approximately cols x rows terminal cells using
// nearest-neighbor sampling. Terminal cells are assumed to be twice as tall
// as they are wide (see approxTerminalCellAspect), so one column maps to one
// pixel and one row maps to two pixels.
func scaleToCells(img image.Image, cols, rows int) image.Image {
	if cols <= 0 || rows <= 0 {
		return img
	}
	dstW := cols
	dstH := rows * 2

	src := img.Bounds()
	srcW, srcH := src.Dx(), src.Dy()
	if srcW == 0 || srcH == 0 || (srcW <= dstW && srcH <= dstH) {
		return img
	}

	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	for y := 0; y < dstH; y++ {
		sy := src.Min.Y + y*srcH/dstH
		for x := 0; x < dstW; x++ {
			sx := src.Min.X + x*srcW/dstW
			dst.Set(x, y, img.At(sx, sy))
		}
	}
	return dst
}

// loadImage loads an image from a local file path or remote URL. Remote
// images are cached in memory so repeated renders don't re-fetch them.
func loadImage(url string) (image.Image, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return loadLocalImage(url)
	}

	imageCache.Lock()
	if img, ok := imageCache.m[url]; ok {
		imageCache.Unlock()
		return img, nil
	}
	imageCache.Unlock()

	img, err := loadRemoteImage(url)
	if err != nil {
		return nil, err
	}

	imageCache.Lock()
	imageCache.m[url] = img
	imageCache.Unlock()
	return img, nil
}

// loadRemoteImage loads an image from a remote URL.
func loadRemoteImage(url string) (image.Image, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("glamour: error creating request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("glamour: error fetching image: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("glamour: unexpected status code %d fetching image", resp.StatusCode)
	}

	// Read the body into a buffer so we can inspect the content type.
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("glamour: error reading image body: %w", err)
	}

	// Check for SVG which Go's image.Decode cannot handle.
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "image/svg") || bytes.HasPrefix(bytes.TrimSpace(buf), []byte("<svg")) {
		return nil, fmt.Errorf("glamour: SVG images are not supported")
	}

	img, _, err := image.Decode(bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("glamour: error decoding image: %w", err)
	}
	return img, nil
}

// loadLocalImage loads an image from a local file path.
func loadLocalImage(url string) (image.Image, error) {
	path := strings.TrimPrefix(url, "file://")
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("glamour: error opening image file: %w", err)
	}
	defer f.Close() //nolint:errcheck

	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("glamour: error decoding image: %w", err)
	}
	return img, nil
}

// parseHTMLImages extracts <img> tags from HTML and returns an ImageElement
// for each one found. Returns nil if no images are found.
func parseHTMLImages(ctx RenderContext, html string) ElementRenderer {
	matches := htmlImgRegex.FindAllStringSubmatch(html, -1)
	if len(matches) == 0 {
		return nil
	}

	var elements []ElementRenderer
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		src := m[1]
		elements = append(elements, &ImageElement{
			Text:    "",
			BaseURL: ctx.options.BaseURL,
			URL:     src,
		})
	}

	if len(elements) == 0 {
		return nil
	}

	return &CompoundElement{Elements: elements}
}

// A CompoundElement renders multiple elements sequentially.
type CompoundElement struct {
	Elements []ElementRenderer
}

// Render renders all child elements.
func (e *CompoundElement) Render(w io.Writer, ctx RenderContext) error {
	for _, el := range e.Elements {
		if err := el.Render(w, ctx); err != nil {
			return fmt.Errorf("glamour: error rendering element: %w", err)
		}
	}
	return nil
}
