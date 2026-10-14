package phone

import "testing"

func TestNormalizeKE(t *testing.T) {
	ok := map[string]string{
		"0712345678":       "+254712345678",
		"0112345678":       "+254112345678",
		"712345678":        "+254712345678",
		"254712345678":     "+254712345678",
		"+254712345678":    "+254712345678",
		"+254 712-345 678": "+254712345678",
	}
	for in, want := range ok {
		got, err := NormalizeKE(in)
		if err != nil || got != want {
			t.Errorf("NormalizeKE(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0812345678", "07123456", "07123456789", "+1 202 555 0100", "07a2345678"} {
		if _, err := NormalizeKE(in); err == nil {
			t.Errorf("NormalizeKE(%q) accepted invalid number", in)
		}
	}
	if m := Mask("+254712345678"); m != "+254 712 ••• 678" {
		t.Errorf("Mask = %q", m)
	}
}
