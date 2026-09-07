package lichess

import "time"

// Perf is one of a user's per-variant/speed ratings (spec's verified
// Perf schema). Rating is always present in Lichess's response — an
// account that has never played this perf type still gets an assigned
// default rating with a high deviation — so Games is what distinguishes
// "never played" from "has a real rating"; that interpretation belongs
// to the caller (internal/standings), not to this package.
type Perf struct {
	Games       int  `json:"games"`
	Rating      int  `json:"rating"`
	RD          int  `json:"rd"`
	Provisional bool `json:"prov"` // absent in the JSON when false — encoding/json's zero value already matches
}

// User is the subset of Lichess's /api/users response this application
// uses. Perfs are pointers because, defensively, a perf type could be
// entirely absent from the response for some account shapes; nil means
// "not present in the response", separate from Games==0 meaning "present
// but never played".
type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Perfs    struct {
		Correspondence *Perf `json:"correspondence"`
		Classical      *Perf `json:"classical"`
	} `json:"perfs"`
}

// GamePlayerUser identifies the human side of a GamePlayer. It's a
// pointer on GamePlayer because an AI opponent has no user at all
// (GamePlayerAi in the spec's schema) — not a shape this league's bulk
// pairings ever produce, but decoding must not panic if it's ever seen.
type GamePlayerUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// GamePlayerAnalysis is present only for a game that has been analysed
// on Lichess (spec §7.1 note: correspondence games are not analysed
// automatically, so this is commonly absent).
type GamePlayerAnalysis struct {
	ACPL     int  `json:"acpl"`
	Accuracy *int `json:"accuracy"`
}

type GamePlayer struct {
	User        *GamePlayerUser     `json:"user"`
	Rating      int                 `json:"rating"`
	Provisional bool                `json:"provisional"`
	Analysis    *GamePlayerAnalysis `json:"analysis"`
}

type GameOpening struct {
	ECO  string `json:"eco"`
	Name string `json:"name"`
	Ply  int    `json:"ply"`
}

// Game is the subset of Lichess's GameJson schema this application maps
// into the games table (spec §4.1, §7.1). CreatedAt/LastMoveAt are epoch
// milliseconds on the wire; CreatedAtTime/LastMoveAtTime convert them.
type Game struct {
	ID          string       `json:"id"`
	Rated       bool         `json:"rated"`
	Variant     string       `json:"variant"`
	Speed       string       `json:"speed"`
	CreatedAt   int64        `json:"createdAt"`
	LastMoveAt  int64        `json:"lastMoveAt"`
	Status      string       `json:"status"` // GameStatusName enum, spec §3.4 discrepancy #1/#2
	Winner      string       `json:"winner"` // "white" | "black" | "" (absent -> zero value)
	DaysPerTurn int          `json:"daysPerTurn"`
	Moves       string       `json:"moves"`
	Opening     *GameOpening `json:"opening"`
	Players     struct {
		White GamePlayer `json:"white"`
		Black GamePlayer `json:"black"`
	} `json:"players"`
}

func (g Game) CreatedAtTime() time.Time  { return time.UnixMilli(g.CreatedAt) }
func (g Game) LastMoveAtTime() time.Time { return time.UnixMilli(g.LastMoveAt) }

// Finished statuses per the verified GameStatusName enum (spec §3.4).
// aborted and noStart are deliberately excluded — spec §4.1 says those
// are not stored as games at all.
const (
	StatusCreated                   = "created"
	StatusStarted                   = "started"
	StatusAborted                   = "aborted"
	StatusMate                      = "mate"
	StatusResign                    = "resign"
	StatusStalemate                 = "stalemate"
	StatusTimeout                   = "timeout"
	StatusDraw                      = "draw"
	StatusOutOfTime                 = "outoftime"
	StatusCheat                     = "cheat"
	StatusNoStart                   = "noStart"
	StatusUnknownFinish             = "unknownFinish"
	StatusInsufficientMaterialClaim = "insufficientMaterialClaim"
	StatusVariantEnd                = "variantEnd"
)
