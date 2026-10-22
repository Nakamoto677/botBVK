package analytics

import (
	"testing"

	"bot-bvk/internal/config"
	"bot-bvk/internal/domain"
)

func TestOptionsFromConfigUsesOnlyConfiguredDepartments(t *testing.T) {
	c := config.Config{Departments: map[domain.Department]config.Team{
		domain.IT: {}, domain.OneC: {}, domain.Field: {},
	}}
	departments := OptionsFromConfig(c).Departments
	if len(departments) != 3 || departments[0] != domain.OneC || departments[1] != domain.IT || departments[2] != domain.Field {
		t.Fatalf("analytics departments: %v", departments)
	}
}
