/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1_test

import (
	"testing"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
)

// TestParseErrorRate verifies percentage string parsing across valid, invalid, and boundary inputs.
func TestParseErrorRate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input   string
		want    float64
		wantErr bool
	}{
		{"2%", 0.02, false},
		{"100%", 1.0, false},
		{"0%", 0.0, false},
		{"0.5%", 0.005, false},
		{"", 0, true},
		{"5", 0, true},
		{"-1%", 0, true},
		{"101%", 0, true},
		{"abc%", 0, true},
		// trailing garbage — strconv.ParseFloat must reject these
		{"5x%", 0, true},
		// non-finite numeric input — caught by math.IsNaN / math.IsInf guard
		{"NaN%", 0, true},
		{"Inf%", 0, true},
		{"-Inf%", 0, true},
		// leading whitespace — strconv.ParseFloat must reject " 5"
		{" 5%", 0, true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got, err := agentraxv1alpha1.ParseErrorRate(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ParseErrorRate(%q) error=%v, wantErr=%v", tc.input, err, tc.wantErr)
			}
			if err == nil && abs(got-tc.want) > 1e-9 {
				t.Errorf("ParseErrorRate(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// abs returns the absolute value of a float64.
func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
