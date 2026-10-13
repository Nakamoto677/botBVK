package config

import (
	"bytes"
	"encoding/json"
	"fmt"

	"bot-bvk/internal/domain"
)

type Companies []domain.CompanyDefinition

func (c *Companies) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("companies must be an array (use [] explicitly for no companies)")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		return err
	}
	result := make(Companies, 0, len(entries))
	for _, entry := range entries {
		var name string
		var company domain.CompanyDefinition
		if len(entry) > 0 && entry[0] == '"' {
			if err := json.Unmarshal(entry, &name); err != nil {
				return err
			}
			company = domain.LegacyCompany(name)
		} else {
			d := json.NewDecoder(bytes.NewReader(entry))
			d.DisallowUnknownFields()
			if err := d.Decode(&company); err != nil {
				return err
			}
		}
		result = append(result, company)
	}
	if err := domain.ValidateCompanies(result); err != nil {
		return err
	}
	*c = result
	return nil
}
