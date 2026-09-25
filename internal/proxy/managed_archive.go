package proxy

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vmihailenco/msgpack/v5"
	xdraw "golang.org/x/image/draw"
)

const maxCaptureBytes = 128 << 20
const maxImageBytes = 32 << 20

type archiveWork struct {
	ID, GroupID, KeyID, KeyName, IP, Route string
	Completed                              time.Time
}

type archiveManager struct {
	db   *stateDB
	dir  string
	wake chan struct{}
	mu   sync.Mutex
}

func newArchiveManager(db *stateDB, statePath string, settings *settingsStore) (*archiveManager, error) {
	a := &archiveManager{db: db, dir: statePath + ".archive", wake: make(chan struct{}, 1)}
	if err := os.MkdirAll(a.dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(a.dir, 0700); err != nil {
		return nil, err
	}
	if err := a.cleanupTemps(0); err != nil {
		return nil, err
	}
	if err := a.cleanupOrphans(); err != nil {
		return nil, err
	}
	go a.run(settings)
	a.signal()
	return a, nil
}

func (a *archiveManager) signal() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func (a *archiveManager) run(settings *settingsStore) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-a.wake:
		case <-ticker.C:
		}
		for {
			work, ok, err := a.nextWork()
			if err != nil {
				a.recordFailure(err)
				slog.Error("archive work read failed", "error", err)
				break
			}
			if !ok {
				break
			}
			if err := a.process(work); err != nil {
				a.recordFailure(err)
				slog.Warn("archive processing failed", "error", err)
			}
			if _, err := a.db.db.Exec("DELETE FROM archive_work WHERE id=?", work.ID); err != nil {
				slog.Error("archive work cleanup failed", "error", err)
				break
			}
			_ = os.Remove(a.path(work.ID, ".capture"))
		}
		if err := a.prune(settings.snapshot()); err != nil {
			a.recordFailure(err)
		}
		if err := a.cleanupTemps(time.Hour); err != nil {
			a.recordFailure(err)
		}
		if err := a.cleanupOrphans(); err != nil {
			a.recordFailure(err)
		}
	}
}

func (a *archiveManager) path(id, suffix string) string { return filepath.Join(a.dir, id+suffix) }

func (a *archiveManager) cleanupTemps(olderThan time.Duration) error {
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".archive-") || strings.HasPrefix(e.Name(), ".thumb-") {
			info, err := e.Info()
			if err != nil {
				return err
			}
			if olderThan == 0 || time.Since(info.ModTime()) > olderThan {
				_ = os.Remove(filepath.Join(a.dir, e.Name()))
			}
		}
	}
	return nil
}

func (a *archiveManager) cleanupOrphans() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	used := map[string]bool{}
	images, err := a.db.db.Query("SELECT original,thumbnail FROM images")
	if err != nil {
		return err
	}
	for images.Next() {
		var original, thumb string
		if err := images.Scan(&original, &thumb); err != nil {
			_ = images.Close()
			return err
		}
		used[original], used[thumb] = true, true
	}
	if err := images.Err(); err != nil {
		_ = images.Close()
		return err
	}
	_ = images.Close()
	work, err := a.db.db.Query("SELECT id FROM archive_work")
	if err != nil {
		return err
	}
	for work.Next() {
		var id string
		if err := work.Scan(&id); err != nil {
			_ = work.Close()
			return err
		}
		used[id+".capture"] = true
	}
	if err := work.Err(); err != nil {
		_ = work.Close()
		return err
	}
	_ = work.Close()
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if !used[name] && (strings.HasSuffix(name, ".capture") || strings.HasSuffix(name, ".png") || strings.HasSuffix(name, ".jpg")) {
			_ = os.Remove(filepath.Join(a.dir, name))
		}
	}
	return nil
}

func (a *archiveManager) nextWork() (archiveWork, bool, error) {
	var data []byte
	err := a.db.db.QueryRow("SELECT data FROM archive_work ORDER BY created_at,id LIMIT 1").Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return archiveWork{}, false, nil
	}
	if err != nil {
		return archiveWork{}, false, err
	}
	var work archiveWork
	if err := json.Unmarshal(data, &work); err != nil {
		return work, true, err
	}
	return work, true, nil
}

func (a *archiveManager) queue(work archiveWork, staged string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.Rename(staged, a.path(work.ID, ".capture")); err != nil {
		return err
	}
	if dir, err := os.Open(a.dir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	data, err := json.Marshal(work)
	if err != nil {
		return err
	}
	_, err = a.db.db.Exec("INSERT INTO archive_work(id,data,created_at) VALUES(?,?,?)", work.ID, data, work.Completed.Unix())
	if err != nil {
		_ = os.Remove(a.path(work.ID, ".capture"))
		return err
	}
	a.signal()
	return nil
}

func (a *archiveManager) recordFailure(err error) {
	if err == nil {
		return
	}
	message := err.Error()
	if len(message) > 400 {
		message = message[:400]
	}
	_, dbErr := a.db.db.Exec("INSERT INTO meta(name,value) VALUES('archive_failures','1') ON CONFLICT(name) DO UPDATE SET value=CAST(value AS INTEGER)+1")
	if dbErr == nil {
		_, dbErr = a.db.db.Exec("INSERT INTO meta(name,value) VALUES('archive_last_error',?) ON CONFLICT(name) DO UPDATE SET value=excluded.value", message)
	}
	if dbErr != nil {
		slog.Error("archive failure counter unavailable", "error", dbErr)
	}
}

type archiveWriter struct {
	http.ResponseWriter
	file   *os.File
	size   int64
	status int
	err    error
}

func (w *archiveWriter) Header() http.Header { return w.ResponseWriter.Header() }
func (w *archiveWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *archiveWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	if n > 0 && w.status >= 200 && w.status < 300 && w.err == nil {
		if w.size+int64(n) > maxCaptureBytes {
			w.err = errors.New("archive response exceeds capture limit")
		} else {
			m, writeErr := w.file.Write(p[:n])
			w.size += int64(m)
			if writeErr != nil {
				w.err = writeErr
			} else if m != n {
				w.err = io.ErrShortWrite
			}
		}
	}
	return n, err
}
func (w *archiveWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (a *archiveManager) capture(w http.ResponseWriter) (*archiveWriter, error) {
	f, err := os.CreateTemp(a.dir, ".archive-*")
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	return &archiveWriter{ResponseWriter: w, file: f}, nil
}

func (a *archiveManager) finishCapture(w *archiveWriter, work archiveWork, success bool) {
	defer os.Remove(w.file.Name())
	if success && w.err == nil {
		if err := w.file.Sync(); err != nil {
			w.err = err
		}
	}
	if err := w.file.Close(); err != nil && w.err == nil {
		w.err = err
	}
	if !success || w.status < 200 || w.status >= 300 {
		return
	}
	if w.err != nil {
		a.recordFailure(w.err)
		return
	}
	if w.size == 0 {
		a.recordFailure(errors.New("empty image response"))
		return
	}
	if err := a.queue(work, w.file.Name()); err != nil {
		a.recordFailure(err)
	}
}

func (a *archiveManager) process(work archiveWork) (retErr error) {
	path := a.path(work.ID, ".capture")
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	images, err := extractImages(f, info.Size(), work.Route)
	if err != nil {
		return err
	}
	created := []string{}
	defer func() {
		if retErr != nil {
			for _, path := range created {
				_ = os.Remove(path)
			}
		}
	}()
	tx, err := a.db.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, data := range images {
		id := fmt.Sprintf("%s_%02d", work.ID, i)
		original := a.path(id, ".png")
		thumb := a.path(id, ".thumb.jpg")
		if err = writePrivate(original, data); err != nil {
			return err
		}
		created = append(created, original)
		preview, err := thumbnailJPEG(data)
		if err != nil {
			return err
		}
		if err = writePrivate(thumb, preview); err != nil {
			return err
		}
		created = append(created, thumb)
		_, err = tx.Exec("INSERT INTO images(id,group_id,key_id,key_name,ip,created_at,bytes,original,thumbnail) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING", id, work.GroupID, work.KeyID, work.KeyName, work.IP, work.Completed.Unix(), len(data)+len(preview), filepath.Base(original), filepath.Base(thumb))
		if err != nil {
			return err
		}
	}
	if _, err := tx.Exec("INSERT INTO meta(name,value) VALUES('archive_ever','1') ON CONFLICT(name) DO UPDATE SET value='1'"); err != nil {
		return err
	}
	err = tx.Commit()
	return err
}

func writePrivate(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".archive-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func thumbnailJPEG(data []byte) ([]byte, error) {
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 16_000_000 {
		return nil, errors.New("invalid image dimensions")
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	bounds := img.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 || int64(bounds.Dx())*int64(bounds.Dy()) > 100_000_000 {
		return nil, errors.New("invalid image dimensions")
	}
	w, h := bounds.Dx(), bounds.Dy()
	if w > 320 || h > 320 {
		scale := min(float64(320)/float64(w), float64(320)/float64(h))
		w = max(1, int(float64(w)*scale))
		h = max(1, int(float64(h)*scale))
	}
	for edge := 320; edge >= 64; edge = edge * 3 / 4 {
		targetW, targetH := w, h
		if targetW > edge || targetH > edge {
			scale := min(float64(edge)/float64(targetW), float64(edge)/float64(targetH))
			targetW = max(1, int(float64(targetW)*scale))
			targetH = max(1, int(float64(targetH)*scale))
		}
		out := image.NewRGBA(image.Rect(0, 0, targetW, targetH))
		draw.Draw(out, out.Bounds(), image.NewUniform(image.White), image.Point{}, draw.Src)
		xdraw.ApproxBiLinear.Scale(out, out.Bounds(), img, bounds, draw.Over, nil)
		for quality := 82; quality >= 35; quality -= 12 {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: quality}); err != nil {
				return nil, err
			}
			if buf.Len() <= 100<<10 {
				return buf.Bytes(), nil
			}
		}
	}
	return nil, errors.New("thumbnail exceeds 100 KiB")
}

func extractImages(f *os.File, size int64, route string) ([][]byte, error) {
	if size <= 0 || size > maxCaptureBytes {
		return nil, errors.New("invalid capture size")
	}
	if strings.HasSuffix(route, "generate-image-stream") {
		return extractStream(f)
	}
	var signature [8]byte
	if _, err := f.ReadAt(signature[:], 0); err != nil {
		return nil, err
	}
	if bytes.Equal(signature[:], []byte("\x89PNG\r\n\x1a\n")) {
		data, err := io.ReadAll(io.LimitReader(f, maxImageBytes+1))
		if err != nil {
			return nil, err
		}
		if err := validatePNG(data); err != nil {
			return nil, err
		}
		return [][]byte{data}, nil
	}
	if !bytes.HasPrefix(signature[:], []byte("PK\x03\x04")) {
		return nil, errors.New("unsupported image response")
	}
	z, err := zip.NewReader(f, size)
	if err != nil {
		return nil, err
	}
	if len(z.File) > 32 {
		return nil, errors.New("too many zip entries")
	}
	var images [][]byte
	var total int64
	for _, entry := range z.File {
		if entry.FileInfo().IsDir() || !strings.EqualFold(filepath.Ext(entry.Name), ".png") {
			continue
		}
		if entry.UncompressedSize64 > maxImageBytes {
			return nil, errors.New("zip image too large")
		}
		total += int64(entry.UncompressedSize64)
		if total > 128<<20 {
			return nil, errors.New("zip images exceed size limit")
		}
		r, err := entry.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(r, maxImageBytes+1))
		closeErr := r.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if err := validatePNG(data); err != nil {
			return nil, err
		}
		total += int64(len(data))
		if total > 128<<20 || len(images) >= 4 {
			return nil, errors.New("zip contains too many image bytes")
		}
		images = append(images, data)
	}
	if len(images) == 0 {
		return nil, errors.New("zip contains no PNG")
	}
	return images, nil
}

func validatePNG(data []byte) error {
	if len(data) > maxImageBytes {
		return errors.New("image too large")
	}
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return err
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 16_000_000 {
		return errors.New("image dimensions too large")
	}
	_, err = png.Decode(bytes.NewReader(data))
	return err
}

func extractStream(r io.Reader) ([][]byte, error) {
	var final []byte
	for {
		var header [4]byte
		_, err := io.ReadFull(r, header[:])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		length := binary.BigEndian.Uint32(header[:])
		if length == 0 || length > maxImageBytes {
			return nil, errors.New("invalid stream frame length")
		}
		frame := make([]byte, length)
		if _, err := io.ReadFull(r, frame); err != nil {
			return nil, err
		}
		var message map[string]any
		if err := msgpack.Unmarshal(frame, &message); err != nil {
			return nil, err
		}
		if code, ok := message["code"]; ok && code != nil && fmt.Sprint(code) != "200" {
			return nil, errors.New("stream error frame")
		}
		if message["step_ix"] == nil {
			if data, ok := message["image"].([]byte); ok {
				final = data
			}
		}
	}
	if err := validatePNG(final); err != nil {
		return nil, fmt.Errorf("no valid final image: %w", err)
	}
	return [][]byte{final}, nil
}

func (a *archiveManager) prune(settings proxySettings) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	rows, err := a.db.db.Query("SELECT id,created_at,bytes,original,thumbnail FROM images ORDER BY created_at,id")
	if err != nil {
		return err
	}
	defer rows.Close()
	type item struct {
		id              string
		created, bytes  int64
		original, thumb string
	}
	var all []item
	var total int64
	for rows.Next() {
		var x item
		if err := rows.Scan(&x.id, &x.created, &x.bytes, &x.original, &x.thumb); err != nil {
			return err
		}
		all = append(all, x)
		total += x.bytes
	}
	if err := rows.Err(); err != nil {
		return err
	}
	cutoff := time.Now().AddDate(0, 0, -settings.ArchiveDays).Unix()
	for _, x := range all {
		if (settings.ArchiveDays >= 0 && x.created < cutoff) || total > settings.ArchiveMaxBytes {
			if err := a.deleteImage(x.id, x.original, x.thumb); err != nil {
				return err
			}
			total -= x.bytes
		}
	}
	return nil
}

func (a *archiveManager) deleteImage(id, original, thumb string) error {
	if _, err := a.db.db.Exec("DELETE FROM images WHERE id=?", id); err != nil {
		return err
	}
	_ = os.Remove(a.path(original, ""))
	_ = os.Remove(a.path(thumb, ""))
	return nil
}
