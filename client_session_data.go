package gohlslib

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/limitedreader"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"
)

const clientMaxInboundSessionDataSize = 1 * 1024 * 1024

// SessionData is a EXT-X-SESSION-DATA entry of the primary playlist.
type SessionData struct {
	// DATA-ID
	DataID string

	// LANGUAGE
	Language string

	// VALUE, or the content downloaded from URI
	Value *string

	// absolute URI from which the value has been downloaded
	URI *string
}

func sessionDataEqual(a *SessionData, b *SessionData) bool {
	if a.DataID != b.DataID || a.Language != b.Language {
		return false
	}
	if (a.Value == nil) != (b.Value == nil) || (a.Value != nil && *a.Value != *b.Value) {
		return false
	}
	if (a.URI == nil) != (b.URI == nil) || (a.URI != nil && *a.URI != *b.URI) {
		return false
	}
	return true
}

func sortSessionData(entries []*SessionData) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].DataID != entries[j].DataID {
			return entries[i].DataID < entries[j].DataID
		}
		return entries[i].Language < entries[j].Language
	})
}

// updateSessionData applies changes to the current session data snapshot
// and forwards them to the OnSessionData callback.
func (c *Client) updateSessionData(added []*SessionData, removed []*SessionData) {
	c.sessionDataMutex.Lock()

	for _, re := range removed {
		for i, e := range c.sessionData {
			if e.DataID == re.DataID && e.Language == re.Language {
				c.sessionData = append(c.sessionData[:i], c.sessionData[i+1:]...)
				break
			}
		}
	}

	for _, ad := range added {
		replaced := false
		for i, e := range c.sessionData {
			if e.DataID == ad.DataID && e.Language == ad.Language {
				c.sessionData[i] = ad
				replaced = true
				break
			}
		}
		if !replaced {
			c.sessionData = append(c.sessionData, ad)
		}
	}

	c.sessionDataMutex.Unlock()

	c.OnSessionData(added, removed)
}

type sessionDataKey struct {
	dataID   string
	language string
}

func (d *clientPrimaryDownloader) downloadSessionDataValue(
	errGroupCtx context.Context,
	u *url.URL,
) (string, error) {
	req, err := http.NewRequestWithContext(errGroupCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}

	d.onRequest(req)

	res, err := d.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bad status code: %d", res.StatusCode)
	}

	byts, err := io.ReadAll(&limitedreader.Reader{R: res.Body, N: clientMaxInboundSessionDataSize})
	if err != nil {
		return "", err
	}

	return string(byts), nil
}

// processSessionData resolves the session data entries of a primary playlist
// and notifies entries that changed since the last call.
func (d *clientPrimaryDownloader) processSessionData(
	errGroupCtx context.Context,
	playlistURL *url.URL,
	entries []*playlist.MultivariantSessionData,
) {
	next := make(map[sessionDataKey]*SessionData)

	for _, e := range entries {
		sd := &SessionData{
			DataID:   e.DataID,
			Language: e.Language,
		}

		if e.URI != nil {
			u, err := clientAbsoluteURL(playlistURL, *e.URI)
			if err != nil {
				d.onDecodeError(fmt.Errorf("unable to parse session data URI: %w", err))
				continue
			}
			sd.URI = new(u.String())

			var val string
			val, err = d.downloadSessionDataValue(errGroupCtx, u)
			if err != nil {
				if errGroupCtx.Err() != nil {
					return
				}
				d.onDecodeError(fmt.Errorf("unable to download session data \"%s\": %w", e.DataID, err))
				continue
			}
			sd.Value = new(val)
		} else {
			sd.Value = new(*e.Value)
		}

		next[sessionDataKey{dataID: sd.DataID, language: sd.Language}] = sd
	}

	var added []*SessionData
	for key, sd := range next {
		prev, ok := d.sessionData[key]
		if !ok || !sessionDataEqual(prev, sd) {
			added = append(added, sd)
		}
	}

	var removed []*SessionData
	for key, sd := range d.sessionData {
		if _, ok := next[key]; !ok {
			removed = append(removed, sd)
		}
	}

	d.sessionData = next

	if len(added) != 0 || len(removed) != 0 {
		sortSessionData(added)
		sortSessionData(removed)
		d.parent.updateSessionData(added, removed)
	}
}

// runPrimaryPlaylistRefresher periodically re-downloads the primary playlist
// of a multivariant stream, in order to detect session data changes.
// Variant and rendition selection is intentionally not affected by refreshes.
func (d *clientPrimaryDownloader) runPrimaryPlaylistRefresher(errGroupCtx context.Context) error {
	ticker := time.NewTicker(d.primaryPlaylistRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-errGroupCtx.Done():
			return nil
		case <-ticker.C:
		}

		d.onDownloadPrimaryPlaylist(d.primaryPlaylistURL.String())

		_, pl, err := downloadPlaylist(errGroupCtx, d.httpClient, d.onRequest, d.primaryPlaylistURL)
		if err != nil {
			if errGroupCtx.Err() != nil {
				return nil
			}
			d.onDecodeError(fmt.Errorf("unable to refresh primary playlist: %w", err))
			continue
		}

		if mplt, ok := pl.(*playlist.Multivariant); ok {
			d.processSessionData(errGroupCtx, d.primaryPlaylistURL, mplt.SessionData)
		}
	}
}
