package main

import "testing"

func TestValidateQuestionRow(t *testing.T) {
	columns := map[string]int{
		"question_text": 0, "grade": 1, "topic": 2,
		"followup_1": 3, "followup_2": 4,
		"answer_junior": 5, "answer_middle": 6, "answer_senior": 7,
	}
	valid := []string{"Вопрос", "мидл", "бд", "Уточнение 1", "Уточнение 2", "jun", "mid", "sen"}
	if err := validateQuestionRow(valid, columns); err != nil {
		t.Fatalf("valid row rejected: %v", err)
	}

	tests := []struct {
		name  string
		index int
		value string
	}{
		{name: "empty question", index: 0, value: ""},
		{name: "unknown grade", index: 1, value: "лид"},
		{name: "unknown topic", index: 2, value: "астрология"},
		{name: "empty followup", index: 3, value: ""},
		{name: "empty reference answer", index: 7, value: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row := append([]string(nil), valid...)
			row[tc.index] = tc.value
			if err := validateQuestionRow(row, columns); err == nil {
				t.Fatalf("invalid row was accepted: %v", row)
			}
		})
	}
}

func TestNormalizeQuestionRow(t *testing.T) {
	columns := map[string]int{
		"question_text": 0, "grade": 1, "topic": 2,
		"followup_1": 3, "followup_2": 4,
		"answer_junior": 5, "answer_middle": 6, "answer_senior": 7,
		"source": 8,
	}
	original := []string{"  Вопрос  ", " МИДЛ ", " БД ", " f1 ", " f2 ", " j ", " m ", " s ", " src "}

	got := normalizeQuestionRow(original, columns)
	if got[0] != "Вопрос" || got[1] != "мидл" || got[2] != "бд" || got[8] != "src" {
		t.Fatalf("unexpected normalized row: %#v", got)
	}
	if original[0] != "  Вопрос  " {
		t.Fatal("normalization must not mutate the caller's row")
	}
}
