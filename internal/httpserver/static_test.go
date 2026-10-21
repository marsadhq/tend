package httpserver_test

import (
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
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

// mediaBlock returns the body of the first "@media <query> {" block in css.
func mediaBlock(t *testing.T, css, query string) string {
	t.Helper()
	open := "@media " + query + " {"
	start := strings.Index(css, open)
	if start < 0 {
		t.Fatalf("app.css has no %q block", open)
	}
	depth := 0
	for i := start + len(open) - 1; i < len(css); i++ {
		switch css[i] {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				return css[start+len(open) : i]
			}
		}
	}
	t.Fatalf("app.css: %q block is not closed", open)
	return ""
}

// On a phone the tabs are a bar fixed to the bottom of the screen. The page
// keeps that much room at its bottom when it scrolls something into view (a
// focused link, the end of a run's output), so the bar never covers it. And
// the safe-area insets are only filled in under viewport-fit=cover, which the
// pages do not ask for, so the stylesheet must not lean on them.
func TestPhoneTabBarLeavesRoomForFocus(t *testing.T) {
	raw, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	css := string(raw)
	phone := mediaBlock(t, css, "(max-width: 640px)")
	if !regexp.MustCompile(`\.nav \{[^}]*position: fixed`).MatchString(phone) {
		t.Skip("the phone tabs are no longer a fixed bar")
	}
	if !regexp.MustCompile(`(?m)^\s*html \{[^}]*scroll-padding-bottom:`).MatchString(phone) {
		t.Errorf("the phone block fixes the tabs to the bottom but sets no scroll-padding-bottom on html")
	}
	base, err := os.ReadFile("templates/base.html")
	if err != nil {
		t.Fatalf("read base.html: %v", err)
	}
	if strings.Contains(css, "safe-area-inset") && !strings.Contains(string(base), "viewport-fit=cover") {
		t.Errorf("app.css uses env(safe-area-inset-*), which is 0 without viewport-fit=cover in the viewport meta")
	}
}

// On a phone the table header row is hidden from sight but not from screen
// readers (no display: none), which announce each cell with its column's
// header; the column names shown in front of the cells have empty alt text,
// so a screen reader does not read them a second time.
func TestPhoneCardsKeepHeadersForScreenReaders(t *testing.T) {
	raw, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	phone := mediaBlock(t, string(raw), "(max-width: 640px)")
	if regexp.MustCompile(`thead \{[^}]*display: none`).MatchString(phone) {
		t.Errorf("the phone block hides the table headers with display: none, which hides them from screen readers too")
	}
	if !strings.Contains(phone, `content: attr(data-label) / "";`) {
		t.Errorf(`the cards' column names should be generated with empty alt text (content: attr(data-label) / "";)`)
	}
}
