// Package media stores the GIF and sound for each attention type on disk.
package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

const (
	MaxGIF   = 20 << 20
	MaxSound = 5 << 20
)

var soundMimes = map[string]bool{
	"audio/mpeg": true, "audio/ogg": true, "audio/wave": true, "audio/wav": true,
	"audio/x-wav": true, "audio/flac": true, "audio/x-flac": true, "application/ogg": true,
}

type Dir string

func (d Dir) GIFPath(typeID int64) string {
	return filepath.Join(string(d), fmt.Sprintf("%d.gif", typeID))
}
func (d Dir) SoundPath(typeID int64) string {
	return filepath.Join(string(d), fmt.Sprintf("%d.sound", typeID))
}

// ReadGIF reads and validates an uploaded GIF.
func ReadGIF(r io.Reader) ([]byte, error) {
	b, err := readLimited(r, MaxGIF)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(b, []byte("GIF87a")) && !bytes.HasPrefix(b, []byte("GIF89a")) {
		return nil, errors.New("not a GIF file")
	}
	return b, nil
}

// ReadSound reads an uploaded sound and returns its sniffed MIME type.
func ReadSound(r io.Reader) ([]byte, string, error) {
	b, err := readLimited(r, MaxSound)
	if err != nil {
		return nil, "", err
	}
	mime := http.DetectContentType(b)
	if bytes.HasPrefix(b, []byte("fLaC")) {
		mime = "audio/flac"
	} else if bytes.HasPrefix(b, []byte("ID3")) || (len(b) > 1 && b[0] == 0xFF && b[1]&0xE0 == 0xE0) {
		mime = "audio/mpeg"
	}
	if !soundMimes[mime] {
		return nil, "", fmt.Errorf("unsupported sound format %q (use mp3, ogg, wav or flac)", mime)
	}
	return b, mime, nil
}

func readLimited(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("file larger than %d MB", max>>20)
	}
	if len(b) == 0 {
		return nil, errors.New("empty file")
	}
	return b, nil
}

// Hash identifies a GIF+sound pair so PCs can tell when their cached copy is stale.
func Hash(gif, sound []byte) string {
	h := sha256.New()
	h.Write(gif)
	h.Write(sound)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// WriteFile writes atomically so a PC never downloads a half-written file.
func WriteFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
