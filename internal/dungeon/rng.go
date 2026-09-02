package dungeon

// The generator's deterministic source of draws. splitmix64: a fixed,
// self-contained algorithm whose whole state is one uint64 — no shared
// global source, no seed races, the same draws on every machine and in
// every process. That is what makes Layout pure: identical arguments,
// identical draw sequence, identical graph, forever.
type rng struct {
	state uint64
}

func newRNG(seed int64) *rng {
	return &rng{state: uint64(seed)}
}

// next advances the state and returns the next draw.
func (r *rng) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// intn returns a draw in [0, n). n must be positive; the generator never
// calls it otherwise, and a panic on zero is the honest failure for a
// call that has no answer.
func (r *rng) intn(n int) int {
	if n <= 0 {
		panic("dungeon: rng.intn with n <= 0")
	}
	return int(r.next() % uint64(n))
}
