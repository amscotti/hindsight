package templates

import "testing"

func TestColumnColorAllowsSeedPalette(t *testing.T) {
	for _, color := range []string{"#e5484d", "#3e8ef7", "#46a758", "#fff"} {
		if got := columnColor(color); got != color {
			t.Errorf("columnColor(%q) = %q, want passthrough", color, got)
		}
	}
}

func TestColumnColorRejectsStyleSink(t *testing.T) {
	for _, color := range []string{
		`red; background: url(evil)`,
		`#fff; color: expression(x)`,
		"javascript:alert(1)",
		"",
		"red",
		"#12345",
	} {
		if got := columnColor(color); got != "transparent" {
			t.Errorf("columnColor(%q) = %q, want transparent", color, got)
		}
	}
}

func TestCardCountLabel(t *testing.T) {
	cases := map[int]string{0: "0 cards", 1: "1 card", 2: "2 cards"}
	for n, want := range cases {
		if got := cardCountLabel(n); got != want {
			t.Errorf("cardCountLabel(%d) = %q, want %q", n, got, want)
		}
	}
}
