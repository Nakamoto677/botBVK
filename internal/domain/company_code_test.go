package domain

import "testing"

func TestCompanyCodeGenerationAndNormalization(t *testing.T) {
	first, err := GenerateCompanyCode()
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateCompanyCode()
	if err != nil {
		t.Fatal(err)
	}
	a, err := NormalizeCompanyCode(first)
	if err != nil || len(a) != 26 {
		t.Fatalf("generated code rejected: %v", err)
	}
	b, err := NormalizeCompanyCode(second)
	if err != nil || a == b {
		t.Fatal("two generated credentials collided", err)
	}
	if got, err := NormalizeCompanyCode(a); err != nil || got != a {
		t.Fatal("link form rejected", err)
	}
	if _, err := NormalizeCompanyCode("short"); err == nil {
		t.Fatal("short credential accepted")
	}
}
