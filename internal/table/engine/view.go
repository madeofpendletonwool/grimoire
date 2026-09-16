package engine

// The viewer model (MAD-337, stage 6 of MAD-321): who is reading, and
// what the store may hand them. ADR 13's rule as a type — the owner sees
// everything (the DM analog, and the common solo-tracker case where one
// account runs the whole table), and a seated account sees the public
// stream plus its own seats' rows. Entitlement is a set of seats and
// nothing else, because nothing else may distinguish readers: the
// per-seat stream is a WHERE clause over visibility, and this type is
// the WHERE clause's parameter.

// Viewer is one reader's entitlement. The zero Viewer sees public rows
// only — the safe default every caller degrades to.
type Viewer struct {
	Owner bool
	Seats []int
}

// OwnerViewer sees every row, hidden zones included.
func OwnerViewer() Viewer { return Viewer{Owner: true} }

// SeatViewer sees the public stream plus the named seats' seat-visible
// rows. Duplicate seats are fine; the set semantics below absorb them.
func SeatViewer(seats ...int) Viewer { return Viewer{Seats: seats} }

// PublicViewer sees the public stream only — the judge's and the
// spectator's entitlement (MAD-338). It is the zero Viewer, spelled:
// every scoped read already degrades to exactly this, so the observer's
// reads run the same WHERE clause a stranger's would, minus the 404.
func PublicViewer() Viewer { return Viewer{} }

// SeesSeat reports whether a seat's hidden zones are this viewer's to
// read: the owner's, or one of the seats the viewer holds.
func (v Viewer) SeesSeat(seat int) bool {
	if v.Owner {
		return true
	}
	for _, s := range v.Seats {
		if s == seat {
			return true
		}
	}
	return false
}

// SeesEvent reports whether one event row is this viewer's. It mirrors
// the SQL predicate EventsFor builds — public rows for everyone,
// seat-visible rows for the seat named on the row and nobody else. The
// reflection leak gate leans on the SQL being the authority and this
// being its honest copy for in-memory slices.
func (v Viewer) SeesEvent(e Event) bool {
	if e.Visibility != VisibilitySeat {
		return true
	}
	return v.SeesSeat(e.VisibleSeat)
}

// FilterEvents keeps the rows a viewer may see — the in-memory half of
// EventsFor's WHERE, for slices the server already holds (a Submit
// response's fresh rows, say). The stream's rows still come from the
// database through EventsFor; this exists so no response path can
// hand back rows the query would not have returned.
func FilterEvents(v Viewer, evs []Event) []Event {
	out := make([]Event, 0, len(evs))
	for i := range evs {
		if v.SeesEvent(evs[i]) {
			out = append(out, evs[i])
		}
	}
	return out
}
