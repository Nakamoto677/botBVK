package domain

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
)

// CompanyDefinition identifies an organization independently of its display name.
type CompanyDefinition struct {
	Key        string `json:"id"`
	Name       string `json:"name"`
	LegacyName string `json:"legacy_name,omitempty"`
}

func LegacyCompany(name string) CompanyDefinition {
	return CompanyDefinition{Key: fmt.Sprintf("legacy:%x", sha256.Sum256([]byte(name))), Name: name}
}

var companyKey = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func ValidateCompanies(companies []CompanyDefinition) error {
	keys, names, aliases := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, c := range companies {
		legacy := c.Key == LegacyCompany(c.Name).Key && c.LegacyName == ""
		if (!legacy && !companyKey.MatchString(c.Key)) || strings.TrimSpace(c.Name) == "" || c.Name != strings.TrimSpace(c.Name) {
			return fmt.Errorf("invalid company id or name: %q", c.Key)
		}
		if c.LegacyName != "" && strings.TrimSpace(c.LegacyName) == "" {
			return fmt.Errorf("empty legacy_name")
		}
		if keys[c.Key] || names[c.Name] || (c.LegacyName != "" && aliases[c.LegacyName]) {
			return fmt.Errorf("duplicate company id, name or legacy_name: %q", c.Key)
		}
		keys[c.Key], names[c.Name] = true, true
		if c.LegacyName != "" {
			aliases[c.LegacyName] = true
		}
	}
	return nil
}
