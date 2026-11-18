package playlist

import (
	"fmt"
	"net/url"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist/primitives"
)

// MultivariantSessionData is a EXT-X-SESSION-DATA tag.
type MultivariantSessionData struct {
	// DATA-ID
	// required
	DataID string

	// VALUE
	// either VALUE or URI must be present
	Value *string

	// URI
	// either VALUE or URI must be present
	// must be an absolute URI
	URI *string

	// LANGUAGE
	Language string
}

func (t *MultivariantSessionData) unmarshal(v string) error {
	var attrs primitives.Attributes
	err := attrs.Unmarshal(v)
	if err != nil {
		return err
	}

	for key, val := range attrs {
		switch key {
		case "DATA-ID":
			t.DataID = val

		case "VALUE":
			t.Value = new(val)

		case "URI":
			t.URI = new(val)

		case "LANGUAGE":
			t.Language = val
		}
	}

	if t.DataID == "" {
		return fmt.Errorf("DATA-ID missing")
	}

	if t.Value == nil && t.URI == nil {
		return fmt.Errorf("either VALUE or URI is required")
	}

	if t.Value != nil && t.URI != nil {
		return fmt.Errorf("VALUE and URI cannot be both present")
	}

	if t.URI != nil {
		u, err := url.Parse(*t.URI)
		if err != nil {
			return fmt.Errorf("invalid URI: %w", err)
		}
		if !u.IsAbs() {
			return fmt.Errorf("URI is not absolute")
		}
	}

	return nil
}

func (t MultivariantSessionData) marshal() string {
	ret := "#EXT-X-SESSION-DATA:DATA-ID=\"" + t.DataID + "\""

	if t.Value != nil {
		ret += ",VALUE=\"" + *t.Value + "\""
	}

	if t.URI != nil {
		ret += ",URI=\"" + *t.URI + "\""
	}

	if t.Language != "" {
		ret += ",LANGUAGE=\"" + t.Language + "\""
	}

	ret += "\n"

	return ret
}
