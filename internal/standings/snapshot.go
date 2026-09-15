package standings

import (
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
)

// SnapshotParams maps a Lichess user's ratings to a rating_snapshots row
// verbatim — Games and Provisional are stored alongside Rating rather
// than interpreted, so "does this player actually have a correspondence
// rating" stays a decision made later, by Recompute (spec §5.1), not
// baked in here. Shared by refresh-ratings (an existing player's daily
// update), seed-players and the registration callback (a new player's
// first snapshot) so the mapping exists in one place.
func SnapshotParams(userID pgtype.UUID, lu lichess.User) gen.InsertRatingSnapshotParams {
	params := gen.InsertRatingSnapshotParams{UserID: userID}
	if lu.Perfs.Correspondence != nil {
		rating := int32(lu.Perfs.Correspondence.Rating)
		games := int32(lu.Perfs.Correspondence.Games)
		prov := lu.Perfs.Correspondence.Provisional
		params.CorrespondenceRating = &rating
		params.CorrespondenceGames = &games
		params.CorrespondenceProv = &prov
	}
	if lu.Perfs.Classical != nil {
		rating := int32(lu.Perfs.Classical.Rating)
		games := int32(lu.Perfs.Classical.Games)
		prov := lu.Perfs.Classical.Provisional
		params.ClassicalRating = &rating
		params.ClassicalGames = &games
		params.ClassicalProv = &prov
	}
	return params
}
