package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSizes(t *testing.T, body string) *Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sizes.txt")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Config{Sizes: p}
}

func TestSizeFieldsJoinsSpacedUnits(t *testing.T) {
	got := strings.Join(sizeFields("  photos/upload   -   1.44 TB  1.44TB   188 GB"), "|")
	if want := "photos/upload|-|1.44TB|1.44TB|188GB"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if v := parseHuman("1.44TB"); v != 1.44e12 {
		t.Fatalf("parseHuman decimal: got %v", v)
	}
}

// The old four-column file still reads. price_per_tb alone prices every remote store, not LOCAL.
func TestSizesBlocksFourColumns(t *testing.T) {
	c := writeSizes(t, `BACKUP SIZES
  TARGET   LOCAL   NAS   COLD
  ─────────
  a        1GB     2GB   2GB
  TOTAL    1GB     2GB   2GB
`)
	c.PricePerTB = 10
	bl, _, _, err := sizesBlocks(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(bl) != 3 {
		t.Fatalf("want 3 blocks, got %d", len(bl))
	}
	if strings.Contains(bl[0].Text, "$") || !strings.Contains(bl[1].Text, "$0.02/mo") || !strings.Contains(bl[2].Text, "$0.02/mo") {
		t.Fatalf("pricing: local %q nas %q cold %q", bl[0].Text, bl[1].Text, bl[2].Text)
	}
}

// Six columns with spaced units (the current sizes-file format) and size_prices.
func TestSizesBlocksSixColumnsSpaced(t *testing.T) {
	c := writeSizes(t, `  TARGET  LOCAL  NAS  COLD  VAULT  SYNC
  photos  -  1.44 TB  1.44 TB  1.44 TB  -
  db      17 GB  17 GB  17 GB  -  17 GB
  TOTAL   17 GB  1.46 TB  1.46 TB  1.44 TB  17 GB
`)
	c.SizePrices = map[string]float64{"VAULT": 10, "SYNC": 10, "COLD": 0}
	c.SizeLabels = map[string]string{"VAULT": "vault"}
	bl, _, note, err := sizesBlocks(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(bl) != 5 {
		t.Fatalf("want 5 blocks, got %d", len(bl))
	}
	if bl[3].Label != "vault" || bl[4].Label != "sync" {
		t.Fatalf("labels %q %q", bl[3].Label, bl[4].Label)
	}
	if !strings.HasPrefix(bl[3].Text, "1.44 TB · $14.40/mo") {
		t.Fatalf("vault text %q", bl[3].Text)
	}
	if !strings.HasSuffix(bl[2].Text, "$0.00/mo") {
		t.Fatalf("cold at $0/TB should say so: %q", bl[2].Text)
	}
	if strings.Contains(bl[1].Text, "$") {
		t.Fatalf("NAS has no price and should show none: %q", bl[1].Text)
	}
	if !strings.Contains(note, "photos is the NAS figure") {
		t.Fatalf("note %q", note)
	}
}
