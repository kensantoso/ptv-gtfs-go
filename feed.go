package gtfs

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// DefaultFeedURL is Victoria's published GTFS bundle. No credential is needed:
// unlike the realtime feeds and the Timetable API, this is a plain download.
const DefaultFeedURL = "https://data.ptv.vic.gov.au/downloads/gtfs.zip"

// Mode identifies one of the sub-feeds inside the bundle. Victoria nests a
// google_transit.zip per mode in numbered folders, so the numbers below are
// directory names in the outer archive, not GTFS route_type values.
type Mode int

const (
	ModeRegionalTrain Mode = 1
	ModeMetroTrain    Mode = 2
	ModeMetroTram     Mode = 3
	ModeMetroBus      Mode = 4
	ModeRegionalCoach Mode = 5
	ModeRegionalBus   Mode = 6
	ModeTeleBus       Mode = 7
	ModeNightBus      Mode = 8
	ModeInterstate    Mode = 10
	ModeSkyBus        Mode = 11
)

func (m Mode) String() string {
	switch m {
	case ModeRegionalTrain:
		return "regional train"
	case ModeMetroTrain:
		return "metro train"
	case ModeMetroTram:
		return "tram"
	case ModeMetroBus:
		return "bus"
	case ModeRegionalCoach:
		return "regional coach"
	case ModeRegionalBus:
		return "regional bus"
	case ModeTeleBus:
		return "telebus"
	case ModeNightBus:
		return "night bus"
	case ModeInterstate:
		return "interstate train"
	case ModeSkyBus:
		return "skybus"
	}
	return "mode " + strconv.Itoa(int(m))
}

// AllModes is every mode worth indexing for journey planning. Interstate and
// SkyBus are omitted: they serve journeys this is not trying to plan.
var AllModes = []Mode{
	ModeRegionalTrain, ModeMetroTrain, ModeMetroTram, ModeMetroBus,
	ModeRegionalCoach, ModeRegionalBus, ModeTeleBus, ModeNightBus,
}

// wantedFiles are the only members read from each mode's archive.
//
// shapes.txt is deliberately absent. It is route geometry for drawing maps and
// accounts for roughly 60% of the bundle; skipping it is the single largest
// saving available and costs nothing for answering questions about times.
var wantedFiles = map[string]bool{
	"stops.txt": true, "stop_times.txt": true, "trips.txt": true,
	"routes.txt": true, "calendar.txt": true, "calendar_dates.txt": true,
	"transfers.txt": true, "pathways.txt": true,
}

// Download fetches the feed bundle to dst, reporting progress.
//
// The bundle is ~280MB, so this streams to disk rather than buffering. progress
// may be nil.
func Download(ctx context.Context, url, dst string, progress func(downloaded int64)) error {
	if url == "" {
		url = DefaultFeedURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("gtfs: build request: %w", err)
	}
	// Generous: the bundle is large and the origin is not always quick.
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("gtfs: download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gtfs: download: status %d", resp.StatusCode)
	}

	// Write to a temporary name and rename on success, so an interrupted run
	// never leaves a truncated archive that looks complete.
	tmp := dst + ".partial"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("gtfs: create %s: %w", tmp, err)
	}
	var n int64
	buf := make([]byte, 1<<20)
	for {
		r, rerr := resp.Body.Read(buf)
		if r > 0 {
			if _, werr := f.Write(buf[:r]); werr != nil {
				f.Close()
				return fmt.Errorf("gtfs: write: %w", werr)
			}
			n += int64(r)
			if progress != nil {
				progress(n)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return fmt.Errorf("gtfs: read body: %w", rerr)
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("gtfs: close: %w", err)
	}
	return os.Rename(tmp, dst)
}

// ModeFiles holds one mode's raw CSV members, already extracted.
type ModeFiles struct {
	Mode  Mode
	Files map[string][]byte
}

// OpenBundle reads the outer archive and returns the requested modes' files.
//
// The bundle nests an archive per mode, so this unzips twice. Only wantedFiles
// are retained, which is what keeps memory near 400MB rather than over 1GB.
func OpenBundle(zipPath string, modes []Mode) ([]ModeFiles, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("gtfs: open %s: %w", zipPath, err)
	}
	defer zr.Close()

	want := map[string]Mode{}
	for _, m := range modes {
		want[strconv.Itoa(int(m))] = m
	}

	var out []ModeFiles
	for _, f := range zr.File {
		// Members look like "2/google_transit.zip".
		dir, name := path.Split(f.Name)
		if name != "google_transit.zip" {
			continue
		}
		mode, ok := want[strings.Trim(dir, "/")]
		if !ok {
			continue
		}
		inner, err := readInnerZip(f)
		if err != nil {
			return nil, fmt.Errorf("gtfs: mode %s: %w", mode, err)
		}
		out = append(out, ModeFiles{Mode: mode, Files: inner})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("gtfs: no requested modes found in %s", zipPath)
	}
	return out, nil
}

func readInnerZip(f *zip.File) (map[string][]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	// archive/zip needs a ReaderAt, so the inner archive is buffered whole.
	// The largest is ~75MB compressed, which is acceptable.
	raw, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, err
	}

	files := map[string][]byte{}
	for _, m := range zr.File {
		base := path.Base(m.Name)
		if !wantedFiles[base] {
			continue
		}
		r, err := m.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			return nil, err
		}
		files[base] = b
	}
	return files, nil
}
