package documents

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// allowed maps the extensions accepted for upload to their Content-Type.
var allowed = map[string]string{
	"pdf":  "application/pdf",
	"doc":  "application/msword",
	"docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"xls":  "application/vnd.ms-excel",
	"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"ppt":  "application/vnd.ms-powerpoint",
	"pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	"odt":  "application/vnd.oasis.opendocument.text",
	"ods":  "application/vnd.oasis.opendocument.spreadsheet",
	"csv":  "text/csv",
	"txt":  "text/plain",
	"jpg":  "image/jpeg",
	"jpeg": "image/jpeg",
	"png":  "image/png",
	"gif":  "image/gif",
	"zip":  "application/zip",
}

// AllowedExtensions is the upload allow-list, for the UI.
const AllowedExtensions = "pdf doc docx xls xlsx ppt pptx odt ods csv txt jpg jpeg png gif zip"

// FallbackMime is served when a file's type is unknown.
const FallbackMime = "application/octet-stream"

func ext(name string) string {
	return strings.ToLower(strings.TrimPrefix(path.Ext(strings.ReplaceAll(name, `\`, "/")), "."))
}

// AllowedUpload reports whether name has an allowed extension, and its
// Content-Type.
func AllowedUpload(name string) (string, bool) {
	m, ok := allowed[ext(name)]
	return m, ok
}

// MimeFor is the Content-Type stored for a file name: the allow-list's, or
// application/octet-stream for anything else (legacy files such as .html are
// then never rendered by a browser).
func MimeFor(name string) string {
	if m, ok := allowed[ext(name)]; ok {
		return m
	}
	return FallbackMime
}

// SanitizeFilename makes an original file name safe for
// Content-Disposition: no directories, control characters, quotes or
// separators; at most 150 characters; never empty.
func SanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	name = path.Base(name)
	name = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r), r == '"', r == '/', r == ';', r == ':', r == '*', r == '?', r == '<', r == '>', r == '|':
			return '_'
		case unicode.IsSpace(r):
			return ' '
		}
		return r
	}, name)
	name = strings.Trim(strings.TrimSpace(name), ".")
	if r := []rune(name); len(r) > 150 {
		e := path.Ext(name)
		if len([]rune(e)) > 10 {
			e = ""
		}
		name = string(r[:150-len([]rune(e))]) + e
	}
	if name == "" || name == "." {
		return "document"
	}
	return name
}

var storedNameRE = regexp.MustCompile(`^[0-9a-f]{32}\.bin$`)

// NewStoredName returns a random name for a file under DOCUMENTS_DIR.
func NewStoredName() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:]) + ".bin"
}

// Files stores document contents under Dir, outside any web root.
type Files struct{ Dir string }

func (f Files) path(stored string) (string, error) {
	if !storedNameRE.MatchString(stored) {
		return "", fmt.Errorf("invalid stored name %q", stored)
	}
	return filepath.Join(f.Dir, stored), nil
}

// ErrTooLarge is returned by Save when the content exceeds the limit.
var ErrTooLarge = errors.New("file too large")

// Saved describes a file written by Save.
type Saved struct {
	StoredName string
	Size       int64
	SHA256     string
}

// Save writes r to a new stored file (via a temporary file and rename),
// refusing more than max bytes when max > 0, and extra receives the bytes
// too (e.g. an md5 for verification) when not nil.
func (f Files) Save(r io.Reader, max int64, extra io.Writer) (Saved, error) {
	if err := os.MkdirAll(f.Dir, 0o750); err != nil {
		return Saved{}, err
	}
	tmp, err := os.CreateTemp(f.Dir, ".upload-*")
	if err != nil {
		return Saved{}, err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	h := sha256.New()
	w := io.MultiWriter(tmp, h)
	if extra != nil {
		w = io.MultiWriter(tmp, h, extra)
	}
	src := r
	if max > 0 {
		src = io.LimitReader(r, max+1)
	}
	n, err := io.Copy(w, src)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Saved{}, err
	}
	if max > 0 && n > max {
		return Saved{}, ErrTooLarge
	}
	s := Saved{StoredName: NewStoredName(), Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}
	dst, _ := f.path(s.StoredName)
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return Saved{}, err
	}
	return s, nil
}

// Open opens a stored file for reading.
func (f Files) Open(stored string) (*os.File, error) {
	p, err := f.path(stored)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

// Remove deletes a stored file; a missing file is not an error.
func (f Files) Remove(stored string) error {
	p, err := f.path(stored)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Hash returns the sha256 of a stored file.
func (f Files) Hash(stored string) (string, int64, error) {
	fh, err := f.Open(stored)
	if err != nil {
		return "", 0, err
	}
	defer fh.Close()
	h := sha256.New()
	n, err := io.Copy(h, fh)
	return hex.EncodeToString(h.Sum(nil)), n, err
}
