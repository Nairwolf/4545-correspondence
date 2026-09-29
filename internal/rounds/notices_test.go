package rounds

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nairwolf/4545-correspondence/internal/settings"
)

func TestByeSentence(t *testing.T) {
	assert.Equal(t,
		"Odd number of players this week and nobody was free for a double game, so you sat out. You're first in line to avoid the next one.",
		ByeSentence(settings.OddPoolDoubleThenBye))
	assert.Equal(t,
		"Odd number of players this week, so you sat out. You're first in line to avoid the next one.",
		ByeSentence(settings.OddPoolByeOnly))
}

func TestTimeControl(t *testing.T) {
	assert.Equal(t, "a rated correspondence game at 2 days per move",
		timeControl(Snapshot{Rated: true, DaysPerMove: 2}))
	assert.Equal(t, "a casual correspondence game at 1 day per move",
		timeControl(Snapshot{Rated: false, DaysPerMove: 1}))
	assert.Equal(t, "a correspondence game", timeControl(Snapshot{}), "no snapshot")
}
