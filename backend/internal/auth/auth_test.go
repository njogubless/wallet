package auth

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHashAndVerify(t *testing.T) {
	h, err := HashSecret("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := VerifySecret("correct horse battery staple", h); err != nil || !ok {
		t.Fatalf("verify correct: %v %v", ok, err)
	}
	if ok, _ := VerifySecret("wrong", h); ok {
		t.Fatal("wrong secret verified")
	}
	h2, _ := HashSecret("correct horse battery staple")
	if h == h2 {
		t.Fatal("hashes must be salted")
	}
	if _, err := VerifySecret("x", "$bcrypt$garbage"); err == nil {
		t.Fatal("malformed hash accepted")
	}
}

func TestValidatePIN(t *testing.T) {
	for _, bad := range []string{"", "123", "1234567", "12a4", "1234", "4321", "0000", "999999"} {
		if validatePIN(bad) == nil {
			t.Errorf("PIN %q accepted", bad)
		}
	}
	for _, good := range []string{"2580", "1397", "468213"} {
		if err := validatePIN(good); err != nil {
			t.Errorf("PIN %q rejected: %v", good, err)
		}
	}
}

func TestAccessTokens(t *testing.T) {
	ti := &TokenIssuer{Secret: []byte("0123456789abcdef0123456789abcdef"), Issuer: "test", TTL: time.Minute}
	id := uuid.New()
	tok, _, err := ti.Issue(id, "admin", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	gotID, role, err := ti.Verify(tok)
	if err != nil || gotID != id || role != "admin" {
		t.Fatalf("verify: %v %v %v", gotID, role, err)
	}
	expired, _, _ := ti.Issue(id, "user", time.Now().Add(-time.Hour))
	if _, _, err := ti.Verify(expired); err == nil {
		t.Fatal("expired token accepted")
	}
	other := &TokenIssuer{Secret: []byte("another-secret-another-secret-xx"), Issuer: "test", TTL: time.Minute}
	forged, _, _ := other.Issue(id, "admin", time.Now())
	if _, _, err := ti.Verify(forged); err == nil {
		t.Fatal("token signed with a different key accepted")
	}
	// alg=none must be rejected
	if _, _, err := ti.Verify("eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJzdWIiOiIxIn0."); err == nil {
		t.Fatal("unsigned token accepted")
	}
}
