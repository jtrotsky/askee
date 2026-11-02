# askee

Small, dependency-free charts for the terminal and the browser, read from a
Prometheus-compatible database (VictoriaMetrics, Prometheus). One Go binary, standard library
only: nothing to download, no build step beyond `go build`, and it runs from a recovery shell.

You describe your charts once, in a JSON file, and askee draws them three ways: a tiled web
dashboard that fills whatever screen it is on, a btm-style deck in the terminal, and plain-text
panels for scripts and LLM tools to read.

![Newsprint theme](images/web-newsprint.png)

## Accessible first

The default theme is a print theme: **Newsprint** on a light system, **Carbon** on a dark one,
switched automatically. Print themes use one ink. Lines tell themselves apart by dash pattern
— solid, dashed, dotted, dash-dot — and stacked bands by shades of grey, the way a chart in a
newspaper or an old book does. A chart stays readable with no colour at all: on a
black-and-white printer, on a monochrome screen, and for anyone who does not see colour the way
the designer did.

![Carbon theme](images/web-carbon.png)

The rest follows the same idea, after Tufte's *The Visual Display of Quantitative Information*:

- **Colour themes are checked, not eyeballed.** Each palette, in order, was run through a
  colour-blindness validator against its own background (adjacent-pair ΔE ≥ 8 under protan and
  deutan simulation, ΔE ≥ 15 for normal vision, ≥ 3:1 contrast). Themes that fail are labelled
  as failing in the picker.
- **No legend to decode.** Each line is named at its right end, with its latest value, beside a
  sample of its own line style. Stacked bands are named at their middle.
- **Numbers are printed, not left to the eye.** There is no grid. The y axis is a range frame: a
  hairline as long as the data's range, labelled with the data's actual min and max. Every bar
  carries its value.
- **Short titles.** A chart's title says what it shows in a few words; the sentence on what it
  means waits behind the ⓘ beside it.
- **Compared to what.** Reference lines carry the comparison — installed RAM, swap size, core
  count, a drive's own warning temperature — dashed and quiet, so the data stays the loudest
  ink. Events such as reboots are marked on every chart, so a step in a line explains itself.
- **Small multiples.** Series on very different scales get a panel each, on one time axis,
  instead of one chart where the smallest line lies flat along the bottom.
- **Text stays readable.** Fonts never shrink below 11 px. A chart with many bars takes more
  room on the grid, and if it still does not fit, it scrolls inside its tile instead of
  shrinking.
- **Live, without a database.** The `live` range ticks every second, like btm. With
  `live_source` set to an exporter's `/metrics` URL, askee reads it once a second **only while
  someone is watching**, keeps five minutes in memory and stores nothing; the older part of the
  window is filled from the database until those five minutes are read. Live pages are computed
  from a small PromQL subset (`rate`, `sum`/`avg`/`max`/`min`, label matchers, arithmetic);
  anything beyond it falls back to the database. Other ranges stand still and repaint in place
  on a cadence that suits them (1 min for 1h, 5 min beyond).
- **Instant.** The server answers from a cache it keeps warm and refreshes behind the reader;
  the page keeps every group it has seen and fetches the neighbours in the background, so
  ←/→ draws at once. `test/perf.mjs` holds it to a budget (below).
- **Keyboard and touch.** ←/→ move between groups of charts, ↑/↓ between pages, 1–7 pick the
  time range, `t` cycles themes and `a` returns to automatic. Swipe left and right on a phone.

## The three faces

### Web dashboard

One group of charts per screen. Charts are packed into a grid sized to the viewport — three
columns on a big display, two on a laptop, one on a phone — and a chart that needs more room
spans more cells. When a group has more charts than fit, it gets pages.

![Sepia theme](images/web-sepia.png)

```sh
askee -serve 127.0.0.1:8095
```

It is read-only and takes no query text from the browser: only a group name and one of the
fixed time ranges. Every query comes from your config file.

<img src="images/web-phone.png" width="260" alt="On a phone">

### Terminal deck

Braille line graphs (2 × 4 dots per character cell) and eighth-block bars. ←/→ or a click on
either half of the screen move between charts, ↑/↓ change the time range, `t` the theme.
Truecolor when the terminal says so, the nearest xterm-256 colour otherwise.

![Terminal deck, Ember](images/terminal-ember.png)

![Terminal deck, Newsprint](images/terminal-newsprint.png)

```sh
askee                      # finds the config: $ASKEE_CONFIG, ./askee.json,
                           # ~/.config/askee/askee.json, /usr/local/etc/askee/askee.json
askee -config my.json      # or name it
```

### Plain text

Boxed panels plus a numeric line per series, so a script — or an LLM reading a health check —
never has to estimate a value from the height of a glyph.

```
$ askee -config my.json -print system -range 24h -size 72x9

┌─ CPU busy [24h] ──────────────── ■ ━━ kernel 18.6%  ■ ╍╍ apps 12.9% ─┐
│  97.5┤        ┊reboot    ⢰     ⣶           ⣄                         │
│      │        ┊          ⢸ ⡇  ⢀⢿          ⢸⢘                 ⡀       │
│      │        ┊      ⢀⢸⡄⢠⢸⡄⡇⣀ ⢸⠈ ⡇      ⡄ ⢸⠉⢇                ⣷ ⢠⡄    │
│      │      ⢤⢀┊   ⢀⢀ ⣸⣸⡇⣾⣿⢇⢸⡏ ⢸  ⣧⣴ ⣦  ⢠⡇ ⡟ ⢸⡇              ⢠⣿ ⣮⣷ ⣠⡄ │
│      │      ⠱⡫⡄⣄⣀ ⢸⡿⣄⡟⣿⢱⣹⡏⠘⢾⣷⣀⣛⢀⠞⡧⢼⣤⢬⣓⣀⣚⣘⣀⡇ ⠘⣗⣒⣀⣒⣒⣀⣪⣓⣀⣒⣓⣀⣚⣚⣄⣿⠟⣷⠎⠻⣄⢼⡅ │
│  6.08┤        ⠙⠺⠛⠎⠛⠁           ⠉                                     │
│       12:46                       00:46                        12:46 │
└────────────────────────── kernel: min 6.08 · max 70.6 · mean 21.5 % ─┘
  kernel             latest 18.6     min 6.08     max 70.6     mean 21.5     % (n=259)
  apps               latest 12.9     min 3.60     max 85.1     mean 14.5     % (n=259)
  other              latest 12.1     min 0.00     max 24.5     mean 0.76     % (n=259)
  so what: Kernel time includes ZFS and disk encryption.
```

## Themes

Nine, shared by every face, in three kinds. Pick with the drop-down, `t`, `?theme=sepia` in the
URL, or `ASKEE_THEME=sepia` in the terminal.

| theme | kind | colour-blind check | look |
|---|---|---|---|
| **newsprint** | print | one ink, line styles | greyscale on newsprint — the light default |
| **sepia** | print | one ink, line styles | brown ink on old paper |
| **carbon** | print | one ink, line styles | greyscale on charcoal — the dark default |
| phosphor | print | one ink, line styles | green CRT |
| amber | print | one ink, line styles | amber CRT |
| ember-chalk | flat | passes | copper and chalk on warm grey |
| fjord | flat | passes | muted Nord-like slate |
| chalkboard | flat | borderline, so lines are also dashed | chalk on slate green |
| terminal | flat | **fails** (yellow and green) | the standard xterm-256 colours |

**Print** themes use one ink and tell lines apart by dash pattern. **Flat** themes use a few
solid, chalky colours; colour only ever names a series or carries a temperature.

![Ember chalk theme](images/web-ember.png)

![Fjord theme](images/web-fjord.png)

## Chart styles

Six, and every page is one of them. Each follows the same rules: a title of a few words,
numbers printed, a comparison on the chart, no legend to decode.

| style | config | use it for |
|---|---|---|
| **line** | `kind: line` | one to four series over time. Named at the right end; reference values dashed. `log` for values spanning orders of magnitude (latency). |
| **stack** | `kind: line`, `stack: true` | parts of a whole over time (memory, CPU by kind). Bands are solid tints, named at their middle. |
| **small multiples** | `kind: line`, `split: true` | series on very different scales (free space per pool): one panel each, one time axis. |
| **bars** | `instant`, `runs`, `hour` | a ranked list or a profile. Block cells, value at the end, a reference rule where one exists. |
| **blocks** | `capacity`, `sizes` | how full something is, or what fills it: a row of cells per thing, free space as empty cells. |
| **table** | `snapshot` | a handful of numbers a reader would rather read than estimate. |

Horizontal bars are drawn as runs of small cells, like a row of `█` in a terminal — the same
look in the browser, the deck and `-print`.

## Install

```sh
git clone https://github.com/jtrotsky/askee.git && cd askee
go build -o askee .
./askee -themes
./askee -config examples/node-exporter.json -dump 1 -size 120x30
```

Go 1.27 or later. Edit `vm` in the example to point at your database.

## Tests

```sh
go test ./...                            # every page of the example renders, at every range,
                                         # against a fake Prometheus API — no database needed
go test -run XXX -bench Group .          # one group built from scratch (about 10 ms)
node test/perf.mjs http://127.0.0.1:8095/  # a real headless Chrome: fetch, draw and live tick
                                           # for every group and range, against a budget
```

The browser test needs Node 22+ and Chrome or Chromium (`CHROME=` to point at it). It fails
if a fetch takes over 150 ms, a draw over 100 ms, or a live tick over one 16 ms frame.

## The config

```jsonc
{
  "title": "my-box",
  "vm": "http://127.0.0.1:8428",
  "default_range": "24h",
  "default_theme": "auto",                // newsprint on light systems, carbon on dark
  "thermal_range": [30, 100],             // °C mapped to the cool→hot scale, the same in every chart
  "events": [{"label": "reboot", "expr": "node_boot_time_seconds"}],  // value = a unix time
  "live_source": "http://127.0.0.1:9100/metrics?collect[]=cpu&collect[]=loadavg&collect[]=netdev",
  "snapshot": "/path/data.md",            // optional: `### key` sections written by another job
  "sizes": "/path/sizes.txt",             // optional: TARGET LOCAL REMOTE BILLED table
  "price_per_tb": 7.50, "price_label": "cold $/mo",
  "groups": [{"id": "system", "title": "System"}],
  "pages": [
    {"id": "load", "group": "system", "kind": "line", "title": "Load",
     "subtitle": "Above 4, the core count, work is queueing.",
     "series": [{"expr": "node_load1", "name": "1 min"}, {"expr": "node_load15", "name": "15 min"}],
     "refs": [{"value": 4, "label": "4 cores"}]}
  ]
}
```

| kind | draws | keys |
|---|---|---|
| `line` | time series | `series[]` (`expr` + `name`, or `label` to name each result), `refs[]` (`value` or `expr`, + `label`), `ymin`/`ymax`, `stack` for parts of a whole, `split` for one panel per series on its own scale, `thermal` to colour a line by temperature, `live` to follow the live range, `log` for a log scale |
| `hour` | 30-day mean by hour of day, as bars | `expr` |
| `capacity` | how full each thing is, as block rows | `key`: a snapshot section of `name (USED of TOTAL UNIT)` lines; `refs` in % |
| `instant` | one bar per result of an instant query | `expr`, `label`, `min_value`, `limit` |
| `runs` | one bar per run of a job, de-duplicated on the run's own timestamp | `expr`, `key` |
| `snapshot` | a section of the snapshot file; a bar figure becomes real bars | `key` |
| `sizes` | a backup sizes table as block rows, one per store, split by target | — |

Every page also takes `title` (what, in a few words), `subtitle` (so what, one sentence, shown on hover), `unit`, `range` (a fixed range
such as `30d`), `note`, and `span: [columns, rows]` to give it more of the grid.

## How it works

The binary holds everything: the web page is embedded, the colour maths and the braille
drawing are plain Go. Every expression comes from the config; the web API accepts only a group
id and a range. Each face asks the same page definition for its data, so a chart reads the same
in the browser, the terminal and a script.

## Report kit

`ascii/` is the companion for written reports: `ascii-chart.sh` draws bars, gauges, stacks,
sparklines and braille line plots in plain `sh` and `awk`, and `render.sh` turns a Markdown
subset into one self-contained HTML file. See [`ascii/README.md`](ascii/README.md).

## License

MIT
