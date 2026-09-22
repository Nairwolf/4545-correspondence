package pairing

// leftoverImbalance is the total colour imbalance a game leaves behind
// once white has played white and black has played black: playing white
// adds one to a colour score, playing black subtracts one (§5.5, §6.2
// step 3). Lower is better.
func leftoverImbalance(white, black Player) int {
	return abs(white.ColorScore+1) + abs(black.ColorScore-1)
}

// colourPenalty is §6.2's colour_penalty: the better of the two colour
// assignments for a pair, and which one that is. Returning both is
// deliberate — step 3 prices the pair with the penalty and step 5
// assigns the colours with the argmin, so the two cannot disagree.
//
// On a tie the lower-rated player takes white, then the lower id; never
// anything that depends on order or the clock, so a regenerated round
// is identical.
func colourPenalty(a, b Player) (penalty int, aWhite bool) {
	aFirst := leftoverImbalance(a, b)
	bFirst := leftoverImbalance(b, a)
	switch {
	case aFirst < bFirst:
		return aFirst, true
	case bFirst < aFirst:
		return bFirst, false
	case a.PowerRating != b.PowerRating:
		return aFirst, a.PowerRating < b.PowerRating
	default:
		return aFirst, a.ID < b.ID
	}
}

// assignColours applies the argmin.
func assignColours(a, b Player) (white, black Player) {
	if _, aWhite := colourPenalty(a, b); aWhite {
		return a, b
	}
	return b, a
}

// AssignColours is assignColours, exported for the one caller outside
// Generate: an admin's swap between two pairings (spec §8.5) re-derives
// colours with the engine's own colour step, so a swap never worsens
// balance and can never disagree with how Generate would have coloured
// the same pair.
func AssignColours(a, b Player) (white, black Player) {
	return assignColours(a, b)
}

// assignDoubleColours re-colours the double-game volunteer's two games
// so they get ONE WHITE AND ONE BLACK (§6.2 step 6a) — a hard
// constraint, not a preference, which is what makes a double game free
// colour balancing: the volunteer's own colour score comes out
// unchanged either way. It therefore drops out of the comparison
// entirely, and only the two opponents' leftover imbalance decides
// which way round the games go. A tie is settled by the step 5
// tie-break on the first of the two games.
//
// The stored ColorPenalty is the penalty actually realised, which can
// differ from the per-pair argmin the cost used, since that argmin
// considered each game on its own.
func assignDoubleColours(pairings []Pairing, byID map[string]Player, volunteerID string) {
	var games []int
	for i, p := range pairings {
		if p.White == volunteerID || p.Black == volunteerID {
			games = append(games, i)
		}
	}
	v := byID[volunteerID]
	first := byID[opponentOf(pairings[games[0]], volunteerID)]
	second := byID[opponentOf(pairings[games[1]], volunteerID)]

	// The volunteer takes white in one game and black in the other; the
	// only choice is which opponent gets which.
	volunteerWhiteFirst := abs(first.ColorScore-1) + abs(second.ColorScore+1)
	volunteerBlackFirst := abs(first.ColorScore+1) + abs(second.ColorScore-1)

	var whiteFirst bool
	switch {
	case volunteerWhiteFirst < volunteerBlackFirst:
		whiteFirst = true
	case volunteerBlackFirst < volunteerWhiteFirst:
		whiteFirst = false
	default:
		_, whiteFirst = colourPenalty(v, first)
	}

	setColours(&pairings[games[0]], v, first, whiteFirst)
	setColours(&pairings[games[1]], v, second, !whiteFirst)
}

func setColours(p *Pairing, volunteer, opponent Player, volunteerWhite bool) {
	white, black := volunteer, opponent
	if !volunteerWhite {
		white, black = opponent, volunteer
	}
	p.White, p.Black = white.ID, black.ID
	p.ColorPenalty = leftoverImbalance(white, black)
}

func opponentOf(p Pairing, id string) string {
	if p.White == id {
		return p.Black
	}
	return p.White
}
