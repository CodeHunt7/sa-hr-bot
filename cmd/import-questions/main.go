// Command import-questions loads the interview question bank from a CSV
// file into the question_bank table.
package main

import (
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

// requiredColumns lists the CSV header names this tool maps to
// question_bank columns. Their order in the file does not matter.
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
	"требования": true, "безопасность": true, "подача": true,
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	csvPath := flag.String("file", "question_bank.csv", "path to the question bank CSV file")
	flag.Parse()

	// In production DATABASE_URL is injected directly; .env is only for
	// local development and may not exist.
	_ = godotenv.Load()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}

	f, err := os.Open(*csvPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", *csvPath, err)
	}
	defer f.Close()

	reader := csv.NewReader(f)
	// FieldsPerRecord is left at its zero value: csv.Reader locks it to
	// the header's field count and rejects ragged rows automatically.

	header, err := reader.Read()
	if err != nil {
		return fmt.Errorf("read csv header: %w", err)
	}

	colIndex := make(map[string]int, len(header))
	for i, name := range header {
		name = strings.TrimPrefix(strings.TrimSpace(name), "\ufeff")
		if _, exists := colIndex[name]; exists {
			return fmt.Errorf("csv header contains duplicate column %q", name)
		}
		colIndex[name] = i
	}
	for _, col := range requiredColumns {
		if _, ok := colIndex[col]; !ok {
			return fmt.Errorf("csv header is missing required column %q", col)
		}
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	imported := 0
	for {
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read csv row %d: %w", imported+2, err)
		}
		row = normalizeQuestionRow(row, colIndex)
		if err := validateQuestionRow(row, colIndex); err != nil {
			return fmt.Errorf("validate csv row %d: %w", imported+2, err)
		}

		_, err = tx.Exec(ctx,
			`INSERT INTO question_bank
			 (question_text, question_context, grade, topic,
			  followup_1, followup_1_context, followup_2, followup_2_context,
			  answer_junior, answer_middle, answer_senior, source)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
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
			     source = EXCLUDED.source`,
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
			return fmt.Errorf("insert csv row %d: %w", imported+2, err)
		}
		imported++
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}

	fmt.Printf("Загружено вопросов: %d\n", imported)
	return nil
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
		return fmt.Errorf("unsupported grade %q", row[columns["grade"]])
	}
	topic := row[columns["topic"]]
	if !allowedTopics[topic] {
		return fmt.Errorf("unsupported topic %q", row[columns["topic"]])
	}
	return nil
}
