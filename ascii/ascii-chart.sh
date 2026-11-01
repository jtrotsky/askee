#!/bin/sh
# ascii-chart.sh — text-only charts for reports and terminals. No dependencies
# beyond sh + awk, so it runs anywhere, including a recovery shell with no
# browser and no network.
#
# WHY TEXT. Hand-placed SVG goes wrong in coordinate arithmetic and colliding
# labels. Text charts have no coordinates to get wrong: a column is a column. They also paste into a terminal, an email, a commit
# message or a runbook unchanged, which SVG does not.
#
# HOW THE SUB-CELL RESOLUTION WORKS. Two Unicode families give more than one
# pixel per character cell:
#   • Block elements U+2588..U+258F — eight widths of a left-aligned block, so a
#     horizontal bar resolves to 1/8 of a character.
#   • Braille patterns U+2800..U+28FF — a 2x4 dot matrix per cell, so a line plot
#     resolves to 2 across and 4 down per character. This is what btm and gping
#     use to draw smooth graphs in a terminal.
# awk cannot portably emit \uXXXX, so the UTF-8 bytes are assembled by hand:
# U+2800+n is 0xE2, 0xA0|(n>>6), 0x80|(n&0x3F). Verified byte-exact on FreeBSD
# awk; do not "simplify" this to printf "%c" with a codepoint, which emits one
# truncated byte and produces mojibake.
#
# EVERY CHART IS ALSO READABLE WITH NO GLYPHS AT ALL. Pass -a for ASCII-only
# mode (# and . instead of blocks and braille), for a console with no UTF-8, for
# `grep`, and for anyone using a screen reader. The numbers are always printed
# beside the bars, never encoded only as length — the same rule as a chart legend.
#
# USAGE (all read stdin; fields are TAB-separated)
#   hbar    [-w cols] [-u unit] [-a]     "label<TAB>value"        one bar per line
#   meter   [-w cols] [-a]               "label<TAB>value<TAB>max" one gauge per line
#   stack   [-w cols] [-a]               "label<TAB>value"        one proportional bar
#   braille [-w cols] [-h rows] [-a]     "value"                  one value per line
#   spark   [-a]                         "value"                  inline sparkline
#
# EXAMPLES
#   printf 'photos\t1421\ndocs\t212\n' | sh ascii-chart.sh hbar -u MB
#   zpool list -Hp -o name,capacity | sh ascii-chart.sh meter
#   awk '{print $2}' latency.txt | sh ascii-chart.sh braille -h 6
set -u
# BYTE SEMANTICS, ALWAYS (2026-11-01). Every glyph below is assembled from bytes with
# sprintf("%c", 226) etc. FreeBSD's awk became UTF-8 aware (onetrue-awk 20250804): under a
# UTF-8 locale %c of 226 now emits the CODEPOINT U+00E2 as two bytes, so each glyph came out
# double-encoded ("â\x96\x88" instead of "█") — exactly when run the way the README advises.
# Forcing the C locale restores the byte behaviour the code was written for. Cost: label
# widths are counted in bytes, so a label with a multibyte character pads one short.
export LC_ALL=C

CMD="${1:-}"; [ -n "$CMD" ] && shift
W=48; H=6; UNIT=""; ASCII=0
while [ $# -gt 0 ]; do
    case "$1" in
        -w) W="$2"; shift 2 ;;
        -h) H="$2"; shift 2 ;;
        -u) UNIT="$2"; shift 2 ;;
        -a) ASCII=1; shift ;;
        *)  echo "ascii-chart: unknown option '$1'" >&2; exit 2 ;;
    esac
done

case "$CMD" in
# ── hbar ─────────────────────────────────────────────────────────────────────
# Horizontal bars, longest first is the CALLER's job — this preserves input
# order, because for a report the row order usually carries meaning (engine,
# severity, chronology) and silently re-sorting would destroy it.
hbar)
    awk -v W="$W" -v UNIT="$UNIT" -v ASCII="$ASCII" '
    BEGIN { FS="\t"; n=0; max=0 }
    { lab[n]=$1; val[n]=$2+0; if (val[n]>max) max=val[n]
      if (length($1)>lw) lw=length($1); n++ }
    function blk(k) {   # k = eighths of a cell, 1..8 -> U+2588+(8-k)
        return sprintf("%c%c%c", 226, 150, 128 + (8 - k) + 8)
    }
    function bar(v,  cells, eighths, s, i, full) {
        if (max <= 0) return ""
        eighths = int((v / max) * W * 8 + 0.5)
        full = int(eighths / 8); rem = eighths % 8
        s = ""
        if (ASCII) { for (i=0;i<full;i++) s = s "#"; if (rem>0) s = s "-"; return s }
        for (i=0;i<full;i++) s = s sprintf("%c%c%c", 226, 150, 136)   # U+2588 full
        if (rem > 0) s = s blk(rem)
        return s
    }
    END {
        for (i=0;i<n;i++) {
            v = val[i]
            lbl = (v == int(v)) ? sprintf("%d", v) : sprintf("%.1f", v)
            printf "%-*s %s  %s%s\n", lw, lab[i], bar(v), lbl, (UNIT==""?"":" " UNIT)
        }
    }'
    ;;

# ── meter ────────────────────────────────────────────────────────────────────
# One gauge per line against its own max, so unlike hbar the bars are comparable
# as PERCENTAGES rather than absolute values. Use this for "how full is it".
meter)
    # Two-pass so the label column is sized to the longest label. The first version
    # hardcoded %-16s, which silently pushed the bars out of alignment the moment a
    # label was longer — and a misaligned bar chart is harder to read than no chart.
    awk -v W="$W" -v ASCII="$ASCII" '
    BEGIN { FS="\t"; n=0; lw=0 }
    { lab[n]=$1; val[n]=$2+0; max[n]=$3+0
      if (length($1)>lw) lw=length($1); n++ }
    END {
        for (i=0;i<n;i++) {
            if (max[i]<=0) { printf "%-*s  (no maximum given)\n", lw, lab[i]; continue }
            pct = val[i]/max[i]*100; if (pct>100) pct=100
            filled = int(pct/100*W + 0.5)
            s=""
            for (j=0;j<filled;j++) s = s (ASCII ? "#" : sprintf("%c%c%c",226,150,136))
            for (j=filled;j<W;j++) s = s (ASCII ? "." : sprintf("%c%c%c",226,148,128))
            printf "%-*s [%s] %5.1f%%\n", lw, lab[i], s, pct
        }
    }'
    ;;

# ── stack ────────────────────────────────────────────────────────────────────
# A single proportional bar: all values as shares of their own total. Segments
# are told apart by DIFFERENT GLYPHS, not colour, and the key is printed under
# the bar — so it survives greyscale, a screen reader and a plain-text paste.
stack)
    awk -v W="$W" -v ASCII="$ASCII" '
    BEGIN { FS="\t"; n=0; tot=0
            g[0]="#"; g[1]="="; g[2]="+"; g[3]="-"; g[4]=":"; g[5]="."
            if (!ASCII) { u[0]=sprintf("%c%c%c",226,150,136); u[1]=sprintf("%c%c%c",226,150,147)
                          u[2]=sprintf("%c%c%c",226,150,146); u[3]=sprintf("%c%c%c",226,150,145)
                          u[4]="+"; u[5]="." } }
    { lab[n]=$1; val[n]=$2+0; tot+=val[n]; n++ }
    END {
        if (tot<=0) { print "(nothing to show)"; exit }
        bar=""; used=0
        for (i=0;i<n;i++) {
            w = (i==n-1) ? W-used : int(val[i]/tot*W + 0.5)
            if (w<0) w=0; used+=w
            ch = ASCII ? g[i%6] : u[i%6]
            for (j=0;j<w;j++) bar = bar ch
        }
        print bar
        for (i=0;i<n;i++) {
            ch = ASCII ? g[i%6] : u[i%6]
            printf "  %s %s  %.1f%%\n", ch, lab[i], val[i]/tot*100
        }
    }'
    ;;

# ── braille / spark ──────────────────────────────────────────────────────────
# A real line plot in text. The canvas is W cells wide and H cells tall, i.e.
# 2W x 4H dots. Dot bit order within a cell is the braille standard (NOT
# row-major): bits 0,1,2 then 6 down the left column; 3,4,5 then 7 down the
# right. Getting that wrong gives a plot that looks plausible and is subtly
# scrambled, which is worse than one that looks broken.
braille|spark)
    [ "$CMD" = spark ] && { W=0; H=1; }
    awk -v W="$W" -v H="$H" -v ASCII="$ASCII" -v MODE="$CMD" '
    BEGIN { n=0; min=1e18; max=-1e18 }
    { v=$1+0; d[n++]=v; if (v<min) min=v; if (v>max) max=v }
    END {
        if (n==0) { print "(no data)"; exit }
        if (W<=0) W=n<60?n:60
        if (max==min) { max=min+1 }
        # ASCII mode gets ONE dot row per text row, so -h means the same thing in both
        # modes: rows of output. The first version made -h mean cells, so `-h 4` printed
        # 16 ASCII rows and 4 braille rows — the same flag with two meanings.
        DX = ASCII ? W : 2*W
        DY = ASCII ? H : 4*H
        for (x=0; x<DX; x++) {
            idx = (DX==1) ? 0 : x*(n-1)/(DX-1)
            lo=int(idx); hi=lo+1; if (hi>n-1) hi=n-1
            f = idx-lo
            v = d[lo]*(1-f) + d[hi]*f
            yy = int((v-min)/(max-min)*(DY-1) + 0.5)
            row[x] = DY-1-yy          # 0 = top dot row
        }
        # CONNECT the line. Plotting one dot per column gives a scatter, not a line:
        # with a spiky series it reads as noise, which is how the first version looked.
        # For each column, fill every dot row between this sample and the midpoint of
        # the step to each neighbour — so a steep segment draws as a near-vertical
        # stroke instead of two orphaned dots. lo_[]/hi_[] is the inclusive span drawn.
        for (x=0; x<DX; x++) {
            a = (x>0)    ? int((row[x] + row[x-1]) / 2) : row[x]
            b = (x<DX-1) ? int((row[x] + row[x+1]) / 2) : row[x]
            lo_[x] = row[x]; hi_[x] = row[x]
            if (a < lo_[x]) lo_[x] = a; if (a > hi_[x]) hi_[x] = a
            if (b < lo_[x]) lo_[x] = b; if (b > hi_[x]) hi_[x] = b
        }
        if (ASCII) {
            for (r=0; r<DY; r++) {
                line=""
                for (x=0; x<DX; x++)
                    line = line ((r >= lo_[x] && r <= hi_[x]) ? "*" : " ")
                sub(/ +$/,"",line); printf "%s\n", line
            }
            printf "  min %g   max %g   n=%d\n", min, max, n
            exit
        }
        # braille bit for (col 0|1, dotrow 0..3)
        split("0 1 2 6", L, " "); split("3 4 5 7", R, " ")
        for (cy=0; cy<H; cy++) {
            line=""
            for (cx=0; cx<W; cx++) {
                bits=0
                for (sub_=0; sub_<2; sub_++) {
                    x = cx*2 + sub_
                    if (x >= DX) continue
                    for (rr = lo_[x]; rr <= hi_[x]; rr++) {
                        r = rr - cy*4
                        if (r>=0 && r<4) {
                            b = (sub_==0) ? L[r+1] : R[r+1]
                            bits = or(bits, lshift(1, b))
                        }
                    }
                }
                line = line sprintf("%c%c%c", 226, 160 + int(bits/64), 128 + (bits%64))
            }
            printf "%s\n", line
        }
        if (MODE=="braille") printf "  min %g   max %g   n=%d\n", min, max, n
    }'
    ;;
*)
    sed -n '1,40p' "$0" >&2
    exit 2 ;;
esac
