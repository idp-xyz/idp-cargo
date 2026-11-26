// Package settingstest holds what the tests of the settings share: a pairwise covering array over a set of factors with
// a constraint across them, and the checker of the refusal convention. It is test support and is imported by tests only.
package settingstest

import "fmt"

// Factor is a setting with its levels, in the order a row lists them.
type Factor struct {
	Name   string
	Levels []string
}

// Row is one configuration: a level index for each factor.
type Row []int

// Valid says whether a configuration is allowed (the rules across items).
type Valid func(levels map[string]string) bool

// Value answers the level a row gives a factor.
func Value(factors []Factor, r Row, name string) string {
	for i, f := range factors {
		if f.Name == name {
			return f.Levels[r[i]]
		}
	}
	panic("settingstest: no factor " + name)
}

func asMap(factors []Factor, r Row) map[string]string {
	m := make(map[string]string, len(factors))
	for i, f := range factors {
		m[f.Name] = f.Levels[r[i]]
	}
	return m
}

type pair struct{ a, la, b, lb int }

// Pairwise answers a covering array: a list of valid configurations in which every pair of levels of two factors that some
// valid configuration holds together appears in at least one row. It is greedy and deterministic: it takes, until every
// such pair is covered, the first valid configuration (in lexicographic order) that covers the most pairs not yet covered.
func Pairwise(factors []Factor, valid Valid) []Row {
	var all []Row
	row := make(Row, len(factors))
	var walk func(i int)
	walk = func(i int) {
		if i == len(factors) {
			if valid == nil || valid(asMap(factors, row)) {
				all = append(all, append(Row(nil), row...))
			}
			return
		}
		for l := range factors[i].Levels {
			row[i] = l
			walk(i + 1)
		}
	}
	walk(0)
	need := map[pair]bool{}
	pairsOf := func(r Row, f func(pair)) {
		for i := range factors {
			for j := i + 1; j < len(factors); j++ {
				f(pair{i, r[i], j, r[j]})
			}
		}
	}
	for _, r := range all {
		pairsOf(r, func(p pair) { need[p] = true })
	}
	var out []Row
	for len(need) > 0 {
		best, bestGain := -1, 0
		for k, r := range all {
			gain := 0
			pairsOf(r, func(p pair) {
				if need[p] {
					gain++
				}
			})
			if gain > bestGain {
				best, bestGain = k, gain
			}
		}
		out = append(out, all[best])
		pairsOf(all[best], func(p pair) { delete(need, p) })
	}
	return out
}

// Missing lists the pairs of levels of two factors that some valid configuration holds and no row of the array does; it is
// empty for a covering array. Pairs that no valid configuration holds are not listed.
func Missing(factors []Factor, valid Valid, rows []Row) []string {
	have := map[pair]bool{}
	for _, r := range rows {
		for i := range factors {
			for j := i + 1; j < len(factors); j++ {
				have[pair{i, r[i], j, r[j]}] = true
			}
		}
	}
	var out []string
	row := make(Row, len(factors))
	seen := map[pair]bool{}
	var walk func(i int)
	walk = func(i int) {
		if i == len(factors) {
			if valid != nil && !valid(asMap(factors, row)) {
				return
			}
			for a := range factors {
				for b := a + 1; b < len(factors); b++ {
					p := pair{a, row[a], b, row[b]}
					if !have[p] && !seen[p] {
						seen[p] = true
						out = append(out, fmt.Sprintf("%s=%s with %s=%s", factors[a].Name, factors[a].Levels[row[a]], factors[b].Name, factors[b].Levels[row[b]]))
					}
				}
			}
			return
		}
		for l := range factors[i].Levels {
			row[i] = l
			walk(i + 1)
		}
	}
	walk(0)
	return out
}
