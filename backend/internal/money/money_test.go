package money

import (
	"errors"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		err  error
	}{
		{"0", 0, nil},
		{"1", 100, nil},
		{"5000", 500000, nil},
		{"5000.5", 500050, nil},
		{"5000.50", 500050, nil},
		{"0.01", 1, nil},
		{"0.10", 10, nil},
		{"5000.505", 0, ErrTooPrecise},
		{"5000.500", 0, ErrTooPrecise},
		{"-1", 0, ErrInvalidAmount},
		{"+1", 0, ErrInvalidAmount},
		{"1e3", 0, ErrInvalidAmount},
		{"1,000", 0, ErrInvalidAmount},
		{" 1", 0, ErrInvalidAmount},
		{"1.", 0, ErrInvalidAmount},
		{".5", 0, ErrInvalidAmount},
		{"01", 0, ErrInvalidAmount},
		{"", 0, ErrInvalidAmount},
		{"NaN", 0, ErrInvalidAmount},
		{"1000000000000", 100000000000000, nil},
		{"1000000000001", 0, ErrOutOfRange},
		{"9999999999999999999", 0, ErrOutOfRange},
	}
	for _, tc := range cases {
		got, err := Parse(tc.in, KES)
		if !errors.Is(err, tc.err) {
			t.Errorf("Parse(%q) err = %v, want %v", tc.in, err, tc.err)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("Parse(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParsePositiveRejectsZero(t *testing.T) {
	if _, err := ParsePositive("0.00", KES); !errors.Is(err, ErrNotPositive) {
		t.Fatalf("expected ErrNotPositive, got %v", err)
	}
}

func TestFormatRoundTrip(t *testing.T) {
	for _, v := range []int64{0, 1, 10, 99, 100, 500050, 123456789, MaxMinor} {
		s := Format(v, KES)
		back, err := Parse(s, KES)
		if err != nil || back != v {
			t.Errorf("round trip %d -> %q -> %d (%v)", v, s, back, err)
		}
	}
	if got := Format(-150, KES); got != "-1.50" {
		t.Errorf("Format(-150) = %q", got)
	}
}

func TestApplyBasisPointsRoundsUp(t *testing.T) {
	cases := []struct {
		amount int64
		bps    int
		want   int64
	}{
		{10000, 50, 50}, // 0.5% of 100.00 = 0.50
		{10001, 50, 51}, // 50.005 -> 51 (ceil)
		{1, 1, 1},       // tiny fee still charged
		{0, 50, 0},      // zero amount
		{MaxMinor, 10000, MaxMinor},
	}
	for _, tc := range cases {
		got, err := ApplyBasisPoints(tc.amount, tc.bps)
		if err != nil || got != tc.want {
			t.Errorf("ApplyBasisPoints(%d,%d) = %d,%v want %d", tc.amount, tc.bps, got, err, tc.want)
		}
	}
	if _, err := ApplyBasisPoints(MaxMinor+1, 1); err == nil {
		t.Error("expected out of range")
	}
}

func TestAddOverflow(t *testing.T) {
	if _, err := Add(1<<62, 1<<62); !errors.Is(err, ErrOverflow) {
		t.Fatalf("expected overflow, got %v", err)
	}
}

func TestWholeMajor(t *testing.T) {
	if _, err := ToWholeMajor(500050, KES); err == nil {
		t.Error("fractional amount must not convert to whole shillings")
	}
	if v, err := ToWholeMajor(500000, KES); err != nil || v != 5000 {
		t.Errorf("ToWholeMajor = %d, %v", v, err)
	}
}
