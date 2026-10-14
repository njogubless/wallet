package policy

import "testing"

func TestFeeRuleCompute(t *testing.T) {
	maxFee := int64(30000)
	r := FeeRule{FlatFee: 1500, RateBps: 50, MinFee: 1500, MaxFee: &maxFee}
	cases := []struct{ amount, want int64 }{
		{100000, 2000},    // KES 1,000: 15.00 + 0.5% (5.00) = 20.00
		{1000, 1505},      // KES 10: 15.00 + 0.05 = 15.05
		{10000000, 30000}, // KES 100,000: 15 + 500 capped at 300.00
		{100001, 2001},    // rounding up 500.005 -> 501
	}
	for _, c := range cases {
		got, err := r.Compute(c.amount)
		if err != nil || got != c.want {
			t.Errorf("Compute(%d) = %d, %v; want %d", c.amount, got, err, c.want)
		}
	}
	free := FeeRule{}
	if got, _ := free.Compute(500000); got != 0 {
		t.Errorf("free rule charged %d", got)
	}
	minOnly := FeeRule{RateBps: 10, MinFee: 500}
	if got, _ := minOnly.Compute(1000); got != 500 {
		t.Errorf("minimum fee not applied: %d", got)
	}
}
