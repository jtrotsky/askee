#!/bin/sh
# render.sh — Markdown subset -> one self-contained HTML file, theme inlined.
#
#   sh ascii/render.sh report.md > report.html
#   sh ascii/render.sh report.md --title "Custom title" > report.html
#
# WHY A HAND-WRITTEN RENDERER. A report should be producible from a recovery shell, with
# no pandoc, cmark or python-markdown installed, so a dependency is the wrong trade. This is ~150 lines of awk
# and it handles exactly the subset described in README.md.
#
# The output is ONE file: the theme is read and inlined, so the HTML can be copied
# anywhere, opened offline, or mailed. No <script> at all, so it renders even where a strict
# Content-Security-Policy blocks inline script (many file browsers serve one).
#
# SUBSET: # ## ### #### · paragraphs · ``` fenced blocks · | tables | · - and 1. lists ·
# **bold** · *italic* · `code` · [text](url) · > quote · --- rule · any line starting
# with `<` passes through raw.
set -u

KITDIR=$(dirname "$0")
SRC="${1:-}"
[ -n "$SRC" ] && [ -r "$SRC" ] || { echo "usage: render.sh <file.md> [--title T]" >&2; exit 2; }
shift
TITLE=""
while [ $# -gt 0 ]; do
    case "$1" in
        --title) TITLE="$2"; shift 2 ;;
        *) echo "render.sh: unknown option '$1'" >&2; exit 2 ;;
    esac
done
# default title: the first H1, else the filename
[ -n "$TITLE" ] || TITLE=$(awk '/^# /{sub(/^# /,"");print;exit}' "$SRC")
[ -n "$TITLE" ] || TITLE=$(basename "$SRC" .md)

CSS="$KITDIR/theme.css"
[ -r "$CSS" ] || { echo "render.sh: cannot read $CSS" >&2; exit 1; }

# WARN on bold or code spans that straddle a line break. The renderer works a line at a
# time, so `**text` on one line and `text**` on the next cannot match and the asterisks
# are emitted literally — which looks like a typo in the output and is easy to miss in a
# 7,000-word document. Three of these survived the first draft of this report.
# A warning rather than an error: the output is still usable, and the fix belongs in the
# Markdown (rewrap the line) rather than in a renderer that guesses across lines.
#
# The first version of this check fired on every ``` fence line, because a fence has three
# backticks and three is odd. Twenty false warnings on the first real document, which is
# how a guard gets ignored. Skip fence markers and everything between them: a chart is
# full of characters that mean nothing to Markdown and must not be inspected.
awk '
    /^```/ { infence = !infence; next }
    infence { next }
    { n = gsub(/\*\*/, "**"); if (n % 2 == 1)
        printf "render.sh: WARNING line %d has an unclosed ** (bold across a line break): %s\n", \
               NR, substr($0, 1, 60) > "/dev/stderr"
      m = gsub(/`/, "`"); if (m % 2 == 1)
        printf "render.sh: WARNING line %d has an unclosed ` (code span across a line break): %s\n", \
               NR, substr($0, 1, 60) > "/dev/stderr" }
' "$SRC"

# ── head ─────────────────────────────────────────────────────────────────────────
printf '<!DOCTYPE html>\n<html lang="en-NZ">\n<head>\n<meta charset="utf-8">\n'
printf '<meta name="viewport" content="width=device-width, initial-scale=1">\n'
printf '<title>%s</title>\n<style>\n' "$TITLE"
cat "$CSS"
printf '</style>\n</head>\n<body>\n<main>\n'

# ── body ─────────────────────────────────────────────────────────────────────────
awk '
function esc(s) {
    gsub(/&/, "\\&amp;", s); gsub(/</, "\\&lt;", s); gsub(/>/, "\\&gt;", s)
    return s
}
# Inline formatting. Order matters: code spans FIRST so their contents are not then
# treated as emphasis, and links before emphasis so a URL with _ survives.
function inl(s,   out, n, i, parts) {
    s = esc(s)
    # `code`
    while (match(s, /`[^`]+`/)) {
        t = substr(s, RSTART+1, RLENGTH-2)
        s = substr(s, 1, RSTART-1) "\001code\002" t "\001/code\002" substr(s, RSTART+RLENGTH)
    }
    # [text](url)
    while (match(s, /\[[^]]+\]\([^)]+\)/)) {
        full = substr(s, RSTART, RLENGTH)
        p = index(full, "](")
        txt = substr(full, 2, p-2)
        url = substr(full, p+2, length(full)-p-2)
        s = substr(s, 1, RSTART-1) "\001a href=\"" url "\"\002" txt "\001/a\002" substr(s, RSTART+RLENGTH)
    }
    # **bold**
    while (match(s, /\*\*[^*]+\*\*/)) {
        t = substr(s, RSTART+2, RLENGTH-4)
        s = substr(s, 1, RSTART-1) "\001strong\002" t "\001/strong\002" substr(s, RSTART+RLENGTH)
    }
    # *italic*
    while (match(s, /\*[^*]+\*/)) {
        t = substr(s, RSTART+1, RLENGTH-2)
        s = substr(s, 1, RSTART-1) "\001em\002" t "\001/em\002" substr(s, RSTART+RLENGTH)
    }
    # STATUS SHORTHAND: {ok ✓ Good} {warn ▲ Watch} {bad ✕ Fix needed}
    # A shorthand rather than raw inline HTML, deliberately. esc() escapes everything,
    # so a <span> in a table cell came out as literal text — and the fix is NOT to stop
    # escaping, which would let any markup through. This reads fine in the Markdown
    # artefact too, which matters because the .md is a first-class output here.
    # The class set is closed: only ok/warn/bad, each defined in theme.css to carry a
    # colour, and the author still writes the symbol and the word themselves, so the
    # "never colour alone" rule cannot be broken by accident.
    while (match(s, /\{(ok|warn|bad)[[:space:]][^}]*\}/)) {
        full = substr(s, RSTART, RLENGTH)
        inner = substr(full, 2, length(full)-2)
        sp = index(inner, " ")
        cls = substr(inner, 1, sp-1)
        txt = substr(inner, sp+1)
        s = substr(s, 1, RSTART-1) "\001span class=\"" cls "\"\002" txt "\001/span\002" substr(s, RSTART+RLENGTH)
    }
    gsub(/\001/, "<", s); gsub(/\002/, ">", s)
    return s
}
function closepara() { if (inpara) { print "</p>"; inpara = 0 } }
function closelist() {
    if (inul) { print "</ul>"; inul = 0 }
    if (inol) { print "</ol>"; inol = 0 }
}
function closequote() { if (inquote) { print "</blockquote>"; inquote = 0 } }
function closetable() {
    if (intable) { print "</table>"; intable = 0; thead = 0 }
}
function closeall() { closepara(); closelist(); closequote(); closetable() }

BEGIN { inpara=0; inul=0; inol=0; inquote=0; intable=0; incode=0; thead=0; lede_ok=0 }

# fenced code: everything inside is emitted verbatim-escaped, no inline processing,
# because a chart contains characters that would otherwise be eaten as markup.
/^```/ {
    if (incode) { print "</pre>"; incode = 0 }
    else { closeall(); lede_ok=0; print "<pre>"; incode = 1 }
    next
}
incode { print esc($0); next }

# raw HTML passthrough
/^</ { closeall(); print; next }

# lede_ok is set by the H1 and cleared by ANY other block, so only a paragraph
# directly after the title becomes the lede. Without this a table between the H1 and
# the first paragraph left the flag set and the wrong paragraph got the class.
/^#### / { closeall(); lede_ok=0; sub(/^#### /,""); print "<h4>" inl($0) "</h4>"; next }
/^### /  { closeall(); lede_ok=0; sub(/^### /,"");  print "<h3>" inl($0) "</h3>"; next }
/^## /   { closeall(); lede_ok=0; sub(/^## /,"");   print "<h2>" inl($0) "</h2>"; next }
/^# /    {
    closeall(); sub(/^# /,"")
    print "<h1>" inl($0) "</h1>"
    lede_ok = 1
    next
}

/^---+[[:space:]]*$/ { closeall(); lede_ok=0; print "<hr>"; next }

# table: a header row, then a |---|---| separator, then body rows
/^\|/ {
    closepara(); closelist(); closequote(); lede_ok=0
    line = $0
    sub(/^\|/, "", line); sub(/\|[[:space:]]*$/, "", line)
    if (line ~ /^[[:space:]]*:?-+:?[[:space:]]*(\|[[:space:]]*:?-+:?[[:space:]]*)*$/) {
        thead = 0; next          # separator row: switch from <th> to <td>
    }
    if (!intable) { print "<table>"; intable = 1; thead = 1 }
    n = split(line, cell, /\|/)
    printf "<tr>"
    for (i = 1; i <= n; i++) {
        c = cell[i]; gsub(/^[[:space:]]+|[[:space:]]+$/, "", c)
        printf "%s%s%s", (thead ? "<th>" : "<td>"), inl(c), (thead ? "</th>" : "</td>")
    }
    print "</tr>"
    next
}
intable && !/^\|/ { closetable() }

/^> / {
    closepara(); closelist(); lede_ok=0
    if (!inquote) { print "<blockquote>"; inquote = 1 }
    sub(/^> /, "")
    print "<p>" inl($0) "</p>"
    next
}
inquote && !/^> / { closequote() }

/^[-*] / {
    closepara(); closequote(); lede_ok=0
    if (inol) { print "</ol>"; inol = 0 }
    if (!inul) { print "<ul>"; inul = 1 }
    sub(/^[-*] /, "")
    print "<li>" inl($0) "</li>"
    next
}
/^[0-9]+\. / {
    closepara(); closequote(); lede_ok=0
    if (inul) { print "</ul>"; inul = 0 }
    if (!inol) { print "<ol>"; inol = 1 }
    sub(/^[0-9]+\. /, "")
    print "<li>" inl($0) "</li>"
    next
}
/^[[:space:]]*$/ { closepara(); closelist(); closequote(); next }

{
    # the paragraph immediately after the first H1 becomes the lede
    if (!inpara) {
        if (lede_ok) { print "<p class=\"lede\">"; lede_ok = 0 }
        else print "<p>"
        inpara = 1
    }
    print inl($0)
}
END { closeall(); if (incode) print "</pre>" }
' "$SRC"

printf '</main>\n</body>\n</html>\n'
