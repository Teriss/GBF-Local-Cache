package host

import "testing"

func TestWhitelistExactAndControlledSubdomainRules(t *testing.T) {
	w := New([]string{"PRD-GAME-A-GRANBLUEFANTASY.akamaized.net"}, []string{"assets.example.test"})
	tests := []struct {
		name  string
		allow bool
	}{
		{"prd-game-a-granbluefantasy.akamaized.net", true},
		{"prd-game-a-granbluefantasy.akamaized.net.", true},
		{"assets.example.test", true},
		{"img.assets.example.test", true},
		{"example.test", false},
		{"evilakamaized.net", false},
		{"foo.akamaized.net", false},
		{"game.granbluefantasy.jp", false},
		{"assets.example.test.evil", false},
		{"assets.example.test/path", false},
		{"xn--eckwd4c7c.xn--example", false},
		{"127.0.0.1", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := w.Allowed(test.name); got != test.allow {
				t.Fatalf("Allowed(%q) = %v, want %v", test.name, got, test.allow)
			}
		})
	}
}
