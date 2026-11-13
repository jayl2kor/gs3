package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gs3/internal/pyfmt"
)

// pyRepr is Python's repr() of a str; see pyfmt.Repr.
func pyRepr(s string) string { return pyfmt.Repr(s) }

// pyInt converts s like Python's int(): surrounding whitespace, a sign and
// single underscores between digits are accepted. The error text is the one
// Python raises, which the AWS CLI prints verbatim.
func pyInt(s string) (int, error) {
	t := strings.TrimSpace(s)
	valid := t != "" && !strings.HasPrefix(t, "_") && !strings.HasSuffix(t, "_") && !strings.Contains(t, "__")
	if valid {
		digits := strings.TrimLeft(t, "+-")
		if len(t)-len(digits) > 1 || strings.HasPrefix(digits, "_") {
			valid = false
		}
	}
	if valid {
		if n, err := strconv.Atoi(strings.ReplaceAll(t, "_", "")); err == nil {
			return n, nil
		}
	}
	return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyRepr(s))
}

// closeMatches is difflib.get_close_matches: the best n candidates whose
// similarity ratio to word is at least cutoff, best first.
func closeMatches(word string, candidates []string, n int, cutoff float64) []string {
	type scored struct {
		ratio float64
		value string
	}
	var hits []scored
	w := []rune(word)
	for _, c := range candidates {
		if r := sequenceRatio([]rune(c), w); r >= cutoff {
			hits = append(hits, scored{r, c})
		}
	}
	// heapq.nlargest over (ratio, value) tuples: ties fall to the larger string.
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].ratio != hits[j].ratio {
			return hits[i].ratio > hits[j].ratio
		}
		return hits[i].value > hits[j].value
	})
	if len(hits) > n {
		hits = hits[:n]
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.value
	}
	return out
}

// sequenceRatio is difflib.SequenceMatcher(None, a, b).ratio(): twice the
// number of matched characters over the total length, where matches are found
// by repeatedly taking the longest common block (Ratcliff/Obershelp).
func sequenceRatio(a, b []rune) float64 {
	total := len(a) + len(b)
	if total == 0 {
		return 1
	}
	return 2 * float64(matchedLen(a, b)) / float64(total)
}

func matchedLen(a, b []rune) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	// Longest common block; earliest in a, then earliest in b, on ties.
	bestI, bestJ, bestK := 0, 0, 0
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1] + 1
				if cur[j] > bestK {
					bestK, bestI, bestJ = cur[j], i-cur[j], j-cur[j]
				}
			} else {
				cur[j] = 0
			}
		}
		prev, cur = cur, prev
		for j := range cur {
			cur[j] = 0
		}
	}
	if bestK == 0 {
		return 0
	}
	return bestK +
		matchedLen(a[:bestI], b[:bestJ]) +
		matchedLen(a[bestI+bestK:], b[bestJ+bestK:])
}
