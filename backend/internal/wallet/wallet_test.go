package wallet

import "testing"

func TestAccountNumberLuhn(t *testing.T) {
	for i := 0; i < 200; i++ {
		n, err := newAccountNumber()
		if err != nil {
			t.Fatal(err)
		}
		if !ValidAccountNumber(n) {
			t.Fatalf("generated invalid account number %q", n)
		}
		// Any single-digit change must be detected.
		b := []byte(n)
		b[5] = '0' + (b[5]-'0'+1)%10
		if ValidAccountNumber(string(b)) {
			t.Fatalf("Luhn failed to detect change in %q", n)
		}
	}
	if ValidAccountNumber("12345") || ValidAccountNumber("12345678901a") {
		t.Fatal("accepted malformed account number")
	}
}
