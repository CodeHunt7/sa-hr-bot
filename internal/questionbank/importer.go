// Package questionbank synchronizes the versioned CSV question bank with
// PostgreSQL. The CSV is the source of truth for questions available to new
// sessions; removed rows remain inactive for historical attempt references.
package questionbank

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

var requiredColumns = []string{
	"question_text",
	"question_context",
	"grade",
	"topic",
	"followup_1",
	"followup_1_context",
	"followup_2",
	"followup_2_context",
	"answer_junior",
	"answer_middle",
	"answer_senior",
	"source",
}

var allowedGrades = map[string]bool{
	"джун": true, "джун-мидл": true, "мидл": true,
	"мидл-сеньор": true, "сеньор": true,
}

var allowedTopics = map[string]bool{
	"интеграции": true, "архитектура": true, "бд": true,
	"требования": true, "безопасность": true, "soft-skills": true,
}

// Sync validates path completely and then updates the bank in one transaction.
// If validation or writing fails, the existing active bank remains unchanged.
func Sync(ctx context.Context, pool *pgxpool.Pool, path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	reader := csv.NewReader(f)
	header, err := reader.Read()
	if err != nil {
		return 0, fmt.Errorf("read csv header: %w", err)
	}

	colIndex := make(map[string]int, len(header))
	for i, name := range header {
		name = strings.TrimPrefix(strings.TrimSpace(name), "\ufeff")
		if _, exists := colIndex[name]; exists {
			return 0, fmt.Errorf("csv header contains duplicate column %q", name)
		}
		colIndex[name] = i
	}
	for _, col := range requiredColumns {
		if _, ok := colIndex[col]; !ok {
			return 0, fmt.Errorf("csv header is missing required column %q", col)
		}
	}

	var rows [][]string
	for {
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("read csv row %d: %w", len(rows)+2, err)
		}
		row = normalizeQuestionRow(row, colIndex)
		if err := validateQuestionRow(row, colIndex); err != nil {
			return 0, fmt.Errorf("validate csv row %d: %w", len(rows)+2, err)
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return 0, errors.New("question bank csv contains no questions")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin question bank sync: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `UPDATE question_bank SET active = false`); err != nil {
		return 0, fmt.Errorf("deactivate existing question bank: %w", err)
	}
	for i, row := range rows {
		_, err = tx.Exec(ctx,
			`INSERT INTO question_bank
			 (question_text, question_context, grade, topic,
			  followup_1, followup_1_context, followup_2, followup_2_context,
			  answer_junior, answer_middle, answer_senior, source, active)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, true)
			 ON CONFLICT (question_text) DO UPDATE SET
			     question_context = EXCLUDED.question_context,
			     grade = EXCLUDED.grade,
			     topic = EXCLUDED.topic,
			     followup_1 = EXCLUDED.followup_1,
			     followup_1_context = EXCLUDED.followup_1_context,
			     followup_2 = EXCLUDED.followup_2,
			     followup_2_context = EXCLUDED.followup_2_context,
			     answer_junior = EXCLUDED.answer_junior,
			     answer_middle = EXCLUDED.answer_middle,
			     answer_senior = EXCLUDED.answer_senior,
			     source = EXCLUDED.source,
			     active = true`,
			row[colIndex["question_text"]],
			row[colIndex["question_context"]],
			row[colIndex["grade"]],
			row[colIndex["topic"]],
			row[colIndex["followup_1"]],
			row[colIndex["followup_1_context"]],
			row[colIndex["followup_2"]],
			row[colIndex["followup_2_context"]],
			row[colIndex["answer_junior"]],
			row[colIndex["answer_middle"]],
			row[colIndex["answer_senior"]],
			row[colIndex["source"]],
		)
		if err != nil {
			return 0, fmt.Errorf("upsert csv row %d: %w", i+2, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit question bank sync: %w", err)
	}
	return len(rows), nil
}

func normalizeQuestionRow(row []string, columns map[string]int) []string {
	normalized := append([]string(nil), row...)
	for _, column := range requiredColumns {
		normalized[columns[column]] = strings.TrimSpace(normalized[columns[column]])
	}
	normalized[columns["grade"]] = strings.ToLower(normalized[columns["grade"]])
	normalized[columns["topic"]] = strings.ToLower(normalized[columns["topic"]])
	return normalized
}

func validateQuestionRow(row []string, columns map[string]int) error {
	requiredText := []string{
		"question_text", "question_context", "followup_1", "followup_1_context",
		"followup_2", "followup_2_context",
		"answer_junior", "answer_middle", "answer_senior",
	}
	for _, column := range requiredText {
		if strings.TrimSpace(row[columns[column]]) == "" {
			return fmt.Errorf("column %q must not be empty", column)
		}
	}

	grade := row[columns["grade"]]
	if !allowedGrades[grade] {
		return fmt.Errorf("unsupported grade %q", grade)
	}
	topic := row[columns["topic"]]
	if !allowedTopics[topic] {
		return fmt.Errorf("unsupported topic %q", topic)
	}
	return nil
}
