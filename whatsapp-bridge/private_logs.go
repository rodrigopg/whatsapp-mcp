package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// logContentEnabled reports whether message text may be written to the log (WHATSAPP_LOG_CONTENT).
func logContentEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WHATSAPP_LOG_CONTENT"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// logMessage prints one line per stored message. By default it carries only metadata: the log is a
// plain file that outlives the chat and is pasted into bug reports, so the text stays out of it
// unless the owner opts in with WHATSAPP_LOG_CONTENT.
func logMessage(timestamp time.Time, fromMe bool, chat, sender, mediaType, content string) {
	direction := "in"
	if fromMe {
		direction = "out"
	}
	if mediaType == "" {
		mediaType = "text"
	}
	line := fmt.Sprintf("[%s] direction=%s chat=%q sender=%q type=%q length=%d", timestamp.Format(time.RFC3339), direction, chat, sender, mediaType, len(content))
	if logContentEnabled() {
		line += fmt.Sprintf(" content=%q", content)
	}
	fmt.Println(line)
}
