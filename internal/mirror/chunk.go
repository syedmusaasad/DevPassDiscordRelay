// Package mirror contains the core relay logic: chunking, throttle, and part classification.
package mirror

import (
	"strings"
	"unicode/utf8"
)

// ChunkText splits text into chunks of at most maxLen bytes (rune-safe),
// preferring to break at natural boundaries (paragraph, sentence, word, newline).
// maxLen is in characters (runes). Zero means use default 2000.
func ChunkText(text string, maxLen int) []string {
	if maxLen <= 0 {
		maxLen = 2000
	}
	if utf8.RuneCountInString(text) <= maxLen {
		return []string{text}
	}

	var chunks []string
	for utf8.RuneCountInString(text) > maxLen {
		idx := findBreak(text, maxLen)
		chunks = append(chunks, text[:idx])
		text = text[idx:]
	}
	if text != "" {
		chunks = append(chunks, text)
	}
	return chunks
}

// findBreak finds the best byte index to split at (≤ maxLen runes from start).
// Preference order: double-newline, single newline, sentence end (. ! ?), word space.
func findBreak(text string, maxLen int) int {
	// Convert maxLen runes to a byte limit
	byteLimit := runeIndexToByte(text, maxLen)
	if byteLimit <= 0 {
		byteLimit = len(text)
	}

	sub := text[:byteLimit]

	// Prefer double-newline (paragraph boundary)
	if i := strings.LastIndex(sub, "\n\n"); i > 0 {
		return i + 2
	}
	// Single newline
	if i := strings.LastIndex(sub, "\n"); i > 0 {
		return i + 1
	}
	// Sentence ends: '. ' or '! ' or '? '
	for _, sep := range []string{". ", "! ", "? "} {
		if i := strings.LastIndex(sub, sep); i > 0 {
			return i + len(sep)
		}
	}
	// Word space
	if i := strings.LastIndex(sub, " "); i > 0 {
		return i + 1
	}
	// Hard cut at byte limit
	return byteLimit
}

// runeIndexToByte converts a rune count index to a byte index in s.
func runeIndexToByte(s string, n int) int {
	count := 0
	for i := range s {
		if count == n {
			return i
		}
		count++
	}
	return len(s)
}
