package domain

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
)

const companyCodeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NormalizeCompanyCode accepts the grouped form given to people and the
// ungrouped form used in Telegram start links. It never returns the input in
// an error, because the input is a credential.
func NormalizeCompanyCode(value string) (string, error) {
	value = strings.ToUpper(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", "")
	value = strings.ReplaceAll(value, " ", "")
	value = strings.NewReplacer("O", "0", "I", "1", "L", "1").Replace(value)
	if len(value) != 26 {
		return "", ErrInvalid
	}
	for _, c := range value {
		if !strings.ContainsRune(companyCodeAlphabet, c) {
			return "", ErrInvalid
		}
	}
	return value, nil
}

// GenerateCompanyCode creates a 128-bit company credential, formatted for
// copying by hand. It is intentionally separate from company identifiers.
func GenerateCompanyCode() (string, error) {
	var seed [16]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return "", err
	}
	n := new(big.Int).SetBytes(seed[:])
	base := big.NewInt(32)
	var digits [26]byte
	for i := len(digits) - 1; i >= 0; i-- {
		q, rem := new(big.Int), new(big.Int)
		q.QuoRem(n, base, rem)
		digits[i] = companyCodeAlphabet[rem.Int64()]
		n = q
	}
	return fmt.Sprintf("%s-%s-%s-%s-%s", digits[:5], digits[5:10], digits[10:15], digits[15:20], digits[20:]), nil
}
