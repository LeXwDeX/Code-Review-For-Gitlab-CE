package gitlab

import "strings"

// SummaryMarker starts the hidden first line of the bot's summary note.
const SummaryMarker = "<!-- gitlab-ce-review:summary"

// HasSummaryMarker reports whether the note body starts with the summary
// marker. It is the recovery key for a lost publications row.
func HasSummaryMarker(body string) bool {
	line := body
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		line = body[:i]
	}
	return strings.Contains(line, SummaryMarker)
}
