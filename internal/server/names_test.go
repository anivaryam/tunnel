package server

import "testing"

func TestValidName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"simple lowercase", "eservice", true},
		{"with hyphen", "borongan-hr", true},
		{"with digits", "app123", true},
		{"single char", "a", true},
		{"two chars", "ab", true},
		{"32 chars max", "abcdefghijabcdefghijabcdefghij12", true},
		{"33 chars too long", "abcdefghijabcdefghijabcdefghij123", false},
		{"empty", "", false},
		{"uppercase rejected", "Eservice", false},
		{"underscore rejected", "my_app", false},
		{"leading hyphen rejected", "-foo", false},
		{"trailing hyphen rejected", "foo-", false},
		{"dot rejected", "foo.bar", false},
		{"space rejected", "foo bar", false},
		{"reserved dashboard", "dashboard", false},
		{"reserved metrics", "metrics", false},
		{"reserved health", "health", false},
		{"reserved t", "t", false},
		{"reserved ws", "ws", false},
		{"reserved api", "api", false},
		{"reserved www", "www", false},
		{"reserved admin", "admin", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidName(tc.in); got != tc.want {
				t.Fatalf("ValidName(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
