package registry

import "strconv"

// ClosestMatch is difflib.get_close_matches(word, models, n=1) with its default
// cutoff of 0.6: the best-scoring candidate, ties going to the larger string.
func ClosestMatch(word string, models []string) string {
	best, bestScore, found := "", 0.0, false
	for _, m := range models {
		score := ratio([]rune(m), []rune(word))
		if score < 0.6 {
			continue
		}
		if !found || score > bestScore || (score == bestScore && m > best) {
			best, bestScore, found = m, score, true
		}
	}
	return best
}

// ratio is SequenceMatcher(None, a, b).ratio() for strings short enough that
// difflib's popularity heuristic (200+ characters) never applies.
func ratio(a, b []rune) float64 {
	total := len(a) + len(b)
	if total == 0 {
		return 1
	}
	b2j := map[rune][]int{}
	for j, c := range b {
		b2j[c] = append(b2j[c], j)
	}
	matches := 0
	type span struct{ alo, ahi, blo, bhi int }
	queue := []span{{0, len(a), 0, len(b)}}
	for len(queue) > 0 {
		s := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		i, j, k := longest(a, b2j, s.alo, s.ahi, s.blo, s.bhi)
		if k == 0 {
			continue
		}
		matches += k
		if s.alo < i && s.blo < j {
			queue = append(queue, span{s.alo, i, s.blo, j})
		}
		if i+k < s.ahi && j+k < s.bhi {
			queue = append(queue, span{i + k, s.ahi, j + k, s.bhi})
		}
	}
	return 2 * float64(matches) / float64(total)
}

func longest(a []rune, b2j map[rune][]int, alo, ahi, blo, bhi int) (besti, bestj, bestsize int) {
	besti, bestj = alo, blo
	j2len := map[int]int{}
	for i := alo; i < ahi; i++ {
		newj2len := map[int]int{}
		for _, j := range b2j[a[i]] {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := j2len[j-1] + 1
			newj2len[j] = k
			if k > bestsize {
				besti, bestj, bestsize = i-k+1, j-k+1, k
			}
		}
		j2len = newj2len
	}
	return
}

// gFormat is Python's "{:g}" for a float.
func gFormat(f float64) string { return strconv.FormatFloat(f, 'g', 6, 64) }
