package torrentzip

import "testing"

func TestEncodeName(t *testing.T) {
	t.Parallel()

	// A name that needs UTF-8, built from runes as gosmopolitan rejects
	// Han script in string literals.
	cjk := string([]rune{0x65e5, 0x672c, 0x8a9e})

	tests := []struct {
		name string
		raw  string
		utf8 bool
	}{
		{"plain.bin", "plain.bin", false},
		{"café", "caf\x82", false},
		{"\u263a\u00a7", "\x01\x15", false}, // glyphs for 0x01 and 0x15
		{"tab\there", "tab\there", true},    // control characters aren't code page 437
		{"\x7f", "\x7f", false},
		{cjk, cjk, true},
		{"\U0001F600", "\U0001F600", true},
	}
	for _, tt := range tests {
		raw, utf8 := encodeName(tt.name)
		if string(raw) != tt.raw || utf8 != tt.utf8 {
			t.Errorf("encodeName(%q) = %q, %v; want %q, %v", tt.name, raw, utf8, tt.raw, tt.utf8)
		}
	}
}

func TestDecodeCP437(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"plain.rom", "café", "☺§", "░▒▓"} {
		raw, utf8 := encodeName(name)
		if utf8 {
			t.Fatalf("%q unexpectedly needs utf-8", name)
		}

		if got := decodeCP437(string(raw)); got != name {
			t.Errorf("decodeCP437(%q) = %q, want %q", raw, got, name)
		}
	}
}
