package domain

import "testing"

func TestIsValidPlatform(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"telegram", PlatformTelegram, true},
		{"lark", PlatformLark, true},
		{"telegram literal", "telegram", true},
		{"lark literal", "lark", true},
		{"empty", "", false},
		{"discord (not yet supported)", "discord", false},
		{"slack (not yet supported)", "slack", false},
		{"case mismatch", "Telegram", false},
		{"case mismatch lark", "Lark", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := IsValidPlatform(tc.in)
			if got != tc.want {
				t.Fatalf("IsValidPlatform(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestPlatformConstants guards against accidental renames — store/migrations
// and bot UI templates pin these literal strings, so a rename would silently
// break the bots.platform column allow-list.
func TestPlatformConstants(t *testing.T) {
	if PlatformTelegram != "telegram" {
		t.Fatalf("PlatformTelegram = %q, want %q", PlatformTelegram, "telegram")
	}
	if PlatformLark != "lark" {
		t.Fatalf("PlatformLark = %q, want %q", PlatformLark, "lark")
	}
}
