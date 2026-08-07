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

package v1alpha1

import (
	"fmt"
	"math"
	"strconv"
)

// ParseErrorRate parses a percentage string like "2%" and returns the float64
// value (e.g. 0.02 for "2%"). Returns an error if the format is invalid.
// Used by the validating webhook and the rollout threshold evaluator.
func ParseErrorRate(s string) (float64, error) {
	if len(s) == 0 {
		return 0, fmt.Errorf("empty error rate string")
	}
	if s[len(s)-1] != '%' {
		return 0, fmt.Errorf("error rate must end with '%%': got %q", s)
	}
	// strconv.ParseFloat rejects trailing garbage (e.g. "5x") and leading
	// whitespace (e.g. " 5"), unlike fmt.Sscanf which silently ignores them.
	pct, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil {
		return 0, fmt.Errorf("parsing error rate %q: %w", s, err)
	}
	// Reject non-finite values (NaN, ±Inf) that ParseFloat may return for
	// inputs like "NaN" or "Inf".
	if math.IsNaN(pct) || math.IsInf(pct, 0) {
		return 0, fmt.Errorf("error rate %q is not a finite number", s)
	}
	if pct < 0 || pct > 100 {
		return 0, fmt.Errorf("error rate %q out of range [0, 100]", s)
	}
	return pct / 100.0, nil
}
