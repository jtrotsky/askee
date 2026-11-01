# askee — one chart catalogue, three faces

A small, dependency-free charting tool for a Prometheus-compatible TSDB (VictoriaMetrics,
Prometheus). Pages are declared once in a JSON file and drawn three ways:

| face | how | for |
|---|---|---|
| **terminal deck** | `askee -config my.json` | btm-style: braille line graphs, eighth-block bars; ←/→ page, ↑/↓ or 1–6 range, `t` theme, click left/right half |
| **web dashboard** | `askee -config my.json -serve 127.0.0.1:8095` | one group per screen; tiles fill the viewport (3 columns on a big display, 2 on a laptop, 1 on a phone); swipe or ←/→ between groups |
| **plain text** | `askee -config my.json -print memory -range 7d` | scripts and LLM skills: boxed panels plus `latest/min/max/mean` numbers per series |

And `ascii/` is the companion **report kit**: `ascii-chart.sh` (hbar, meter, stack, braille,
spark — sh + awk) and `render.sh` (a Markdown subset → one self-contained HTML file). See
`ascii/README.md`.

Standard library only. `go build` needs nothing downloaded; the binary runs from a recovery
shell. Raw terminal mode comes from `stty(1)`.

## Build

```sh
go build -o askee .
./askee -themes                          # list themes and their validation status
./askee -config examples/node-exporter.json -dump 1 -size 120x30
```

## The config

```jsonc
{
  "title": "my-box", "vm": "http://127.0.0.1:8428",
  "default_range": "24h", "default_theme": "reference",
  "thermal_range": [30, 100],            // °C mapped to the cool→hot ramp, the same in every chart
  "snapshot": "/path/data.md",           // optional: `### key` sections from a root-only collector
  "sizes": "/path/sizes.txt",            // optional: TARGET LOCAL REMOTE BILLED table → cost column
  "price_per_tb": 7.50, "price_label": "cold $/mo",
  "groups": [{"id": "system", "title": "System"}],
  "pages": [ { "id": "cpu", "group": "system", "kind": "line", … } ]
}
```

Page kinds:

| kind | draws | keys |
|---|---|---|
| `line` | time series | `series[]` (`expr` + `name`, or `label` to name each result), `refs[]` (`value` or `expr`, + `label`), `ymin`/`ymax`, `stack` (parts of a whole), `thermal` (line coloured by absolute °C) |
| `hour` | 30-day mean by hour of day, as bars | `expr` (an hourly series) |
| `instant` | one bar per result of an instant query | `expr`, `label`, `min_value`, `limit` |
| `runs` | one bar per run of a job — a gauge de-duplicated on the run's own timestamp | `expr`, `key` (the timestamp metric) |
| `snapshot` | a section of the snapshot file; an hbar figure becomes real bars, a table stays a table | `key` |
| `sizes` | the backup sizes table with a cost column | — |

Every page takes `title`, `subtitle` (the *so what*, one line), `unit`, `range` (a fixed range:
`30d`, `90d`, `now`, `snapshot`) and `note`.

**Nothing from a caller reaches the TSDB.** The web API takes a group id and one of the fixed
ranges; every expression comes from the config file.

## How the charts are meant to read

Built to the rules in Tufte's *Visual Display of Quantitative Information* and Eugene Wei's
"Remove the legend to become one", via the dataviz skill's checks:

- **Compared to what.** Reference lines (`refs`) carry the comparison: installed RAM, swap size,
  core count, a drive's own warning temperature. Dashed and dim — the data stays the loudest ink.
- **Stable colour per entity.** Series keep their slot colour in every chart and theme; an empty
  result keeps its slot rather than shifting the colours after it.
- **Gradients only where they carry data.** Bars run along the theme's one-hue sequential ramp,
  so length and colour agree; temperatures use a fixed cool→hot ramp on absolute °C; a single
  line gets a soft wash. No decorative gradients.
- **Parts of a whole are stacked** (`stack`), everything else is lines — never two y-axes.
- **Numbers are printed**, never left to glyph height: legend values, min/max/mean, and in
  `-print` a numeric line per series.
- **Title says what, subtitle says so what.**

## Themes

Fifteen, shared by both faces (truecolor in the terminal when `COLORTERM=truecolor`, else the
nearest xterm-256 colour). Pick with `t`, the web drop-down, `?theme=fjord`, or `ASKEE_THEME`.

- **flat** themes are chalky: solid bars, no washes, temperatures in six hard steps.
- **smooth** themes add gradient washes and one-hue magnitude ramps.
- **mono** (print) themes use one ink: lines differ by dash pattern (solid, dashed, dotted,
  dash-dot, long dash), stacked bands by hatching — Wei's and Tufte's printed charts. They
  survive a black-and-white printer and colour-blind readers alike.

Status is the dataviz validator's verdict on the categorical palette, in order, against the
theme's own surface (lightness band, chroma floor, colour-blind ΔE ≥ 8, normal-vision ΔE ≥ 15,
≥ 3:1 contrast). `floor` = colour-blind ΔE 6–8, legal only with a second cue, so those themes
also dash their lines.

| theme | style | status | look |
|---|---|---|---|
| ember | flat | validated | copper on dark umber |
| fjord | flat | validated | Nord-like slate, muted |
| ember-chalk | flat | validated | Ember softened toward chalk |
| chalkboard | flat | floor (+ dashes) | chalk on slate green |
| newsprint | flat | mono | greyscale on newsprint (light) |
| sepia | flat | mono | brown ink on old paper (light) |
| carbon | flat | mono | greyscale on charcoal |
| phosphor | flat | mono | green CRT |
| amber | flat | mono | amber CRT |
| reference | smooth | validated | a validated reference palette |
| kauri | smooth | validated | sea blue on deep forest |
| pohutukawa | smooth | validated | crimson on warm charcoal |
| aurora | smooth | validated | teal on night blue |
| terminal | flat | fails | the old deck's xterm-256 colours |
| btm | flat | fails | bottom's default |
