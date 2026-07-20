// Package beeline holds the domain model and the pluggable seams for the
// precomputed travel-time/distance matrix service described in beeline-design.md.
//
// The three interfaces here — RoutingEngine, Store, and FreshnessIndex — are the
// points where a prototype implementation (a Haversine engine, in-memory store and
// index) can later be swapped for a production one (OSRM, Redis, Postgres) without
// touching the pipeline that wires them together. They are copied faithfully from
// §5 of the design doc so that swap stays a drop-in.
package beeline

import (
	"time"

	"github.com/uber/h3-go/v4"
)

// H3Cell identifies a single hexagonal cell at some resolution. It is an alias of
// the h3 cell type (an int64), so it is comparable and usable as a map key.
type H3Cell = h3.Cell

// LatLng is a geographic coordinate in degrees.
type LatLng struct {
	Lat float64
	Lng float64
}

// Profile is a routing mode (e.g. "car", "bike", "walk"). The design leaves room
// for it to optionally carry a time-of-day bucket; the prototype keeps it to mode.
type Profile string

// AreaID identifies a configured service area. It is the SQLite areas.id (an
// INTEGER PRIMARY KEY), and an int64 so PairKey stays a compact, comparable map
// key even with millions of pairs held in memory. The zero value means "no area"
// — a pair not attributed to any configured area (e.g. an out-of-area demand read).
type AreaID int64

// Annotations is a bitset selecting which scalars a caller wants computed.
type Annotations uint8

const (
	// AnnotateDuration requests travel duration (seconds).
	AnnotateDuration Annotations = 1 << iota
	// AnnotateDistance requests travel distance (meters).
	AnnotateDistance
)

// Has reports whether the given annotation bit is set.
func (a Annotations) Has(want Annotations) bool { return a&want != 0 }

// PairKey identifies one directed origin→destination estimate within a service
// area, at a resolution and profile. A→B and B→A are distinct keys (asymmetry is
// real; see §9). Area partitions pairs so multiple service areas share one Store
// and FreshnessIndex without colliding: two areas covering overlapping geography
// at the same resolution produce the same Origin/Dest/Res/Profile but distinct
// Area, so their estimates and freshness state stay separate.
type PairKey struct {
	Profile Profile
	Area    AreaID
	Origin  H3Cell
	Dest    H3Cell
	Res     int
}

// Estimate is the scalar answer Beeline serves: duration and/or distance. It
// deliberately carries no route geometry (§2 non-goals).
type Estimate struct {
	Duration float64 // seconds
	Distance float64 // meters
}

// Stored is an Estimate together with the time it was computed, which the read
// path uses to decide staleness and the freshness index uses to schedule refresh.
type Stored struct {
	ComputedAt time.Time
	Estimate
}

// Entry pairs a key with its stored value for writes.
type Entry struct {
	Stored
	Key PairKey
}

// Capabilities describes what a RoutingEngine can do, so callers can plan the
// pair set around its limits (§6).
type Capabilities struct {
	SupportedProfiles []Profile
	MaxTableSize      int // bound on matrix size
	SupportsDistance  bool
}

// TableRequest asks a RoutingEngine for the full cartesian product of Sources ×
// Destinations. 1×K (one origin, its neighbor ring) is the primary, fully-utilized
// shape (§6).
type TableRequest struct {
	Profile      Profile
	Sources      []LatLng
	Destinations []LatLng
	Want         Annotations
}

// TableResponse holds the dense result matrices, len(Sources) × len(Destinations).
// Distance is populated only when requested and supported.
type TableResponse struct {
	Duration [][]float64 // seconds
	Distance [][]float64 // meters
}

// DebtStats is the freshness contract made observable (§3): whether the cluster is
// keeping the working set younger than the target TTL, and at what throughput. The
// json tags make it the wire shape of GET /_ops_/freshness.
type DebtStats struct {
	WorkingSet         int     `json:"workingSet"`         // total pairs tracked
	Debt               int     `json:"debt"`               // pairs older than the target TTL (incl. never-computed)
	OldestAgeSeconds   float64 `json:"oldestAgeSeconds"`   // p100 staleness among computed pairs
	RequiredThroughput float64 `json:"requiredThroughput"` // entries/sec needed = WorkingSet / targetTTL
	AchievedThroughput float64 `json:"achievedThroughput"` // entries/sec actually sustained
}

// CellState is a per-origin-cell rollup of the freshness index, used to paint the
// cache-loading progress map: for one origin cell, how many of its outgoing pairs
// exist and how many are currently fresh (computed and younger than the target TTL).
type CellState struct {
	Origin           H3Cell  // origin cell of the rolled-up pairs
	Total            int     // outgoing pairs from this origin in the working set
	Fresh            int     // outgoing pairs that are computed and within the TTL
	OldestAgeSeconds float64 // p100 staleness among this cell's computed pairs
}

// Selector chooses a subset of the index for Invalidate. A pair whose ComputedAt
// predates OlderThan is re-enqueued for refresh.
type Selector struct {
	OlderThan time.Time
}
