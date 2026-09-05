package questionbank

import (
	"encoding/csv"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestActiveQuestionBankMatchesKatyaSourceContract(t *testing.T) {
	f, err := os.Open("../../materials/question_bank.csv")
	if err != nil {
		t.Fatalf("open active question bank: %v", err)
	}
	defer f.Close()

	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("read active question bank: %v", err)
	}
	if len(records) != 59 {
		t.Fatalf("active bank rows = %d, want header + 58 questions", len(records))
	}

	columns := make(map[string]int, len(records[0]))
	for i, name := range records[0] {
		columns[strings.TrimPrefix(name, "\ufeff")] = i
	}
	for _, name := range requiredColumns {
		if _, ok := columns[name]; !ok {
			t.Fatalf("active bank is missing column %q", name)
		}
	}

	topicCounts := map[string]int{}
	seenQuestions := map[string]bool{}
	for rowNumber, row := range records[1:] {
		if err := validateQuestionRow(normalizeQuestionRow(row, columns), columns); err != nil {
			t.Fatalf("row %d is invalid: %v", rowNumber+2, err)
		}
		question := row[columns["question_text"]]
		if seenQuestions[question] {
			t.Fatalf("duplicate question %q", question)
		}
		seenQuestions[question] = true
		if row[columns["grade"]] != "мидл-сеньор" {
			t.Fatalf("row %d grade = %q, want мидл-сеньор", rowNumber+2, row[columns["grade"]])
		}
		for fieldName, value := range map[string]string{
			"question_text": question,
			"followup_1":    row[columns["followup_1"]],
			"followup_2":    row[columns["followup_2"]],
		} {
			for _, placeholder := range []string{"[задача]", "[конкретный сценарий]", "[пример]"} {
				if strings.Contains(value, placeholder) {
					t.Fatalf("row %d field %s still contains placeholder %q", rowNumber+2, fieldName, placeholder)
				}
			}
		}
		if row[columns["answer_middle"]] != row[columns["answer_senior"]] {
			t.Fatalf("row %d must use the same stakeholder example for both target levels", rowNumber+2)
		}
		for _, marker := range []string{"Пример к основному вопросу:", "Пример к первому дожиму:", "Пример ко второму дожиму:"} {
			if !strings.Contains(row[columns["answer_middle"]], marker) {
				t.Fatalf("row %d reference is missing %q", rowNumber+2, marker)
			}
		}
		topicCounts[row[columns["topic"]]]++
	}

	wantTopics := map[string]int{
		"архитектура":  10,
		"интеграции":   10,
		"бд":           10,
		"требования":   10,
		"soft-skills":  10,
		"безопасность": 8,
	}
	if !reflect.DeepEqual(topicCounts, wantTopics) {
		t.Fatalf("topic counts = %#v, want %#v", topicCounts, wantTopics)
	}
}
