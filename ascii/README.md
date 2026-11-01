# askee/ascii — self-contained reports with text-drawn charts

A small, dependency-free toolkit for writing reports. Markdown in, a
single self-contained HTML file out, plus the Markdown itself as a second readable
artefact. Charts are drawn in text, so the **same bytes** render in the HTML, in the
Markdown, in a terminal, in a commit message and in a runbook. There is no second
source to drift.

## Why it is built this way

**No dependencies, deliberately.** No pandoc, no cmark, no node, no network, no web
fonts. Nothing here needs installing. The renderer is ~150 lines of awk and the charts
are `sh` + `awk`, so a report can be produced from a recovery shell on a box with no
browser — which is exactly when you most want to read one.

**Charts are text because hand-placed SVG kept coming out wrong.** Coordinate arithmetic
and colliding labels. A column of block characters cannot collide with anything. See
`ascii-chart.sh` for the sub-cell technique (block elements for ⅛-character bars,
braille 2×4 dot matrices for line plots).

**No script.** Many file browsers serve files with a strict `Content-Security-Policy`
that blocks inline `<script>`, so a JavaScript chart library cannot run in a report opened
through one. Text charts need no script at all, so the report survives being opened from a
file manager.

## ⚠ Run it in a UTF-8 locale

The block and braille glyphs are multi-byte UTF-8. The bytes are always correct — the kit
asserts that with `iconv -f UTF-8 -t UTF-8` in its own tests — but a shell decoding them
as latin-1 renders `█` as `â`, so charts can *look* broken while being fine.

Set a UTF-8 locale (`LC_ALL=en_US.UTF-8`). If output looks like `ââââ`, that is the
terminal, not the data —
check with `iconv` before changing anything. Pass `-a` for pure-ASCII output if a console
genuinely cannot do UTF-8.

## Usage

```sh
# write the report in Markdown, with charts piped in from ascii-chart.sh
sh ascii/render.sh report.md > report.html

# a chart, inside a fenced block in the Markdown:
printf 'photos\t1266\ndocs\t215\n' | sh ascii/ascii-chart.sh hbar -u MB
```

`render.sh` inlines `theme.css`, so the output HTML is one portable file.

## The Markdown subset it understands

Headings `#`/`##`/`###` · paragraphs · fenced code blocks (become charts) · tables ·
`-` and `1.` lists · `**bold**` · `*italic*` · `` `code` `` · `[links](url)` ·
`> blockquote` (renders as a highlighted note) · `---` horizontal rule · and any line
beginning with `<` passes through as raw HTML, so you can drop in a figure when the
subset is not enough.

The paragraph immediately after the `# title` becomes the lede. Any other block between
them cancels that, so a table under the title does not steal the styling.

**Status shorthand** — three closed classes, usable inline or in a table cell:

```
| Backups | {ok ✓ Good}       | two copies, byte-verified   |
| Repo    | {bad ✕ Fix needed}| 104 files uncommitted       |
| Images  | {warn ▲ Watch}    | 9 of 22 float `:latest`     |
```

A shorthand rather than raw `<span>`, for two reasons: inline HTML inside a table cell
gets escaped (everything is escaped, which is what keeps the renderer safe), and this
still reads as plain prose in the Markdown artefact. You write the symbol and the word
yourself, so rule 3 above cannot be broken by forgetting. A `{brace}` that is not one of
the three classes stays literal.

Not supported, on purpose: nested lists, footnotes, images, reference links. If a report
needs them, extend `render.sh` rather than hand-writing HTML — the point is one source.

## Design rules this kit encodes

These are not preferences; each one is a check that caught a real mistake.

1. **Colour is validated by script, never by eye.** The chart-accessibility validator
   rejected two palettes during this kit's design: a four-hue engine palette (violet vs
   blue, ΔE 1.9 under protanopia — indistinguishable) and an all-earthy accent trio
   (ΔE 1.4 protan, and two of three below the chroma floor, so they read as grey).
   Earthy tones work for *surfaces*; they fail as *categories*.
2. **Earthy paper, bright accents.** Surfaces and ink are warm and low-contrast-friendly;
   the few hues that carry meaning are the validated set. Every text colour in
   `theme.css` has its measured WCAG ratio written beside it.
3. **Never colour alone.** Status carries a symbol *and* a word. Chart segments differ by
   glyph density (`█▓▒░`), not hue. A greyscale print, a screen reader and a
   colour-blind reader all get the same information.
4. **Numbers are printed, never only encoded as length.** A bar is an aid to comparison,
   not the datum.
5. **Every figure carries a key.** From c4model.com: explain every colour, line style and
   symbol, even the obvious ones — the figure will be read outside its original context.
6. **Say what is measured and what is inferred.**

## Files

| file | what it is |
|---|---|
| `render.sh` | Markdown subset → self-contained HTML, theme inlined |
| `theme.css` | the earthy accessible theme, with measured contrast ratios in comments |
| `ascii-chart.sh` | the chart library (`hbar`, `meter`, `stack`, `braille`, `spark`) |

## Typography

System fonts only, so there is nothing to fetch and nothing to break offline. Body is a
warm old-style serif (`Iowan Old Style`, `Palatino`, `Georgia`); headings are a geometric
sans (`Avenir Next`, `Segoe UI`); charts are monospace. The serif/sans pairing is a plain
editorial convention and both halves ship with macOS, iOS and Windows.
