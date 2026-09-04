package homebrew

// The computed checks (MAD-385, check 1): the maths that exists, against
// the numbers the homebrew carries. For a monster, the DMG procedure the
// statblock package implements — with the offensive/defensive split and
// the specific shortfall. For an item, the corpus rarity distribution —
// checkable claims about the real SRD shelf, never a computed verdict.
// Every finding's basis is its arithmetic: the numbers and where they
// came from.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/madeofpendletonwool/grimoire/internal/items"
	"github.com/madeofpendletonwool/grimoire/internal/statblock"
)

/* ---------- the monster's numbers ---------- */

// lintMonsterCR compares the requested CR against the computed one and
// reports the calculator's confidence. The rating arrives already
// computed by the caller (LintMonster runs statblock.ComputeCR once), so
// the basis is this run's own arithmetic.
func lintMonsterCR(sb statblock.Statblock, requestedCR string, rating statblock.Rating) []Finding {
	var out []Finding

	delivery := "attack bonus +" + itoa(rating.AttackBonus)
	if rating.SaveBased {
		delivery = "save-based offense (DC " + itoa(rating.AttackBonus+8) + ")"
	}
	arithmetic := fmt.Sprintf(
		"defensive CR %s (%d effective HP at AC %d); offensive CR %s (%d DPR at %s); final CR %s, confidence %s",
		statblock.Label(rating.Defensive), rating.EffectiveHP, rating.AC,
		statblock.Label(rating.Offensive), rating.DPR, delivery,
		rating.Label, rating.Confidence)

	if req, ok := statblock.ParseLabel(requestedCR); ok {
		shortfall := statblock.Shortfall(req, rating)
		if statblock.Label(req) != rating.Label {
			message := fmt.Sprintf(
				"The brief asked for CR %s; the calculator computes CR %s.",
				statblock.Label(req), rating.Label)
			if len(shortfall) > 0 {
				message += " " + strings.Join(shortfall, " ")
			}
			out = append(out, Finding{
				Check:    CheckMonsterCR,
				Severity: SeverityWarning,
				Subject:  "cr",
				Message:  message,
				Basis:    Basis{Origin: OriginComputed, Arithmetic: arithmetic},
			})
		}
	}

	// Confidence: a low rating is a diagnosis, not an answer. The finding
	// says which actions did not parse, or which known blind spot
	// applies, so the DM knows exactly what the number is missing.
	switch rating.Confidence {
	case statblock.ConfidenceLow:
		var unparsed []string
		for _, a := range sb.Actions {
			if !a.Parsed {
				unparsed = append(unparsed, a.Name)
			}
		}
		detail := ""
		if len(unparsed) > 0 {
			detail = fmt.Sprintf(" %d of %d actions did not parse (%s);",
				len(unparsed), len(sb.Actions), strings.Join(unparsed, ", "))
		}
		notes := ""
		if len(rating.Notes) > 0 {
			notes = " " + strings.Join(rating.Notes, " ")
		}
		out = append(out, Finding{
			Check:    CheckMonsterConfidence,
			Severity: SeverityNote,
			Subject:  "cr",
			Message: fmt.Sprintf(
				"The CR figure is low-confidence — treat it as a diagnosis, not an answer.%s%s",
				detail, notes),
			Basis: Basis{Origin: OriginComputed, Arithmetic: arithmetic},
		})
	case statblock.ConfidenceMedium:
		out = append(out, Finding{
			Check:    CheckMonsterConfidence,
			Severity: SeverityNote,
			Subject:  "cr",
			Message: fmt.Sprintf(
				"The CR figure is medium-confidence: %s",
				strings.Join(rating.Notes, " ")),
			Basis: Basis{Origin: OriginComputed, Arithmetic: arithmetic},
		})
	}
	return out
}

/* ---------- the item's numbers, against the shelf ---------- */

// rarityMetrics is one metric the bands read, described for findings the
// way the item designer's notes describe them.
type rarityMetric struct {
	name  string  // plural noun: "bonuses"
	value float64 // the design's own value
	val   string  // the value in claim form: "a +3 bonus"
	fetch func(items.Metrics) float64
}

func rarityMetricsOf(m items.Metrics) []rarityMetric {
	return []rarityMetric{
		{"bonuses", float64(m.Bonus), fmt.Sprintf("a +%d bonus", m.Bonus),
			func(x items.Metrics) float64 { return float64(x.Bonus) }},
		{"charge counts", float64(m.Charges), fmt.Sprintf("%d charges", m.Charges),
			func(x items.Metrics) float64 { return float64(x.Charges) }},
		{"daily recharge rates", m.RechargePerDay,
			fmt.Sprintf("a daily recharge of about %s charges", trimFloat(m.RechargePerDay)),
			func(x items.Metrics) float64 { return x.RechargePerDay }},
		{"save DCs", float64(m.SaveDC), fmt.Sprintf("a save DC of %d", m.SaveDC),
			func(x items.Metrics) float64 { return float64(x.SaveDC) }},
		{"die expressions", m.DamagePerRoll,
			fmt.Sprintf("a die expression worth about %s damage", trimFloat(m.DamagePerRoll)),
			func(x items.Metrics) float64 { return x.DamagePerRoll }},
	}
}

// lintItemRarity places a design against the corpus distribution. It never
// judges rarity: the findings are checkable claims about the real shelf —
// "every SRD item carrying a +3 bonus is Legendary" — each with the
// counts as its arithmetic.
func lintItemRarity(d items.Design, corpus []items.Item) []Finding {
	var out []Finding
	metrics := items.MetricsOfDesign(d)
	rarity := strings.TrimSpace(d.Rarity)
	rankOfRarity := -1
	for _, r := range items.Rarities {
		if strings.EqualFold(r.Name, rarity) {
			rankOfRarity = r.Rank
		}
	}

	for _, md := range rarityMetricsOf(metrics) {
		if md.value <= 0 {
			continue
		}
		// Who on the shelf carries this metric at this level or above?
		type raritySum struct {
			name  string
			rank  int
			count int
		}
		byRarity := map[string]*raritySum{}
		reaching := 0
		for _, it := range corpus {
			if strings.TrimSpace(it.Rarity) == "" {
				continue
			}
			if md.fetch(items.MetricsOfItem(it)) < md.value {
				continue
			}
			reaching++
			rs, ok := byRarity[it.Rarity]
			if !ok {
				rank := 0
				for _, r := range items.Rarities {
					if strings.EqualFold(r.Name, it.Rarity) {
						rank = r.Rank
					}
				}
				rs = &raritySum{name: it.Rarity, rank: rank}
				byRarity[it.Rarity] = rs
			}
			rs.count++
		}
		if reaching == 0 {
			// No SRD item carries this at all — worth a look, not a rule
			// broken: the corpus is a shelf, not a ceiling.
			out = append(out, Finding{
				Check:    CheckItemUnmatched,
				Severity: SeverityNote,
				Subject:  "rarity",
				Message: fmt.Sprintf(
					"No SRD item in the mirror carries %s — nothing on the shelf to compare against, which is exactly when a DM should look twice.",
					md.val),
				Basis: Basis{
					Origin: OriginComputed,
					Arithmetic: fmt.Sprintf(
						"%d items scanned; 0 carry %s", len(corpus), md.val),
				},
			})
			continue
		}

		names := make([]string, 0, len(byRarity))
		for _, rs := range byRarity {
			names = append(names, rs.name)
		}
		sort.Slice(names, func(i, j int) bool {
			return rankByName(names[i]) < rankByName(names[j])
		})

		// Everything reaching this value sits strictly above the declared
		// rarity — the "+3 at Uncommon" shape.
		if rankOfRarity >= 0 {
			above := 0
			for _, rs := range byRarity {
				if rs.rank > rankOfRarity {
					above += rs.count
				}
			}
			if above == reaching {
				out = append(out, Finding{
					Check:    CheckItemRarity,
					Severity: SeverityWarning,
					Subject:  "rarity",
					Message: fmt.Sprintf(
						"The design declares %s and carries %s — every SRD item carrying %s is %s. The shelf disagrees with the label.",
						rarityLabel(rarity), md.val, md.val, joinNames(names)),
					Basis: Basis{
						Origin: OriginComputed,
						Arithmetic: fmt.Sprintf(
							"%d of %d SRD items carry %s, all at %s; none at %s or below",
							reaching, len(corpus), md.val, joinNames(names), rarityLabel(rarity)),
					},
				})
				continue
			}
		}
	}
	return out
}

// rarityLabel renders the declared rarity in prose form.
func rarityLabel(rarity string) string {
	if strings.TrimSpace(rarity) == "" {
		return "no rarity"
	}
	return rarity
}

func rankByName(name string) int {
	for _, r := range items.Rarities {
		if strings.EqualFold(r.Name, name) {
			return r.Rank
		}
	}
	return -1
}

func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " or " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + ", or " + names[len(names)-1]
	}
}

func trimFloat(v float64) string {
	if v == float64(int64(v)) {
		return strconv.Itoa(int(v))
	}
	return fmt.Sprintf("%.1f", v)
}

func itoa(v int) string {
	return strconv.Itoa(v)
}
