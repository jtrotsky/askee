// askee — one chart catalogue, three faces: a btm-style terminal deck, a tiled web dashboard,
// and plain-text panels for scripts and LLM skills. Pages are declared in a JSON config and
// read from any Prometheus-compatible TSDB. Standard library only: nothing to download.
//
//	askee -config my.json                       terminal deck (←/→ page, ↑/↓ range, t theme)
//	askee -config my.json -serve 127.0.0.1:8095 web dashboard
//	askee -config my.json -print memory -range 7d
//	askee -config my.json -dump 3 -size 120x30  one page, once, no TTY
//	askee -vm http://127.0.0.1:8428 -serve :8095 no config: every built-in pack
//	askee -themes                                  list themes and their validation status
//	askee -packs                                   list the built-in chart packs
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	cfgPath := flag.String("config", "", "page catalogue (JSON); default: $ASKEE_CONFIG, ./askee.json, ~/.config/askee/askee.json, /usr/local/etc/askee/askee.json")
	printG := flag.String("print", "", "print one group as plain text and exit")
	rangeF := flag.String("range", "", "range for -print (live 1h 6h 24h 7d 30d 90d; default from config)")
	serveA := flag.String("serve", "", "serve the web dashboard on this address")
	dump := flag.Int("dump", 0, "render page N once to stdout and exit")
	size := flag.String("size", "100x14", "width x height for -print and -dump")
	startPage := flag.Int("page", 1, "page to open on")
	listThemes := flag.Bool("themes", false, "list themes and exit")
	listPacks := flag.Bool("packs", false, "list the built-in chart packs and exit")
	vmFlag := flag.String("vm", "", "database URL; overrides the config's, and with no config runs every built-in pack (also $ASKEE_VM)")
	flag.Parse()

	if *listThemes {
		for _, t := range themes {
			fmt.Printf("%-12s %-9s %-6s %s — %s\n", t.ID, t.Status, t.Style, strings.Join(t.Palette, " "), t.Note)
		}
		return
	}
	if *listPacks {
		for _, n := range packNames() {
			p, _ := loadPack(n)
			var gs []string
			for _, g := range p.Groups {
				gs = append(gs, g.Title)
			}
			fmt.Printf("%-8s %2d pages  %s\n", n, len(p.Pages), strings.Join(gs, ", "))
		}
		return
	}
	vm := *vmFlag
	if vm == "" {
		vm = os.Getenv("ASKEE_VM")
	}
	path := *cfgPath
	if path == "" {
		path = findConfig()
	}
	var cfg *Config
	var err error
	switch {
	case path != "":
		cfg, err = loadConfig(path)
	case vm != "":
		host, _ := os.Hostname()
		cfg, err = finishConfig(&Config{Title: host})
	default:
		fmt.Fprintln(os.Stderr, "askee: no config. Pass -vm URL to chart a database with the built-in packs, or -config FILE (also $ASKEE_CONFIG, ~/.config/askee/askee.json)")
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "askee:", err)
		os.Exit(1)
	}
	if vm != "" {
		cfg.VM = vm
	}
	if cfg.VM == "" {
		cfg.VM = "http://127.0.0.1:8428"
	}
	vmBase = cfg.VM
	// a server may start alongside its database: give it two minutes before deciding which
	// pages it can fill; a one-off command asks once
	cfg.prune(map[bool]time.Duration{true: 2 * time.Minute}[*serveA != ""])
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
		if err := serve(cfg, path, vm, *serveA); err != nil {
			fmt.Fprintln(os.Stderr, "askee:", err)
			os.Exit(1)
		}
	case *dump > 0:
		ps := cfg.pages()
		p := ps[min(*dump-1, len(ps)-1)]
		r, _ := rangeByName(cfg.DefaultRange)
		th := themeByID(map[bool]string{true: "carbon", false: cfg.DefaultTheme}[cfg.DefaultTheme == "auto"])
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

// findConfig looks where a person would put the catalogue, so a plain `askee` just works.
func findConfig() string {
	home, _ := os.UserHomeDir()
	for _, p := range []string{os.Getenv("ASKEE_CONFIG"), "askee.json",
		home + "/.config/askee/askee.json", "/usr/local/etc/askee/askee.json"} {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}
