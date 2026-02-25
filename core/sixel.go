package core

import (
	"bufio"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// renderImage displays the image at path using the best available method for
// the current platform and configured backend.
//
// On Windows, chafa is rarely available but Windows Terminal supports sixel
// natively, so we always use the built-in sixel renderer there.
// On other platforms we prefer chafa (respecting the configured backend) and
// fall back to the built-in sixel renderer when chafa is not found.
func renderImage(path, backend string) error {
	if runtime.GOOS == "windows" {
		return renderSixel(path)
	}

	// Non-Windows: try chafa first.
	if _, err := exec.LookPath("chafa"); err == nil {
		cmd := exec.Command("chafa", "-f", backend, path)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	// chafa not found — fall back to built-in sixel.
	return renderSixel(path)
}

// renderSixel encodes the image at path as a DEC sixel stream and writes it
// to stdout. The image is resized to fit within maxCols×maxRows terminal cells
// (each cell assumed to be 8×16 px) before encoding.
func renderSixel(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("cannot open image: %w", err)
	}
	defer f.Close()

	img, _, err := image.Decode(f)
	if err != nil {
		return fmt.Errorf("cannot decode image: %w", err)
	}

	// Target: up to 40 columns × 20 rows of terminal cells.
	const (
		maxCols   = 40
		maxRows   = 20
		cellW     = 8
		cellH     = 16
		maxPixelW = maxCols * cellW // 320
		maxPixelH = maxRows * cellH // 320
	)

	img = resizeImage(img, maxPixelW, maxPixelH)

	w := bufio.NewWriter(os.Stdout)
	if err := encodeSixel(w, img); err != nil {
		return err
	}
	return w.Flush()
}

// resizeImage scales img down so it fits within maxW×maxH, preserving aspect
// ratio. Uses nearest-neighbour sampling (no external deps).
func resizeImage(src image.Image, maxW, maxH int) image.Image {
	bounds := src.Bounds()
	srcW := bounds.Max.X - bounds.Min.X
	srcH := bounds.Max.Y - bounds.Min.Y

	if srcW <= maxW && srcH <= maxH {
		return src
	}

	scaleX := float64(maxW) / float64(srcW)
	scaleY := float64(maxH) / float64(srcH)
	scale := scaleX
	if scaleY < scale {
		scale = scaleY
	}

	dstW := int(float64(srcW) * scale)
	dstH := int(float64(srcH) * scale)
	if dstW < 1 {
		dstW = 1
	}
	if dstH < 1 {
		dstH = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	for y := 0; y < dstH; y++ {
		srcY := bounds.Min.Y + int(float64(y)/scale)
		for x := 0; x < dstW; x++ {
			srcX := bounds.Min.X + int(float64(x)/scale)
			dst.Set(x, y, src.At(srcX, srcY))
		}
	}
	return dst
}

// encodeSixel writes a sixel stream for img to w.
//
// Sixel protocol basics:
//   - DCS intro:  ESC P <Pa> ; <Pb> ; <Pc> q
//   - Raster attr: " <Pan> ; <Pad> ; <Ph> ; <Pv>
//   - Colour def: # <n> ; 2 ; <r> ; <g> ; <b>   (r/g/b in 0-100)
//   - Data:        # <n> <sixel-chars...> $  (CR = next column group)
//   - (next sixel band)
//   - DCS end:    ESC \
//
// We quantise to 256 colours using a simple median-cut palette and write one
// band (6 pixel rows) at a time.
func encodeSixel(w *bufio.Writer, img image.Image) error {
	bounds := img.Bounds()
	width := bounds.Max.X - bounds.Min.X
	height := bounds.Max.Y - bounds.Min.Y

	// Collect all colours and build a 256-entry palette via median cut.
	palette := buildPalette(img, bounds, 256)

	// Map each pixel to its nearest palette index.
	indices := make([]int, width*height)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			c := img.At(bounds.Min.X+x, bounds.Min.Y+y)
			indices[y*width+x] = nearestColor(palette, c)
		}
	}

	// DCS intro: P1=0 (pixel aspect 2:1), P2=1 (background transparent), P3=q
	fmt.Fprintf(w, "\x1bPq")
	// Raster attributes: Pan=1, Pad=1, Ph=width, Pv=height
	fmt.Fprintf(w, "\"1;1;%d;%d", width, height)

	// Emit colour definitions (#n;2;r;g;b).
	for i, c := range palette {
		r, g, b, _ := c.RGBA()
		r100 := int(r) * 100 / 0xffff
		g100 := int(g) * 100 / 0xffff
		b100 := int(b) * 100 / 0xffff
		fmt.Fprintf(w, "#%d;2;%d;%d;%d", i, r100, g100, b100)
	}

	// Emit pixel data band by band (6 rows per band).
	for bandY := 0; bandY < height; bandY += 6 {
		if bandY > 0 {
			fmt.Fprint(w, "-") // next band
		}

		// For each colour used in this band, build its sixel row.
		// A sixel character encodes 1 column × 6 rows for one colour.
		// Value: bit n set if palette[n] is used at row (bandY+n) in this column.
		bandH := 6
		if bandY+6 > height {
			bandH = height - bandY
		}

		// Gather which colour indices appear in this band.
		usedColors := make(map[int]bool)
		for row := 0; row < bandH; row++ {
			y := bandY + row
			for x := 0; x < width; x++ {
				usedColors[indices[y*width+x]] = true
			}
		}

		first := true
		for ci := range usedColors {
			if !first {
				fmt.Fprint(w, "$") // carriage return (back to column 0)
			}
			first = false

			fmt.Fprintf(w, "#%d", ci)

			// Build the sixel string for this colour across all columns.
			var sb strings.Builder
			for x := 0; x < width; x++ {
				var bits byte
				for row := 0; row < bandH; row++ {
					y := bandY + row
					if indices[y*width+x] == ci {
						bits |= 1 << uint(row)
					}
				}
				sb.WriteByte(bits + 63) // sixel data character
			}
			fmt.Fprint(w, sb.String())
		}
	}

	// DCS string terminator.
	fmt.Fprint(w, "\x1b\\")
	fmt.Fprintln(w) // blank line so the prompt appears below the image
	return nil
}

// buildPalette returns up to maxColors distinct colours sampled from img using
// a simple frequency-based reduction (pick the most common colours).
func buildPalette(img image.Image, bounds image.Rectangle, maxColors int) []color.Color {
	freq := make(map[color.RGBA]int)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			// Quantise to 6 bits per channel to reduce palette explosion.
			c := color.RGBA{
				R: uint8(r >> 10),
				G: uint8(g >> 10),
				B: uint8(b >> 10),
				A: uint8(a >> 8),
			}
			freq[c]++
		}
	}

	type entry struct {
		c     color.RGBA
		count int
	}
	entries := make([]entry, 0, len(freq))
	for c, n := range freq {
		entries = append(entries, entry{c, n})
	}
	// Sort descending by frequency (simple insertion-style for small slices).
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].count > entries[j-1].count; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}

	if len(entries) > maxColors {
		entries = entries[:maxColors]
	}

	palette := make([]color.Color, len(entries))
	for i, e := range entries {
		// Scale back to full 8-bit range.
		palette[i] = color.RGBA{
			R: e.c.R << 2,
			G: e.c.G << 2,
			B: e.c.B << 2,
			A: e.c.A,
		}
	}
	return palette
}

// nearestColor returns the index in palette closest to c (Euclidean RGB distance).
func nearestColor(palette []color.Color, c color.Color) int {
	r, g, b, _ := c.RGBA()
	best := 0
	bestDist := int64(1 << 62)
	for i, pc := range palette {
		pr, pg, pb, _ := pc.RGBA()
		dr := int64(r) - int64(pr)
		dg := int64(g) - int64(pg)
		db := int64(b) - int64(pb)
		dist := dr*dr + dg*dg + db*db
		if dist < bestDist {
			bestDist = dist
			best = i
			if dist == 0 {
				break
			}
		}
	}
	return best
}
