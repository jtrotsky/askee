// askee — one chart catalogue, three faces: a btm-style terminal deck, a tiled web dashboard,
// and plain-text panels for scripts and LLM skills. Pages are declared in a JSON config and
// read from any Prometheus-compatible TSDB. Standard library only: nothing to download.
//
//	askee -config my.json                       terminal deck (←/→ page, ↑/↓ range, t theme)
//	askee -config my.json -serve 127.0.0.1:8095 web dashboard
//	askee -config my.json -print memory -range 7d
//	askee -config my.json -dump 3 -size 120x30  one page, once, no TTY
//	askee -themes                                  list themes and their validation status
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	cfgPath := flag.String("config", "askee.json", "page catalogue (JSON)")
	printG := flag.String("print", "", "print one group as plain text and exit")
	rangeF := flag.String("range", "", "range for -print (1h 6h 24h 7d 30d 90d; default from config)")
	serveA := flag.String("serve", "", "serve the web dashboard on this address")
	dump := flag.Int("dump", 0, "render page N once to stdout and exit")
	size := flag.String("size", "100x14", "width x height for -print and -dump")
	startPage := flag.Int("page", 1, "page to open on")
	listThemes := flag.Bool("themes", false, "list themes and exit")
	flag.Parse()

	if *listThemes {
		for _, t := range themes {
			fmt.Printf("%-12s %-9s %-6s %s — %s\n", t.ID, t.Status, t.Style, strings.Join(t.Palette, " "), t.Note)
		}
		return
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "askee:", err)
		os.Exit(1)
	}
	vmBase = cfg.VM
	var w, h int
	fmt.Sscanf(*size, "%dx%d", &w, &h)

	switch {
	case *printG != "":
		rn := *rangeF
		if rn == "" {
			rn = cfg.DefaultRange
		}
		r, ok := rangeByName(rn)
		if !ok {
			fmt.Fprintln(os.Stderr, "askee: unknown range", rn)
			os.Exit(2)
		}
		os.Exit(printGroup(cfg, *printG, r, w, h))
	case *serveA != "":
		if err := serve(cfg, *serveA); err != nil {
			fmt.Fprintln(os.Stderr, "askee:", err)
			os.Exit(1)
		}
	case *dump > 0:
		ps := cfg.pages()
		p := ps[min(*dump-1, len(ps)-1)]
		r, _ := rangeByName(cfg.DefaultRange)
		th := themeByID(cfg.DefaultTheme)
		if env := os.Getenv("ASKEE_THEME"); env != "" {
			th = themeByID(env)
		}
		s := newScreen(w, h, th)
		panel(s, cfg, p, p.fetch(r), 0, 0, w-1, h-1, r.name)
		out := bufio.NewWriter(os.Stdout)
		s.flush(out, true)
		fmt.Println()
	default:
		if err := runDeck(cfg, *startPage); err != nil {
			fmt.Fprintln(os.Stderr, "askee:", err)
			os.Exit(1)
		}
	}
}
