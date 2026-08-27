package story

// The session budget: how many scenes one session carries, and which kinds
// they are — both derived, never asked of a model (MAD-362's "scene budget is
// arithmetic" rule). ScenesPerSession prices a session off the same
// level-crossing currency Pace uses; SceneMix reads the legal template for
// the act's structural position out of Shape's catalogue. A four-hour
// session is not eleven scenes, and the mix is not a model preference.

import "math"

// Scene budget bounds. Three scenes is a quiet session; six is the most a
// table actually plays.
const (
	minScenesPerSession = 3
	maxScenesPerSession = 6
)

// ScenesPerSession says how many scenes one session of an act carries, from
// the act's level band and the number of sessions the act spans — the same
// crossing arithmetic Pace prices acts with, applied one level down.
//
// The rule: sessionsPerCrossing = sessions / crossings (a crossing is one
// level boundary of the band). A session carries ceil(6 / sessionsPerCrossing)
// + 2 scenes, clamped to 3..6 — the denser the leveling, the more each
// session must carry. At one session per level (a gauntlet act) a session
// carries six scenes; at two, five; at three to four and a half, four; at six
// or more (a slow burn), three. sessions below one is treated as one; a band
// with no crossings still has its session count to spend.
func ScenesPerSession(levelStart, levelEnd, sessions int) int {
	crossings := levelEnd - levelStart
	if crossings < 1 {
		crossings = 1
	}
	if sessions < 1 {
		sessions = 1
	}
	perCrossing := float64(sessions) / float64(crossings)
	scenes := int(math.Ceil(6/perCrossing)) + 2
	if scenes < minScenesPerSession {
		return minScenesPerSession
	}
	if scenes > maxScenesPerSession {
		return maxScenesPerSession
	}
	return scenes
}

// sceneQuota is one act role's template: the kinds its sessions are made of,
// in the fixed order the planner deals them. The first entry is the role's
// signature scene — the one a session of this act leads with.
var sceneQuotas = map[string][]string{
	"setup":         {KindSocial, KindExploration, KindSocial, KindDowntime, KindExploration, KindSocial},
	"exposition":    {KindSocial, KindExploration, KindSocial, KindDowntime, KindExploration, KindSocial},
	"complication":  {KindSocial, KindExploration, KindCombat, KindRevelation, KindSocial, KindCombat},
	"confrontation": {KindSocial, KindExploration, KindCombat, KindRevelation, KindSocial, KindCombat},
	"rising":        {KindSocial, KindExploration, KindCombat, KindRevelation, KindSocial, KindCombat},
	"mid_turn":      {KindRevelation, KindSocial, KindExploration, KindCombat, KindRevelation, KindSocial},
	"falling":       {KindExploration, KindCombat, KindSocial, KindRevelation, KindCombat, KindSocial},
	"climax":        {KindCombat, KindSocial, KindRevelation, KindCombat, KindExploration, KindCombat},
	"catastrophe":   {KindCombat, KindRevelation, KindSocial, KindCombat, KindExploration, KindCombat},
	"resolution":    {KindRevelation, KindSocial, KindDowntime, KindSocial, KindExploration, KindDowntime},
}

// defaultSceneQuota covers an act whose role is not in the catalogue (a
// hand-built campaign whose act count has no named shape): the middle
// template, the one almost every act of almost every structure uses.
var defaultSceneQuota = []string{
	KindSocial, KindExploration, KindCombat, KindRevelation, KindSocial, KindCombat,
}

// CastRole reports whether a string is one of the four cast roles.
func CastRole(role string) bool { return validCastRoles[role] }

// SecretDisposition reports whether a string is one of the three secret
// dispositions.
func SecretDisposition(d string) bool { return validDispositions[d] }

// SceneKind reports whether a string is one of the six scene kinds.
func SceneKind(k string) bool { return validKinds[k] }

// SceneMix returns the legal kind mix for one session's n scenes, given the
// act's position: actIndex is zero-based within actCount acts. The template
// comes from the act's structural role in Shape's catalogue — a setup act's
// sessions are social and exploration, a climax act's lead with combat — and
// cycles when n outruns the quota. n below one yields nil; the kinds are
// always drawn from the six the schema checks.
func SceneMix(actIndex, actCount, n int) []string {
	if n < 1 {
		return nil
	}
	quota := defaultSceneQuota
	if shape, ok := Shape(actCount); ok && actIndex >= 0 && actIndex < len(shape.Acts) {
		if q, ok := sceneQuotas[shape.Acts[actIndex].Key]; ok {
			quota = q
		}
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, quota[i%len(quota)])
	}
	return out
}
