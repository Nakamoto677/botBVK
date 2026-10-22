package telegram

import (
	"strings"
	"testing"

	"bot-bvk/internal/domain"
)

func TestUnconfiguredDepartmentHiddenFromPilotMenusAndTags(t *testing.T) {
	a := &Adapter{cfg: Config{Departments: map[domain.Department]Team{
		domain.IT:    {ChatID: -1001, QueueTopicID: 2, Tags: []string{"#it"}},
		domain.OneC:  {ChatID: -1002, QueueTopicID: 2, Tags: []string{"#1с"}},
		domain.Field: {ChatID: -1003, QueueTopicID: 2, Tags: []string{"#выезд"}},
	}}}
	for _, row := range a.departmentButtons("new") {
		for _, button := range row {
			if strings.Contains(button.Data, "installation") || button.Text == "Монтаж" {
				t.Fatalf("disabled department in picker: %+v", button)
			}
		}
	}
	if _, ok := a.groupTags()[domain.Installation]; ok {
		t.Fatal("disabled department accepted as a group tag")
	}
	if strings.Contains(a.groupStartHint(), "#монтаж") {
		t.Fatal("disabled department advertised in group hint")
	}
	if a.departmentEnabled(domain.Installation) {
		t.Fatal("disabled department accepted by callback guard")
	}
}
