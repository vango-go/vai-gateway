package tts

// getFormat normalizes a format string to a known audio format.
// Defaults to "wav" for unrecognized formats.
func getFormat(format string) string {
	switch format {
	case "mp3", "pcm", "raw", "wav":
		return format
	default:
		return "wav"
	}
}
