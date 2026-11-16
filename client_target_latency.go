package gohlslib

import (
	"time"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"
)

// partOrPreloadDuration estimates the duration of the media that is in
// flight at the edge: the partial parts, plus the not yet available preload
// hint. When the playlist does not carry enough information, the target
// duration is used as a conservative estimate.
func partOrPreloadDuration(pl *playlist.Media) time.Duration {
	var ret time.Duration

	for _, part := range pl.Parts {
		ret += part.Duration
	}

	if pl.PreloadHint != nil {
		switch {
		case pl.PartInf != nil:
			ret += pl.PartInf.PartTarget
		case len(pl.Parts) > 0:
			ret += pl.Parts[len(pl.Parts)-1].Duration
		default:
			ret += time.Duration(pl.TargetDuration) * time.Second
		}
	}

	return ret
}

// edgeDuration estimates the amount of media time, measured from the start of
// the first segment of the playlist, that has been produced up to the edge.
// The estimate is based on the segment and part durations carried by the
// playlist, so it does not require EXT-X-PROGRAM-DATE-TIME to be present.
func edgeDuration(pl *playlist.Media) time.Duration {
	var ret time.Duration

	for _, seg := range pl.Segments {
		ret += seg.Duration
	}

	// The EXTINF duration of the last segment is only an estimate while the
	// segment is still being produced: replace it with the sum of its parts.
	if len(pl.Parts) > 0 {
		ret -= pl.Segments[len(pl.Segments)-1].Duration

		for _, part := range pl.Parts {
			ret += part.Duration
		}
	}

	ret += partOrPreloadDuration(pl)

	return ret
}

// segmentStartDuration returns the media time at which the segment at pos
// starts, measured from the start of the first segment of the playlist.
func segmentStartDuration(pl *playlist.Media, pos int) time.Duration {
	var ret time.Duration

	for i := range pos {
		ret += pl.Segments[i].Duration
	}

	return ret
}

// segmentEdgeDistance returns the distance between the start of the segment at
// pos and the estimated edge.
func segmentEdgeDistance(pl *playlist.Media, pos int) time.Duration {
	return edgeDuration(pl) - segmentStartDuration(pl, pos)
}

// maxSafeSegmentPos returns the position of the most recent segment that can
// be safely requested. A partial segment is only addressable when its parts
// are listed in the playlist; otherwise the last segment may still be in
// production and the previous one is the safe boundary.
func maxSafeSegmentPos(pl *playlist.Media) int {
	if len(pl.Parts) > 0 {
		return len(pl.Segments) - 1
	}

	if len(pl.Segments) < 2 {
		return 0
	}

	return len(pl.Segments) - 2
}

// closestSegmentForDistance returns the position of the segment whose edge
// distance is the closest to target.
func closestSegmentForDistance(pl *playlist.Media, target time.Duration) int {
	best := 0
	bestDiff := absDuration(segmentEdgeDistance(pl, 0) - target)

	for pos := 1; pos < len(pl.Segments); pos++ {
		diff := absDuration(segmentEdgeDistance(pl, pos) - target)
		if diff < bestDiff {
			best = pos
			bestDiff = diff
		}
	}

	return best
}

func absDuration(v time.Duration) time.Duration {
	if v < 0 {
		return -v
	}
	return v
}

// initialSegmentForTarget chooses the first segment to download when a target
// latency is configured. A target smaller than the distance of the safest
// segment is clamped to the safe boundary, since the media closer to the edge
// is not addressable yet.
func initialSegmentForTarget(pl *playlist.Media, targetLatency time.Duration) int {
	pos := closestSegmentForDistance(pl, targetLatency)

	if maxSafe := maxSafeSegmentPos(pl); pos > maxSafe {
		pos = maxSafe
	}

	return pos
}

// catchupSegmentPos chooses the segment to jump to when playback is lagging
// behind the target latency. It never returns a segment before curPos, never
// crosses a discontinuity and never returns a segment past the safe boundary.
// When there is no segment worth jumping to, curPos is returned.
func catchupSegmentPos(
	pl *playlist.Media,
	curPos int,
	targetLatency time.Duration,
) int {
	ideal := closestSegmentForDistance(pl, targetLatency)

	if maxSafe := maxSafeSegmentPos(pl); ideal > maxSafe {
		ideal = maxSafe
	}

	// do not jump backwards and do not stay on the current segment
	if ideal <= curPos {
		return curPos
	}

	// a discontinuity marks an incompatible timeline: the jump is clamped to
	// the first segment of the new timeline, which is never crossed
	for pos := curPos + 1; pos <= ideal; pos++ {
		if pl.Segments[pos].Discontinuity {
			if pos-1 <= curPos {
				return curPos
			}
			ideal = pos - 1
			break
		}
	}

	return ideal
}
