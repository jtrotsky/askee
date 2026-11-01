package main

// Themes. Each is one set of hex colours used by BOTH faces — the terminal deck (truecolor,
// or nearest xterm-256 when the terminal can't) and the web dashboard (CSS variables) — so a
// series is the same colour wherever it is read (Tufte: stable colour per entity).
//
// "Validated" means the categorical palette, in this order, passed the dataviz skill's
// validator (scripts/validate_palette.js --mode dark --surface <Surface>): OKLCH lightness
// band, chroma floor, adjacent-pair colour-blind separation (ΔE ≥ 8), normal-vision floor
// (ΔE ≥ 15) and ≥ 3:1 contrast. The order is part of the safety, not decoration: don't
// re-order a validated palette. btm and terminal are kept for familiarity and FAIL the gates
// (yellow↔green are hard to tell apart even with full colour vision) — they say so in the UI.
//
// Thermal is a fixed perceptual ramp (cool → hot), mapped to ABSOLUTE degrees via the
// config's thermal_range, so 80 °C is the same colour in every chart and every theme.

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

type Theme struct {
	ID, Name, Note string
	// Status of the categorical palette under the dataviz validator (dark or light mode against
	// its own surface): "validated" (all gates), "floor" (colour-blind ΔE 6–8: legal only with
	// secondary encoding, which these themes supply as line styles), "fails", or "mono" (one ink:
	// series are told apart by LINE STYLE, never colour — Wei/Tufte print style).
	Status        string
	Style         string // "flat" (solid, stepped — chalky, ASCII-like) or "smooth" (gradient washes)
	Mono          bool   // one ink: series differ by dash pattern; bars and fills in ink tones
	Dashed        bool   // colour AND dash pattern (the secondary encoding a "floor" palette needs)
	Light         bool   // paper-coloured surface
	Surface       string // page background
	Panel         string // tile background
	Border, Grid  string
	Text, Dim     string
	Title, Accent string
	Palette       []string // categorical, fixed order (mono: ink tones, darkest first)
}

func (t Theme) validated() bool { return t.Status == "validated" }

var thermalStops = []string{"#3b4cc0", "#5e8fd8", "#8cc3c7", "#e9d38b", "#f19a5b", "#d9483b", "#9e1e3a"}

// Dash patterns for mono themes, in series order (on/off lengths in px for the web; the
// terminal uses the same order with braille-dot masks): solid, dashed, dotted, dash-dot, long.
var dashPatterns = [][]float64{{}, {7, 4}, {1.5, 3.5}, {9, 3, 2, 3}, {14, 5}, {3, 3}}

var themes = []Theme{
	// ── favourites first, flat and chalky ────────────────────────────────────────────────
	{ID: "ember", Name: "Ember", Status: "validated", Style: "flat", Note: "copper lead on dark umber",
		Surface: "#120f0c", Panel: "#17130f", Border: "#362d24", Grid: "#241e18", Text: "#f3ece4", Dim: "#a99c8c", Title: "#f0a070", Accent: "#d46a34",
		Palette: []string{"#d46a34", "#0d9298", "#b88a01", "#b25196", "#47944c", "#6079d3", "#b9454c", "#2c9a88"}},
	{ID: "fjord", Name: "Fjord", Status: "validated", Style: "flat", Note: "Nord-like slate, muted",
		Surface: "#161a22", Panel: "#1b2029", Border: "#323a48", Grid: "#232935", Text: "#e5e9f0", Dim: "#9aa3b2", Title: "#88c0d0", Accent: "#c48225",
		Palette: []string{"#2e97ca", "#c48225", "#3aa273", "#8d6cc2", "#c6585d", "#06999a", "#949523", "#b55d92"}},
	{ID: "ember-chalk", Name: "Ember chalk", Status: "validated", Style: "flat", Note: "Ember softened toward chalk: lower chroma, warm grey ground",
		Surface: "#191714", Panel: "#1e1b18", Border: "#3a342d", Grid: "#2a2621", Text: "#ece6dc", Dim: "#a59c90", Title: "#e0a07e", Accent: "#ce7951",
		Palette: []string{"#ce7951", "#03999f", "#b2912e", "#af6297", "#569859", "#5d7dc8", "#b75357", "#22a68b"}},
	{ID: "chalkboard", Name: "Chalkboard", Status: "floor", Style: "flat", Note: "chalk on slate green; colour-blind ΔE in the 6–8 band, so lines also differ by style",
		Dashed: true, Surface: "#1a231e", Panel: "#1f2a24", Border: "#3b4a41", Grid: "#2a3630", Text: "#e8ece4", Dim: "#a3ae9f", Title: "#e6d9a2", Accent: "#c86265",
		Palette: []string{"#ac942f", "#2a98c4", "#c86265", "#9175c1", "#39a57e", "#b36d2a", "#4c77bd", "#b4608a"}},
	// ── print: one ink, line styles carry identity ───────────────────────────────────────
	{ID: "newsprint", Name: "Newsprint", Status: "mono", Style: "flat", Mono: true, Light: true, Note: "greyscale on newsprint; dashes, dots and dash-dots tell the lines apart",
		Surface: "#e9e7e1", Panel: "#f1efea", Border: "#b9b5ab", Grid: "#dcd8cf", Text: "#1d1c1a", Dim: "#6b6860", Title: "#1d1c1a", Accent: "#1d1c1a",
		Palette: []string{"#1d1c1a", "#3c3a36", "#5a5751", "#76736b", "#2a2926", "#4a4843"}},
	{ID: "sepia", Name: "Sepia", Status: "mono", Style: "flat", Mono: true, Light: true, Note: "brown ink on old paper, like a letterpress plate",
		Surface: "#ede2c8", Panel: "#f4ead2", Border: "#c2ad85", Grid: "#e2d3b0", Text: "#3b2a18", Dim: "#7a6347", Title: "#3b2a18", Accent: "#6b4423",
		Palette: []string{"#3b2a18", "#5c4128", "#7a5a3a", "#94775a", "#4b3520", "#6b4f33"}},
	{ID: "carbon", Name: "Carbon", Status: "mono", Style: "flat", Mono: true, Note: "greyscale on charcoal — the print style, dark",
		Surface: "#141414", Panel: "#1a1a1a", Border: "#3a3a3a", Grid: "#262626", Text: "#e6e6e6", Dim: "#9a9a9a", Title: "#f2f2f2", Accent: "#f2f2f2",
		Palette: []string{"#ededed", "#c4c4c4", "#a0a0a0", "#848484", "#d8d8d8", "#b0b0b0"}},
	{ID: "phosphor", Name: "Phosphor", Status: "mono", Style: "flat", Mono: true, Note: "green CRT: one phosphor, line styles",
		Surface: "#050a06", Panel: "#081009", Border: "#1d3a22", Grid: "#102214", Text: "#9dffb0", Dim: "#4f9a5f", Title: "#b8ffc6", Accent: "#d6ffde",
		Palette: []string{"#7dff98", "#5fd67a", "#47b062", "#348c4c", "#9dffb0", "#6ce887"}},
	{ID: "amber", Name: "Amber", Status: "mono", Style: "flat", Mono: true, Note: "amber CRT: one phosphor, line styles",
		Surface: "#0d0802", Panel: "#130c03", Border: "#3d2a0d", Grid: "#22180a", Text: "#ffc46b", Dim: "#a87a35", Title: "#ffd28a", Accent: "#ffe0a8",
		Palette: []string{"#ffb84d", "#e09a2e", "#c0801f", "#9c6614", "#ffcc77", "#f0a83c"}},
	// ── smooth: gradient washes and ramps ───────────────────────────────────────────────
	{ID: "reference", Name: "Reference", Status: "validated", Style: "smooth", Note: "a validated reference dark palette — the safest default",
		Surface: "#131312", Panel: "#1a1a19", Border: "#33332f", Grid: "#262624", Text: "#f0efec", Dim: "#a3a29a", Title: "#f0efec", Accent: "#d95926",
		Palette: []string{"#3987e5", "#d95926", "#199e70", "#c98500", "#d55181", "#008300", "#9085e9", "#e66767"}},
	{ID: "kauri", Name: "Kauri", Status: "validated", Style: "smooth", Note: "deep-forest ground, sea blue lead",
		Surface: "#0d1116", Panel: "#12161c", Border: "#26303b", Grid: "#1a212a", Text: "#e6ebef", Dim: "#91a0ad", Title: "#7cc4e8", Accent: "#c28412",
		Palette: []string{"#1292c0", "#2e9e52", "#8f68cb", "#c28412", "#cb4644", "#06999a", "#989912", "#c0558d"}},
	{ID: "pohutukawa", Name: "Pōhutukawa", Status: "validated", Style: "smooth", Note: "crimson lead on warm charcoal",
		Surface: "#110f0f", Panel: "#161414", Border: "#332b2b", Grid: "#221e1e", Text: "#f2ecea", Dim: "#a8a09d", Title: "#f08a86", Accent: "#cb4644",
		Palette: []string{"#cb4644", "#2d86c8", "#4e9a52", "#ae5ea8", "#b28d00", "#179a8e", "#c56c21", "#736dc3"}},
	{ID: "aurora", Name: "Aurora", Status: "validated", Style: "smooth", Note: "teal lead on night blue",
		Surface: "#0b0d18", Panel: "#0f1220", Border: "#262b45", Grid: "#181c30", Text: "#e8eaf6", Dim: "#9298b8", Title: "#5fd0c0", Accent: "#d15d4d",
		Palette: []string{"#08a28f", "#d15d4d", "#af62c1", "#ac9003", "#4278d2", "#4f9b3e", "#725ec2", "#c8741d"}},
	// ── familiar, not colour-blind safe ─────────────────────────────────────────────────
	{ID: "terminal", Name: "Terminal", Status: "fails", Style: "flat", Note: "the deck's xterm-256 colours; yellow and green are hard to tell apart",
		Surface: "#0b0e14", Panel: "#10141b", Border: "#2b313c", Grid: "#1d222b", Text: "#d0d0d0", Dim: "#8a8f98", Title: "#5fd7ff", Accent: "#ffaf00",
		Palette: []string{"#5fd7ff", "#87ff87", "#ffff87", "#ff87ff", "#87d7ff", "#ff8787", "#afffff", "#d7afff"}},
	{ID: "btm", Name: "btm", Status: "fails", Style: "flat", Note: "bottom's default theme (xterm bright colours)",
		Surface: "#000000", Panel: "#000000", Border: "#6c6c6c", Grid: "#1c1c1c", Text: "#bcbcbc", Dim: "#808080", Title: "#bcbcbc", Accent: "#5c5cff",
		Palette: []string{"#ff55ff", "#ffff55", "#55ffff", "#55ff55", "#5c5cff", "#ff5555", "#00cdcd", "#00cd00"}},
}

// thermalFor returns the temperature ramp for a theme: the smooth ramp, the same ramp in six hard
// steps for flat themes (a chalk box, not an airbrush), or nil for mono — one ink cannot carry
// a second scale, so there the reference lines carry the meaning.
func (t Theme) thermalFor() []string {
	if t.Mono {
		return nil
	}
	if t.Style == "flat" {
		var out []string
		for i := 0; i < 6; i++ {
			out = append(out, rampAt(thermalStops, (float64(i)+0.5)/6))
		}
		return out
	}
	return thermalStops
}

// stepAt picks the nearest discrete step (flat themes) or interpolates (smooth).
func (t Theme) thermalAt(v float64, rng [2]float64) string {
	st := t.thermalFor()
	if st == nil {
		return t.Palette[0]
	}
	f := (v - rng[0]) / (rng[1] - rng[0])
	if t.Style == "flat" {
		i := int(math.Max(0, math.Min(float64(len(st)-1), math.Floor(f*float64(len(st))))))
		return st[i]
	}
	return rampAt(st, f)
}

func themeByID(id string) Theme {
	for _, t := range themes {
		if t.ID == id {
			return t
		}
	}
	return themes[0]
}

// ── colour maths (sRGB, linear interpolation is fine for ramps between near hues) ─────
type rgb struct{ r, g, b float64 }

func parseHex(h string) rgb {
	h = strings.TrimPrefix(h, "#")
	v, _ := strconv.ParseUint(h, 16, 32)
	return rgb{float64(v >> 16 & 255), float64(v >> 8 & 255), float64(v & 255)}
}
func (c rgb) hex() string {
	cl := func(x float64) int { return int(math.Max(0, math.Min(255, math.Round(x)))) }
	return fmt.Sprintf("#%02x%02x%02x", cl(c.r), cl(c.g), cl(c.b))
}
func mix(a, b rgb, t float64) rgb {
	return rgb{a.r + (b.r-a.r)*t, a.g + (b.g-a.g)*t, a.b + (b.b-a.b)*t}
}

// rampAt samples a multi-stop ramp at t ∈ [0,1].
func rampAt(stops []string, t float64) string {
	t = math.Max(0, math.Min(1, t))
	if len(stops) == 1 {
		return stops[0]
	}
	f := t * float64(len(stops)-1)
	i := int(f)
	if i >= len(stops)-1 {
		return stops[len(stops)-1]
	}
	return mix(parseHex(stops[i]), parseHex(stops[i+1]), f-float64(i)).hex()
}

// seqRamp is the theme's sequential ramp for magnitude: its lead hue from a dim tint of the
// panel up to the full colour and a little past it. One hue, light→strong (dataviz rule).
func (t Theme) seqRamp() []string {
	lead, panel := parseHex(t.Palette[0]), parseHex(t.Panel)
	if t.Style == "flat" { // chalk: one solid colour, length alone carries the value
		return []string{lead.hex(), lead.hex()}
	}
	return []string{mix(panel, lead, 0.35).hex(), mix(panel, lead, 0.7).hex(), lead.hex(), mix(lead, rgb{255, 255, 255}, 0.25).hex()}
}

// ── terminal output: truecolor when advertised, else the nearest xterm-256 entry ──────
var xterm256 []rgb

func init() {
	base := []string{"#000000", "#800000", "#008000", "#808000", "#000080", "#800080", "#008080", "#c0c0c0",
		"#808080", "#ff0000", "#00ff00", "#ffff00", "#0000ff", "#ff00ff", "#00ffff", "#ffffff"}
	for _, h := range base {
		xterm256 = append(xterm256, parseHex(h))
	}
	lv := []float64{0, 95, 135, 175, 215, 255}
	for r := 0; r < 6; r++ {
		for g := 0; g < 6; g++ {
			for b := 0; b < 6; b++ {
				xterm256 = append(xterm256, rgb{lv[r], lv[g], lv[b]})
			}
		}
	}
	for i := 0; i < 24; i++ {
		v := float64(8 + 10*i)
		xterm256 = append(xterm256, rgb{v, v, v})
	}
}

func nearest256(h string) int {
	c := parseHex(h)
	best, bd := 0, math.MaxFloat64
	for i := 16; i < 256; i++ { // skip the 16 system colours: terminals redefine them
		x := xterm256[i]
		d := (x.r-c.r)*(x.r-c.r)*0.3 + (x.g-c.g)*(x.g-c.g)*0.59 + (x.b-c.b)*(x.b-c.b)*0.11
		if d < bd {
			best, bd = i, d
		}
	}
	return best
}

func sgrFG(h string, truecolor bool) string {
	if truecolor {
		c := parseHex(h)
		return fmt.Sprintf("38;2;%d;%d;%d", int(c.r), int(c.g), int(c.b))
	}
	return fmt.Sprintf("38;5;%d", nearest256(h))
}
func sgrBG(h string, truecolor bool) string {
	if truecolor {
		c := parseHex(h)
		return fmt.Sprintf("48;2;%d;%d;%d", int(c.r), int(c.g), int(c.b))
	}
	return fmt.Sprintf("48;5;%d", nearest256(h))
}
