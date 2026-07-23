package db

import (
	"reflect"
	"strings"
	"testing"
)

func TestRandomAccessCode(t *testing.T) {
	for i := 0; i < 100; i++ {
		code, err := randomAccessCode()
		if err != nil {
			t.Fatalf("randomAccessCode: %v", err)
		}
		if !strings.HasPrefix(code, accessCodePrefix) || len(code) != len(accessCodePrefix)+accessCodeSuffixLen {
			t.Fatalf("unexpected code format: %q", code)
		}
		for _, char := range code[len(accessCodePrefix):] {
			if !strings.ContainsRune(accessCodeAlphabet, char) {
				t.Fatalf("code %q contains unsupported character %q", code, char)
			}
		}
	}
}

func TestCompatibleQuestionGrades(t *testing.T) {
	tests := []struct {
		grade string
		want  []string
	}{
		{grade: "джун", want: []string{"джун", "джун-мидл"}},
		{grade: "мидл", want: []string{"джун-мидл", "мидл", "мидл-сеньор"}},
		{grade: "сеньор", want: []string{"мидл-сеньор", "сеньор"}},
		{grade: " МИДЛ ", want: []string{"джун-мидл", "мидл", "мидл-сеньор"}},
		{grade: "unknown", want: []string{"unknown"}},
	}

	for _, tc := range tests {
		if got := compatibleQuestionGrades(tc.grade); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("compatibleQuestionGrades(%q) = %#v, want %#v", tc.grade, got, tc.want)
		}
	}
}
