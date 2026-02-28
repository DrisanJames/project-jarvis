package logger

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Level represents the severity of a log entry.
type Level int

const (
	DEBUG Level = iota
	INFO
	WARN
	ERROR
)

var levelNames = map[Level]string{
	DEBUG: "DEBUG",
	INFO:  "INFO",
	WARN:  "WARN",
	ERROR: "ERROR",
}

// Logger provides structured JSON logging with optional PII redaction.
type Logger struct {
	level     Level
	mu        sync.Mutex
	redactPII bool
}

var defaultLogger = &Logger{level: INFO, redactPII: true}

// SetLevel sets the minimum log level for the default logger.
func SetLevel(l Level) { defaultLogger.level = l }

// SetRedactPII enables or disables PII redaction for the default logger.
func SetRedactPII(r bool) { defaultLogger.redactPII = r }

// Debug emits a DEBUG-level structured log entry.
func Debug(msg string, fields ...interface{}) { defaultLogger.log(DEBUG, msg, fields...) }

// Info emits an INFO-level structured log entry.
func Info(msg string, fields ...interface{}) { defaultLogger.log(INFO, msg, fields...) }

// Warn emits a WARN-level structured log entry.
func Warn(msg string, fields ...interface{}) { defaultLogger.log(WARN, msg, fields...) }

// Error emits an ERROR-level structured log entry.
func Error(msg string, fields ...interface{}) { defaultLogger.log(ERROR, msg, fields...) }

func (l *Logger) log(level Level, msg string, fields ...interface{}) {
	if level < l.level {
		return
	}

	entry := map[string]interface{}{
		"time":  time.Now().UTC().Format(time.RFC3339),
		"level": levelNames[level],
		"msg":   msg,
	}

	// Parse key-value pairs from fields
	for i := 0; i < len(fields)-1; i += 2 {
		key := fmt.Sprintf("%v", fields[i])
		val := fmt.Sprintf("%v", fields[i+1])
		if l.redactPII {
			val = redactPIIValue(key, val)
		}
		entry[key] = val
	}

	// JSON output
	data, _ := json.Marshal(entry)
	l.mu.Lock()
	fmt.Fprintln(os.Stderr, string(data))
	l.mu.Unlock()
}

var emailRegex = regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`)

func redactPIIValue(key, val string) string {
	key = strings.ToLower(key)
	// Redact email fields
	if strings.Contains(key, "email") || strings.Contains(key, "subscriber") {
		return RedactEmail(val)
	}
	// Redact any embedded emails in generic fields
	return emailRegex.ReplaceAllStringFunc(val, RedactEmail)
}
