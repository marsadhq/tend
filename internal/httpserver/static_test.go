package httpserver_test

import (
	"math"
	"os"
	"regexp"
	"strconv"
	"testing"
)

// cssVar extracts a "--name: #hexvalue;" custom-property declaration from css.
func cssVar(t *testing.T, css, name string) string {
	t.Helper()
	re := regexp.MustCompile(regexp.QuoteMeta(name) + `:\s*(#[0-9a-fA-F]{6})\s*;`)
	m := re.FindStringSubmatch(css)
	if m == nil {
		t.Fatalf("CSS custom property %q not found in app.css", name)
	}
	return m[1]
}

// srgbToLinear converts an 8-bit sRGB channel to its linear-light value per
// the WCAG relative-luminance formula (sRGB spec, IEC 61966-2-1).
func srgbToLinear(c float64) float64 {
	c /= 255
	if c <= 0.03928 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

// relativeLuminance computes the WCAG relative luminance of a "#rrggbb" hex color.
func relativeLuminance(t *testing.T, hex string) float64 {
	t.Helper()
	r, err := strconv.ParseUint(hex[1:3], 16, 8)
	if err != nil {
		t.Fatalf("parse hex color %q: %v", hex, err)
	}
	g, err := strconv.ParseUint(hex[3:5], 16, 8)
	if err != nil {
		t.Fatalf("parse hex color %q: %v", hex, err)
	}
	b, err := strconv.ParseUint(hex[5:7], 16, 8)
	if err != nil {
		t.Fatalf("parse hex color %q: %v", hex, err)
	}
	return 0.2126*srgbToLinear(float64(r)) + 0.7152*srgbToLinear(float64(g)) + 0.0722*srgbToLinear(float64(b))
}

// contrastRatio computes the WCAG 1.4.3 contrast ratio between two "#rrggbb"
// colors: (L_lighter + 0.05) / (L_darker + 0.05).
func contrastRatio(t *testing.T, hexA, hexB string) float64 {
	t.Helper()
	la, lb := relativeLuminance(t, hexA), relativeLuminance(t, hexB)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// TestInkFaintClearsWCAGContrast locks --ink-faint at >=4.5:1 (WCAG 1.4.3
// small-text minimum) against both --bg and the stricter (lighter) --bg-elev,
// so a future tweak to the CSS cannot silently regress contrast below the
// accessible threshold.
func TestInkFaintClearsWCAGContrast(t *testing.T) {
	css, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	inkFaint := cssVar(t, string(css), "--ink-faint")
	bg := cssVar(t, string(css), "--bg")
	bgElev := cssVar(t, string(css), "--bg-elev")

	const minRatio = 4.5
	if r := contrastRatio(t, inkFaint, bg); r < minRatio {
		t.Errorf("--ink-faint (%s) vs --bg (%s): contrast %.3f:1, want >= %.1f:1", inkFaint, bg, r, minRatio)
	}
	if r := contrastRatio(t, inkFaint, bgElev); r < minRatio {
		t.Errorf("--ink-faint (%s) vs --bg-elev (%s): contrast %.3f:1, want >= %.1f:1", inkFaint, bgElev, r, minRatio)
	}
}
