package logging

import (
	"bytes"
	"strings"
	"testing"
)

func TestDebugfIsSuppressedWhenDebugDisabled(t *testing.T) {
	var out bytes.Buffer

	logger, err := New("", &out, false)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	logger.Debugf("hidden message")
	logger.Infof("visible message")

	got := out.String()
	if strings.Contains(got, "hidden message") {
		t.Fatalf("expected debug message to be suppressed, got %q", got)
	}
	if !strings.Contains(got, "visible message") {
		t.Fatalf("expected info message to be logged, got %q", got)
	}
}

func TestWriterSplitsAndBuffersProgressLines(t *testing.T) {
	var out bytes.Buffer

	logger, err := New("", &out, true)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	writer := logger.Writer(LevelDebug)
	if _, err := writer.Write([]byte("first line\nsecond")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if _, err := writer.Write([]byte(" line\rthird line\n")); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	got := out.String()
	for _, want := range []string{"first line", "second line", "third line"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected progress log %q, got %q", want, got)
		}
	}
}
