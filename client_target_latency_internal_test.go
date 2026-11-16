package gohlslib

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"
)

func targetLatencyPlaylist() *playlist.Media {
	pl := &playlist.Media{
		TargetDuration: 1,
		MediaSequence:  10,
	}

	for range 8 {
		pl.Segments = append(pl.Segments, &playlist.MediaSegment{
			Duration: time.Second,
			URI:      "seg",
		})
	}

	return pl
}

func TestEdgeDuration(t *testing.T) {
	t.Run("segments only", func(t *testing.T) {
		pl := targetLatencyPlaylist()
		// 8 one-second segments, no partial parts nor preload hint
		require.Equal(t, 8*time.Second, edgeDuration(pl))
	})

	t.Run("with partial parts and preload hint", func(t *testing.T) {
		pl := targetLatencyPlaylist()
		pl.PartInf = &playlist.MediaPartInf{PartTarget: 200 * time.Millisecond}
		pl.Parts = []*playlist.MediaPart{
			{Duration: 200 * time.Millisecond, URI: "p1"},
			{Duration: 200 * time.Millisecond, URI: "p2"},
		}
		pl.PreloadHint = &playlist.MediaPreloadHint{URI: "p3"}

		// edgeDuration = segments (8s) - last EXTINF (1s) + partial parts
		// (0.4s) + partOrPreloadDuration (parts 0.4s + preload 0.2s) = 8s
		require.Equal(t, 8*time.Second, edgeDuration(pl))
	})
}

func TestSegmentEdgeDistance(t *testing.T) {
	pl := targetLatencyPlaylist()

	// the first segment starts 8 seconds before the edge
	require.Equal(t, 8*time.Second, segmentEdgeDistance(pl, 0))
	// the last complete segment starts 1 second before the edge
	require.Equal(t, time.Second, segmentEdgeDistance(pl, 7))
}

func TestMaxSafeSegmentPos(t *testing.T) {
	t.Run("without parts", func(t *testing.T) {
		pl := targetLatencyPlaylist()
		// the last segment may still be in production, so the previous one is
		// the safe boundary
		require.Equal(t, 6, maxSafeSegmentPos(pl))
	})

	t.Run("with parts", func(t *testing.T) {
		pl := targetLatencyPlaylist()
		pl.Parts = []*playlist.MediaPart{{Duration: 200 * time.Millisecond, URI: "p1"}}
		// the last segment is partially available through its parts, so it is
		// addressable
		require.Equal(t, 7, maxSafeSegmentPos(pl))
	})

	t.Run("single segment", func(t *testing.T) {
		pl := &playlist.Media{TargetDuration: 1}
		pl.Segments = append(pl.Segments, &playlist.MediaSegment{Duration: time.Second})
		require.Equal(t, 0, maxSafeSegmentPos(pl))
	})
}

func TestInitialSegmentForTarget(t *testing.T) {
	t.Run("target matches a middle segment", func(t *testing.T) {
		pl := targetLatencyPlaylist()
		// target of 3s: segment at pos 5 has an edge distance of 3s
		require.Equal(t, 5, initialSegmentForTarget(pl, 3*time.Second))
	})

	t.Run("target too small is clamped to safe boundary", func(t *testing.T) {
		pl := targetLatencyPlaylist()
		// no segment can sit closer than one second without parts
		require.Equal(t, 6, initialSegmentForTarget(pl, 100*time.Millisecond))
	})
}

func TestCatchupSegmentPos(t *testing.T) {
	t.Run("jumps to the segment closest to the target", func(t *testing.T) {
		pl := targetLatencyPlaylist()
		d := &clientStreamDownloader{targetLatency: 2 * time.Second}

		// cursor at the first segment, target of 2s: jump to pos 6 (edge
		// distance 2s), which the safe boundary allows
		pos := catchupSegmentPos(pl, 0, d.targetLatency)
		require.Equal(t, 6, pos)
	})

	t.Run("never moves backwards", func(t *testing.T) {
		pl := targetLatencyPlaylist()
		// a target larger than the whole playlist would point before the
		// cursor: no jump must happen
		require.Equal(t, 7, catchupSegmentPos(pl, 7, 30*time.Second))
	})

	t.Run("does not cross a discontinuity", func(t *testing.T) {
		pl := targetLatencyPlaylist()
		pl.Segments[4].Discontinuity = true

		// jumping from pos 0 towards the edge must stop right before the
		// discontinuity at pos 4
		pos := catchupSegmentPos(pl, 0, 2*time.Second)
		require.Equal(t, 3, pos)
	})

	t.Run("stays when the discontinuity is the next segment", func(t *testing.T) {
		pl := targetLatencyPlaylist()
		pl.Segments[2].Discontinuity = true
		require.Equal(t, 1, catchupSegmentPos(pl, 1, 2*time.Second))
	})
}

func TestDownloadNextSegmentWithTarget(t *testing.T) {
	t.Run("initial segment follows the target", func(t *testing.T) {
		pl := targetLatencyPlaylist()
		d := &clientStreamDownloader{targetLatency: 3 * time.Second}

		_, pos, jumped, err := d.downloadNextSegmentWithTarget(pl)
		require.NoError(t, err)
		require.False(t, jumped)
		require.Equal(t, 5, pos)
		d.curSegmentID = new(pl.MediaSequence + pos)
	})

	t.Run("sequential while close to the target", func(t *testing.T) {
		pl := targetLatencyPlaylist()
		d := &clientStreamDownloader{targetLatency: 3 * time.Second}
		d.curSegmentID = new(15) // segment at pos 5

		_, pos, jumped, err := d.downloadNextSegmentWithTarget(pl)
		require.NoError(t, err)
		require.False(t, jumped)
		require.Equal(t, 6, pos)
	})

	t.Run("not ready when the next segment is missing", func(t *testing.T) {
		pl := targetLatencyPlaylist()
		d := &clientStreamDownloader{targetLatency: 3 * time.Second}
		d.curSegmentID = new(17) // last segment, no next one

		_, _, _, err := d.downloadNextSegmentWithTarget(pl)
		require.EqualError(t, err, "next segment not found or not ready yet")
	})

	t.Run("EOS on endlist", func(t *testing.T) {
		pl := targetLatencyPlaylist()
		pl.Endlist = true
		d := &clientStreamDownloader{targetLatency: 3 * time.Second}
		d.curSegmentID = new(17)

		_, _, _, err := d.downloadNextSegmentWithTarget(pl)
		require.ErrorIs(t, err, ErrClientEOS)
	})

	t.Run("recovers when the edge moved away", func(t *testing.T) {
		pl := targetLatencyPlaylist()
		pl.MediaSequence = 20 // the cursor (id 15) is no longer in the window
		d := &clientStreamDownloader{targetLatency: 3 * time.Second}
		d.curSegmentID = new(15)

		_, pos, jumped, err := d.downloadNextSegmentWithTarget(pl)
		require.NoError(t, err)
		require.True(t, jumped)
		require.Equal(t, maxSafeSegmentPos(pl), pos)
	})

	t.Run("edge moved away does not cross a discontinuity", func(t *testing.T) {
		pl := targetLatencyPlaylist()
		pl.MediaSequence = 20
		pl.Segments[3].Discontinuity = true
		d := &clientStreamDownloader{targetLatency: 3 * time.Second}
		d.curSegmentID = new(15)

		_, pos, jumped, err := d.downloadNextSegmentWithTarget(pl)
		require.NoError(t, err)
		require.True(t, jumped)
		require.Equal(t, 3, pos)
	})
}
