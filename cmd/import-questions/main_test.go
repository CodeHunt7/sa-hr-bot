package main

import "testing"

func TestValidateQuestionRow(t *testing.T) {
	columns := map[string]int{
		"question_text": 0, "question_context": 1, "grade": 2, "topic": 3,
		"followup_1": 4, "followup_1_context": 5, "followup_2": 6, "followup_2_context": 7,
		"answer_junior": 8, "answer_middle": 9, "answer_senior": 10,
	}
	valid := []string{"Вопрос", "Контекст", "мидл", "бд", "Уточнение 1", "Контекст 1", "Уточнение 2", "Контекст 2", "jun", "mid", "sen"}
	if err := validateQuestionRow(valid, columns); err != nil {
		t.Fatalf("valid row rejected: %v", err)
	}

	tests := []struct {
		name  string
		index int
		value string
	}{
		{name: "empty question", index: 0, value: ""},
		{name: "empty context", index: 1, value: ""},
		{name: "unknown grade", index: 2, value: "лид"},
		{name: "unknown topic", index: 3, value: "астрология"},
		{name: "removed presentation topic", index: 3, value: "подача"},
		{name: "empty followup", index: 4, value: ""},
		{name: "empty followup context", index: 5, value: ""},
		{name: "empty reference answer", index: 10, value: ""},
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
		"question_text": 0, "question_context": 1, "grade": 2, "topic": 3,
		"followup_1": 4, "followup_1_context": 5, "followup_2": 6, "followup_2_context": 7,
		"answer_junior": 8, "answer_middle": 9, "answer_senior": 10,
		"source": 11,
	}
	original := []string{"  Вопрос  ", " context ", " МИДЛ ", " БД ", " f1 ", " c1 ", " f2 ", " c2 ", " j ", " m ", " s ", " src "}

	got := normalizeQuestionRow(original, columns)
	if got[0] != "Вопрос" || got[2] != "мидл" || got[3] != "бд" || got[11] != "src" {
		t.Fatalf("unexpected normalized row: %#v", got)
	}
	if original[0] != "  Вопрос  " {
		t.Fatal("normalization must not mutate the caller's row")
	}
}
