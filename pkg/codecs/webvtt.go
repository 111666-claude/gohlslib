package codecs

// WebVTT is a WebVTT (Web Video Text Tracks) codec.
// It is used to carry subtitle cues in HLS.
type WebVTT struct {
}

// IsVideo returns whether the codec is a video one.
func (*WebVTT) IsVideo() bool {
	return false
}

func (*WebVTT) isCodec() {
}
