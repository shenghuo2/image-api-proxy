package proxy

import (
	"bytes"
	"net/http"
)

type statusWriter struct {
	http.ResponseWriter
	status    int
	bodyBytes int64
	prefix    [8]byte
	prefixLen int
	stream    *streamOutcome
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(data)
	w.bodyBytes += int64(n)
	if n > 0 {
		w.prefixLen += copy(w.prefix[w.prefixLen:], data[:n])
		if w.stream != nil {
			w.stream.write(data[:n])
		}
	}
	return n, err
}

func (w *statusWriter) generatedImage() bool {
	if w.stream != nil {
		return w.stream.success()
	}
	return bytes.Equal(w.prefix[:], []byte("\x89PNG\r\n\x1a\n")) || bytes.HasPrefix(w.prefix[:w.prefixLen], []byte("PK\x03\x04"))
}

func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
