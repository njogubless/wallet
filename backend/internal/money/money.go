// Package money implements exact monetary arithmetic on integer minor units.
// Floating point is never used for amounts.
package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Currency describes an ISO 4217 currency and its precision.
type Currency struct {
	Code       string
	MinorUnits int
}

// KES: Kenyan Shilling, 2 decimal places (cents). Mobile-money rails generally
// settle in whole shillings; providers enforce that in their adapters.
var KES = Currency{Code: "KES", MinorUnits: 2}

var currencies = map[string]Currency{"KES": KES}

var (
	ErrUnknownCurrency = errors.New("unsupported currency")
	ErrInvalidAmount   = errors.New("invalid amount format")
	ErrTooPrecise      = errors.New("amount has more decimal places than the currency supports")
	ErrOutOfRange      = errors.New("amount out of range")
	ErrNotPositive     = errors.New("amount must be greater than zero")
	ErrOverflow        = errors.New("arithmetic overflow")
)

// MaxMinor bounds any single amount (10^14 minor units). Keeping amounts well
// below int64 range lets fee calculations multiply by basis points safely.
const MaxMinor int64 = 100_000_000_000_000

func LookupCurrency(code string) (Currency, error) {
	c, ok := currencies[strings.ToUpper(strings.TrimSpace(code))]
	if !ok {
		return Currency{}, ErrUnknownCurrency
	}
	return c, nil
}

func (c Currency) factor() int64 {
	f := int64(1)
	for i := 0; i < c.MinorUnits; i++ {
		f *= 10
	}
	return f
}

// Parse converts a decimal string in major units (e.g. "5000", "5000.5",
// "5000.50") to minor units. Signs, exponents, separators and excess
// precision are rejected rather than rounded.
func Parse(s string, c Currency) (int64, error) {
	if s == "" || len(s) > 32 {
		return 0, ErrInvalidAmount
	}
	intPart, fracPart, hasDot := strings.Cut(s, ".")
	if intPart == "" || (hasDot && fracPart == "") {
		return 0, ErrInvalidAmount
	}
	for _, r := range intPart + fracPart {
		if r < '0' || r > '9' {
			return 0, ErrInvalidAmount
		}
	}
	if len(intPart) > 1 && intPart[0] == '0' {
		return 0, ErrInvalidAmount
	}
	if len(fracPart) > c.MinorUnits {
		// Strict even for trailing zeros, so clients learn the precision contract.
		return 0, ErrTooPrecise
	}
	if len(intPart) > 15 {
		return 0, ErrOutOfRange
	}
	major, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, ErrOutOfRange
	}
	frac := int64(0)
	if fracPart != "" {
		padded := fracPart + strings.Repeat("0", c.MinorUnits-len(fracPart))
		frac, err = strconv.ParseInt(padded, 10, 64)
		if err != nil {
			return 0, ErrInvalidAmount
		}
	}
	if major > (MaxMinor / c.factor()) {
		return 0, ErrOutOfRange
	}
	v := major*c.factor() + frac
	if v > MaxMinor {
		return 0, ErrOutOfRange
	}
	return v, nil
}

// ParsePositive is Parse that also rejects zero.
func ParsePositive(s string, c Currency) (int64, error) {
	v, err := Parse(s, c)
	if err != nil {
		return 0, err
	}
	if v <= 0 {
		return 0, ErrNotPositive
	}
	return v, nil
}

// Format renders minor units as a plain decimal string ("5000.00").
// It is a wire format, not a display format.
func Format(minor int64, c Currency) string {
	neg := minor < 0
	if neg {
		if minor == math.MinInt64 {
			return "overflow"
		}
		minor = -minor
	}
	f := c.factor()
	s := strconv.FormatInt(minor/f, 10)
	if c.MinorUnits > 0 {
		s += "." + fmt.Sprintf("%0*d", c.MinorUnits, minor%f)
	}
	if neg {
		s = "-" + s
	}
	return s
}

// Add returns a+b, failing on overflow.
func Add(a, b int64) (int64, error) {
	s := a + b
	if (b > 0 && s < a) || (b < 0 && s > a) {
		return 0, ErrOverflow
	}
	return s, nil
}

// IsWholeMajor reports whether the amount has no fractional part.
func IsWholeMajor(minor int64, c Currency) bool { return minor%c.factor() == 0 }

// ToWholeMajor converts to whole major units, failing if a fraction exists.
func ToWholeMajor(minor int64, c Currency) (int64, error) {
	if !IsWholeMajor(minor, c) {
		return 0, ErrTooPrecise
	}
	return minor / c.factor(), nil
}

// FromWholeMajor converts whole major units to minor units.
func FromWholeMajor(major int64, c Currency) (int64, error) {
	if major < 0 || major > MaxMinor/c.factor() {
		return 0, ErrOutOfRange
	}
	return major * c.factor(), nil
}

// ApplyBasisPoints returns ceil(amount * bps / 10000). Fees round up to the
// next minor unit so the platform never under-collects by a fraction.
func ApplyBasisPoints(amount int64, bps int) (int64, error) {
	if amount < 0 || amount > MaxMinor || bps < 0 || bps > 10000 {
		return 0, ErrOutOfRange
	}
	p := amount * int64(bps) // <= 1e18, fits int64
	q := p / 10000
	if p%10000 != 0 {
		q++
	}
	return q, nil
}
