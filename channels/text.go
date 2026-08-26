package channels

// SplitText splits text into non-empty UTF-8-safe chunks containing at most
// maxRunes runes. An empty input returns nil so adapters can skip invalid empty
// platform messages.
func SplitText(text string, maxRunes int) []string {
	if text == "" || maxRunes < 1 {
		return nil
	}
	runes := []rune(text)
	chunks := make([]string, 0, (len(runes)+maxRunes-1)/maxRunes)
	for len(runes) > 0 {
		end := maxRunes
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[:end]))
		runes = runes[end:]
	}
	return chunks
}
