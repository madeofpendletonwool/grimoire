package clock

import (
	"hash/fnv"
	"strconv"
)

// Forecast is one day's answer to "what is it like out there": a condition
// summary, a rough temperature, and a wind. Nothing more — the DM turns
// "light rain, 9°, steady wind" into fiction; this package never does.
type Forecast struct {
	Summary string `json:"summary"`
	TempC   int    `json:"temp_c"`
	Wind    string `json:"wind"`
}

// Climate tags a location's payload may carry to bend the tables below:
// desert, tropical, arctic. Anything else (including "") is temperate.
const (
	ClimateDesert   = "desert"
	ClimateTropical = "tropical"
	ClimateArctic   = "arctic"
)

// Condition tables per season, temperate by default. The "" season is for
// calendars whose seasons leave the current day in no band at all.
var weatherConditions = map[string][]string{
	"spring": {"clear skies", "light rain", "overcast", "showers", "spring wind", "thaw", "bright and mild"},
	"summer": {"clear skies", "hot sun", "heat haze", "afternoon storm", "light breeze", "humid air", "still heat"},
	"autumn": {"overcast", "steady rain", "cold wind", "fog", "clear and cold", "drizzle", "first frost"},
	"winter": {"snowfall", "clear frost", "grey skies", "blizzard", "freezing rain", "still cold", "deep snow"},
	"":       {"clear", "overcast", "wind", "rain", "fair skies", "grey", "mist"},
}

// climateOverrides replace a season's table and shift its temperatures; a
// desert winter is not a temperate winter with a tan.
type climateTable struct {
	conditions map[string][]string
	shift      int
}

var climateOverrides = map[string]climateTable{
	ClimateDesert: {conditions: map[string][]string{
		"spring": {"dry heat", "sandstorm on the horizon", "clear and parched", "cool wind"},
		"summer": {"scorching sun", "sandstorm", "shimmering heat", "still, crushing heat"},
		"autumn": {"dry heat", "clear and parched", "cool nights, hot days", "dust wind"},
		"winter": {"cold clear night", "pale sun", "chill wind", "frost at dawn"},
		"":       {"dry heat", "clear and parched", "dust wind"},
	}, shift: 10},
	ClimateTropical: {conditions: map[string][]string{
		"spring": {"sultry haze", "tropical storm", "bright humid heat", "sea breeze"},
		"summer": {"monsoon rain", "sultry haze", "tropical storm", "bright humid heat", "doldrums"},
		"autumn": {"monsoon rain", "sultry haze", "tropical storm", "sea breeze"},
		"winter": {"warm rain", "humid calm", "tropical storm", "sea breeze"},
		"":       {"humid calm", "warm rain", "sea breeze"},
	}, shift: 6},
	ClimateArctic: {conditions: map[string][]string{
		"spring": {"whiteout", "pale sun", "biting wind", "thaw wind"},
		"summer": {"midnight sun", "cold rain", "biting wind", "pale clear day"},
		"autumn": {"whiteout", "biting wind", "frozen fog", "pale sun"},
		"winter": {"whiteout", "deep freeze", "polar night", "northern lights over the ice"},
		"":       {"whiteout", "deep freeze", "pale sun"},
	}, shift: -16},
}

// Season base temperatures (temperate, °C).
var seasonBaseTemp = map[string]int{
	"spring": 12, "summer": 24, "autumn": 11, "winter": -3, "": 13,
}

var weatherWinds = []string{"calm", "light breeze", "steady wind", "strong gusts"}

// Weather derives the day's weather from the campaign's seed, the day, the
// season name, and an optional climate tag. It is pure: same seed, same day,
// same season, same climate — same weather, in this process and every
// restart, on this machine and any other. That determinism is the feature:
// weather costs zero rows and zero tokens, and "re-rolling" it means changing
// the seed, which is a recorded decision rather than a refresh button.
func Weather(seed string, day int64, season, climate string) Forecast {
	h := fnv.New64a()
	h.Write([]byte(seed))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(day, 10)))
	h.Write([]byte{0})
	h.Write([]byte(season))
	h.Write([]byte{0})
	h.Write([]byte(climate))
	v := h.Sum64()

	conditions := weatherConditions[season]
	base := seasonBaseTemp[season]
	if table, ok := climateOverrides[climate]; ok {
		if list, ok := table.conditions[season]; ok {
			conditions = list
		}
		base += table.shift
	}
	if len(conditions) == 0 {
		conditions = weatherConditions[""]
		base = seasonBaseTemp[""]
	}
	return Forecast{
		Summary: conditions[v%uint64(len(conditions))],
		TempC:   base + int((v>>16)%11) - 5,
		Wind:    weatherWinds[(v>>32)%uint64(len(weatherWinds))],
	}
}
