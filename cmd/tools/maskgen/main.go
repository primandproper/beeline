// Command maskgen builds a beeline road mask (design §7) for a service area from
// Overture Maps transportation data. It is the ingestion step the runtime does not
// do: given the same area spec as the config (center, resolution, area rings), it
// asks Overture which cells actually contain roads and writes the resulting H3
// cell set to a mask file that `serve` loads via BEELINE_MATRIX_ROAD_MASK_PATH.
//
// The heavy lifting — reading Overture's public GeoParquet on S3 and densifying
// road geometries into points — is delegated to the DuckDB CLI (spatial + httpfs
// extensions); this tool derives the query bbox, maps the returned points to H3
// cells with the same h3-go library the runtime uses (so the cell IDs match), and
// serializes the mask. The expensive S3 pull is cached to --cache so re-running to
// tune the mask (or building for localdev repeatedly) does not re-fetch.
//
// Usage:
//
//	maskgen --lat 30.2672 --lng -97.7431 --resolution 8 --area-rings 6 \
//	        --out config/masks/austin-res8.cells --cache artifacts/overture/austin.csv
package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/tessellate"

	"github.com/uber/h3-go/v4"
)

// defaultRelease is the Overture Maps release the mask is built from. Overture
// cuts monthly releases; override with --release to pin a different one.
const defaultRelease = "2026-06-17.0"

type options struct {
	release    string
	cache      string
	out        string
	duckdb     string
	lat        float64
	lng        float64
	resolution int
	areaRings  int
	spacing    float64
	margin     float64
	refetch    bool
}

func main() {
	log.SetFlags(0)

	opts := parseFlags()
	if err := run(&opts); err != nil {
		log.Fatalf("maskgen: %v", err)
	}
}

func parseFlags() options {
	var opts options

	flag.Float64Var(&opts.lat, "lat", 0, "service-area center latitude")
	flag.Float64Var(&opts.lng, "lng", 0, "service-area center longitude")
	flag.IntVar(&opts.resolution, "resolution", 8, "H3 resolution (must match the config's area resolution)")
	flag.IntVar(&opts.areaRings, "area-rings", 6, "rings around the center that define the area (matches config areaRings)")
	flag.StringVar(&opts.release, "release", defaultRelease, "Overture Maps release to read")
	flag.StringVar(&opts.cache, "cache", "", "path to cache the fetched road points (skips the S3 pull when present)")
	flag.StringVar(&opts.out, "out", "", "path to write the mask file (required)")
	flag.StringVar(&opts.duckdb, "duckdb", "duckdb", "duckdb CLI binary")
	flag.Float64Var(&opts.spacing, "spacing", 0.0015, "point spacing in degrees when densifying roads (~150m); must be < cell width")
	flag.Float64Var(&opts.margin, "margin", 0.02, "degrees of bbox margin beyond the area disk (headroom for runtime area growth)")
	flag.BoolVar(&opts.refetch, "refetch", false, "re-run the Overture query even if the cache exists")
	flag.Parse()

	return opts
}

func run(opts *options) error {
	if opts.out == "" {
		return fmt.Errorf("--out is required")
	}
	if opts.resolution < 0 || opts.resolution > 15 {
		return fmt.Errorf("resolution %d out of range [0,15]", opts.resolution)
	}

	// Derive the area disk (same GridDisk the runtime seeds) and the query bbox
	// around it, padded by --margin so modest runtime area growth stays covered.
	disk, box, err := areaDisk(opts)
	if err != nil {
		return err
	}
	log.Printf("area center (%.4f, %.4f) res %d, %d rings → %d-cell disk, bbox [W %.4f, S %.4f, E %.4f, N %.4f]",
		opts.lat, opts.lng, opts.resolution, opts.areaRings, len(disk), box.w, box.s, box.e, box.n)

	// Fetch road points from Overture (or reuse the cache).
	pointsPath, cleanup, err := opts.ensurePoints(box)
	if err != nil {
		return err
	}
	defer cleanup()

	// Map each road point to its H3 cell at the target resolution.
	cells, points, err := cellsFromPoints(pointsPath, opts.resolution)
	if err != nil {
		return err
	}
	log.Printf("%d road points → %d cells with road coverage", points, len(cells))

	if err = writeMask(opts.out, opts.resolution, cells); err != nil {
		return err
	}
	log.Printf("wrote %s (%d cells, resolution %d)", opts.out, len(cells), opts.resolution)

	// Report how much this mask prunes for the configured area — the number that
	// matters for the demo. Cells in the disk with no road coverage get dropped.
	roads := make(map[beeline.H3Cell]struct{}, len(cells))
	for _, c := range cells {
		roads[c] = struct{}{}
	}
	kept := 0
	for _, c := range disk {
		if _, ok := roads[c]; ok {
			kept++
		}
	}
	pruned := len(disk) - kept
	log.Printf("service-area effect: %d/%d disk cells have roads, %d pruned (%.0f%%)",
		kept, len(disk), pruned, 100*float64(pruned)/float64(len(disk)))

	return nil
}

// bbox is a lng/lat bounding box: west/east are longitudes, south/north latitudes.
type bbox struct {
	w, s, e, n float64
}

// areaDisk returns the area's H3 cells (center + area-rings, the set the runtime
// seeds) and the bounding box around them padded by the configured margin.
func areaDisk(opts *options) ([]beeline.H3Cell, bbox, error) {
	center, err := h3.LatLngToCell(h3.NewLatLng(opts.lat, opts.lng), opts.resolution)
	if err != nil {
		return nil, bbox{}, fmt.Errorf("locating center cell: %w", err)
	}

	disk, err := h3.GridDisk(center, opts.areaRings)
	if err != nil {
		return nil, bbox{}, fmt.Errorf("covering area: %w", err)
	}

	box := bbox{w: 180, s: 90, e: -180, n: -90}
	for _, c := range disk {
		boundary, boundErr := c.Boundary()
		if boundErr != nil {
			return nil, bbox{}, fmt.Errorf("cell boundary: %w", boundErr)
		}
		for _, p := range boundary {
			box.w = min(box.w, p.Lng)
			box.e = max(box.e, p.Lng)
			box.s = min(box.s, p.Lat)
			box.n = max(box.n, p.Lat)
		}
	}

	box.w -= opts.margin
	box.e += opts.margin
	box.s -= opts.margin
	box.n += opts.margin

	return disk, box, nil
}

// ensurePoints returns the path to a CSV of `lng,lat` road points for the bbox,
// running the Overture query via DuckDB unless a usable cache already exists. The
// returned cleanup removes the file only when it is a temporary (uncached) one.
func (opts *options) ensurePoints(box bbox) (path string, cleanup func(), err error) {
	noop := func() {}
	cleanup = noop

	if opts.cache != "" && !opts.refetch {
		// Reuse the cache only if it was fetched for this same bbox. A fixed cache
		// path with a changed area would otherwise silently yield a stale mask for
		// the wrong region — so on a mismatch we fall through and re-fetch.
		if _, statErr := os.Stat(opts.cache); statErr == nil {
			if cachedBBoxMatches(opts.cache, box) {
				log.Printf("using cached road points at %s (pass --refetch to rebuild)", opts.cache)

				return opts.cache, noop, nil
			}
			log.Printf("cache %s was built for a different area; re-fetching", opts.cache)
		}
	}

	path = opts.cache
	if path == "" {
		tmp, tmpErr := os.CreateTemp("", "maskgen-points-*.csv")
		if tmpErr != nil {
			return "", noop, fmt.Errorf("creating temp points file: %w", tmpErr)
		}
		if cerr := tmp.Close(); cerr != nil {
			return "", noop, fmt.Errorf("closing temp points file: %w", cerr)
		}
		path = tmp.Name()
		cleanup = func() {
			if rmErr := os.Remove(path); rmErr != nil {
				log.Printf("warning: removing temp points file %s: %v", path, rmErr)
			}
		}
	} else if mkErr := os.MkdirAll(filepath.Dir(path), 0o750); mkErr != nil { //nolint:gosec // operator-supplied cache path
		return "", noop, fmt.Errorf("creating cache dir: %w", mkErr)
	}

	if err = opts.fetchPoints(box, path); err != nil {
		cleanup()

		return "", noop, err
	}

	// Record the bbox next to a persistent cache so a later run with a different
	// area re-fetches instead of trusting stale points.
	if opts.cache != "" {
		if metaErr := writeBBoxMeta(path, box); metaErr != nil {
			cleanup()

			return "", noop, metaErr
		}
	}

	return path, cleanup, nil
}

// bboxMetaPath is the sidecar recording which bbox a cache file was fetched for.
func bboxMetaPath(cache string) string { return cache + ".bbox" }

// bboxKey is the canonical `W S E N` string a cache is tagged with. Comparing
// these formatted keys sidesteps float round-trip issues.
func bboxKey(box bbox) string {
	return fmt.Sprintf("%.6f %.6f %.6f %.6f", box.w, box.s, box.e, box.n)
}

// writeBBoxMeta records the bbox a cache was fetched for.
func writeBBoxMeta(cache string, box bbox) error {
	if err := os.WriteFile(bboxMetaPath(cache), []byte(bboxKey(box)+"\n"), 0o600); err != nil { //nolint:gosec // operator-supplied cache path
		return fmt.Errorf("writing cache metadata: %w", err)
	}

	return nil
}

// cachedBBoxMatches reports whether the cache's sidecar records the given bbox.
// A missing or unreadable sidecar counts as a mismatch (force a re-fetch).
func cachedBBoxMatches(cache string, box bbox) bool {
	data, err := os.ReadFile(bboxMetaPath(cache))
	if err != nil {
		return false
	}

	return strings.TrimSpace(string(data)) == bboxKey(box)
}

// fetchPoints runs the DuckDB query that reads Overture road segments overlapping
// the bbox, densifies each into points ~spacing degrees apart, and writes distinct
// lng/lat pairs to dest as CSV.
func (opts *options) fetchPoints(box bbox, dest string) error {
	source := fmt.Sprintf(
		"s3://overturemaps-us-west-2/release/%s/theme=transportation/type=segment/*",
		opts.release,
	)

	// Args are in appearance order: source, then the four bbox-overlap bounds
	// (xmin≤E, xmax≥W, ymin≤N, ymax≥S), the densify spacing, and the output path.
	query := fmt.Sprintf(`
INSTALL httpfs; LOAD httpfs;
INSTALL spatial; LOAD spatial;
SET s3_region='us-west-2';
COPY (
  WITH roads AS (
    SELECT geometry FROM read_parquet('%s', hive_partitioning=1)
    WHERE bbox.xmin <= %.6f AND bbox.xmax >= %.6f
      AND bbox.ymin <= %.6f AND bbox.ymax >= %.6f
      AND subtype = 'road'
      AND ST_GeometryType(geometry) = 'LINESTRING'
  )
  SELECT DISTINCT round(ST_X(p.geom), 6) AS lng, round(ST_Y(p.geom), 6) AS lat
  FROM roads, LATERAL (
    SELECT UNNEST(ST_Dump(ST_LineInterpolatePoints(
      geometry, LEAST(1.0, %.6f / NULLIF(ST_Length(geometry), 0)), true
    ))) AS p
  ) pts
) TO '%s' (HEADER false, FORMAT csv);
`, source, box.e, box.w, box.n, box.s, opts.spacing, dest)

	log.Printf("querying Overture release %s via %s (this pulls from S3)…", opts.release, opts.duckdb)

	cmd := exec.CommandContext(context.Background(), opts.duckdb, "-c", query) //nolint:gosec // operator tool; args are flags/derived geometry
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("duckdb query failed: %w", err)
	}

	return nil
}

// cellsFromPoints reads the lng/lat CSV and returns the distinct H3 cells the
// points fall in at the given resolution, plus the number of points read.
func cellsFromPoints(path string, resolution int) (cells []beeline.H3Cell, points int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("opening points file: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("closing points file: %w", cerr)
		}
	}()

	seen := make(map[beeline.H3Cell]struct{})
	reader := csv.NewReader(bufio.NewReader(f))
	reader.FieldsPerRecord = 2

	for {
		record, readErr := reader.Read()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, 0, fmt.Errorf("reading points: %w", readErr)
		}

		lng, parseErr := strconv.ParseFloat(record[0], 64)
		if parseErr != nil {
			return nil, 0, fmt.Errorf("parsing lng %q: %w", record[0], parseErr)
		}
		lat, parseErr := strconv.ParseFloat(record[1], 64)
		if parseErr != nil {
			return nil, 0, fmt.Errorf("parsing lat %q: %w", record[1], parseErr)
		}

		cell, cellErr := h3.LatLngToCell(h3.NewLatLng(lat, lng), resolution)
		if cellErr != nil {
			return nil, 0, fmt.Errorf("locating cell for (%.6f, %.6f): %w", lat, lng, cellErr)
		}
		seen[cell] = struct{}{}
		points++
	}

	cells = make([]beeline.H3Cell, 0, len(seen))
	for c := range seen {
		cells = append(cells, c)
	}

	return cells, points, nil
}

// writeMask serializes the cells to a mask file via tessellate.Mask so the format
// stays in lockstep with the runtime loader.
func writeMask(path string, resolution int, cells []beeline.H3Cell) (err error) {
	if err = os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating mask file: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("closing mask file: %w", cerr)
		}
	}()

	if err = tessellate.NewMask(resolution, cells).Write(f); err != nil {
		return fmt.Errorf("writing mask: %w", err)
	}

	return nil
}
