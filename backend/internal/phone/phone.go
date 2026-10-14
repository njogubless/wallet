// Package phone normalises Kenyan mobile numbers to E.164.
package phone

import (
	"errors"
	"strings"
)

var ErrInvalid = errors.New("invalid Kenyan mobile number")

// NormalizeKE accepts 07XXXXXXXX, 01XXXXXXXX, 7XXXXXXXX, 2547XXXXXXXX and
// +2547XXXXXXXX (spaces and dashes ignored) and returns +254XXXXXXXXX.
func NormalizeKE(s string) (string, error) {
	s = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "+")
	switch {
	case strings.HasPrefix(s, "254"):
		s = s[3:]
	case strings.HasPrefix(s, "0"):
		s = s[1:]
	}
	if len(s) != 9 || (s[0] != '7' && s[0] != '1') {
		return "", ErrInvalid
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return "", ErrInvalid
		}
	}
	return "+254" + s, nil
}

// MSISDN returns the number without '+' (e.g. 254712345678), as Daraja expects.
func MSISDN(e164 string) string { return strings.TrimPrefix(e164, "+") }

// National returns the 9-digit subscriber number (e.g. 712345678), as Airtel expects.
func National(e164 string) string { return strings.TrimPrefix(e164, "+254") }

// Mask renders +254 712 ••• 678 for display.
func Mask(e164 string) string {
	n := National(e164)
	if len(n) != 9 {
		return "•••"
	}
	return "+254 " + n[:3] + " ••• " + n[6:]
}
