package db

import (
	"reflect"
	"testing"
)

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
