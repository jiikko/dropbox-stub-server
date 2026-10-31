package stub

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"hash/fnv"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
)

// thumbnailable mirrors the file types Dropbox renders thumbnails for (images and videos).
var thumbnailable = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".bmp": true, ".tif": true, ".tiff": true,
	".heic": true, ".webp": true,
	".mp4": true, ".mov": true, ".m4v": true, ".mkv": true, ".webm": true, ".avi": true, ".wmv": true,
	".flv": true, ".mpg": true, ".mpeg": true, ".3gp": true, ".ts": true,
}

// thumbnailSizes are the sizes Dropbox accepts. Anything else is refused, which also bounds the
// image the stub allocates (an arbitrary "w100000h100000" would ask for tens of GB).
var thumbnailSizes = map[string][2]int{
	"w32h32": {32, 32}, "w64h64": {64, 64}, "w128h128": {128, 128}, "w256h256": {256, 256},
	"w480h320": {480, 320}, "w640h480": {640, 480}, "w960h640": {960, 640}, "w1024h768": {1024, 768},
	"w2048h1536": {2048, 1536},
}

var errBadSize = errors.New("unsupported thumbnail size")

// union accepts both the shorthand ("jpeg") and the tagged ({".tag": "jpeg"}) union forms.
type union string

func (u *union) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*u = union(s)
		return nil
	}
	var obj struct {
		Tag string `json:".tag"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	*u = union(obj.Tag)
	return nil
}

type thumbArg struct {
	Path     string `json:"path"` // get_thumbnail_batch entries / legacy form
	Resource *struct {
		Path string `json:"path"`
	} `json:"resource"`
	Size union `json:"size"`
}

func (a thumbArg) path() string {
	if a.Resource != nil {
		return a.Resource.Path
	}
	return a.Path
}

// renderThumbnail returns a JPEG of the requested size, colored by the path so different files
// look different in the UI.
func (s *Server) renderThumbnail(arg thumbArg) (Metadata, []byte, map[string]any, error) {
	w, h := 256, 256
	if arg.Size != "" {
		wh, ok := thumbnailSizes[string(arg.Size)]
		if !ok {
			return Metadata{}, nil, nil, errBadSize
		}
		w, h = wh[0], wh[1]
	}
	_, display, err := s.fs.lookup(arg.path())
	if err != nil {
		return Metadata{}, nil, nil, err
	}
	m, err := s.fs.metadataFor(display)
	if err != nil {
		return Metadata{}, nil, nil, err
	}
	if m.Tag != "file" || !thumbnailable[strings.ToLower(filepath.Ext(display))] {
		return m, nil, map[string]any{".tag": "unsupported_extension"}, nil
	}
	hsh := fnv.New32a()
	hsh.Write([]byte(m.PathLower))
	sum := hsh.Sum32()
	c := color.RGBA{R: uint8(sum), G: uint8(sum >> 8), B: uint8(sum >> 16), A: 255}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 60}); err != nil {
		return Metadata{}, nil, nil, err
	}
	return m, buf.Bytes(), nil, nil
}

func (s *Server) handleThumbnail(w http.ResponseWriter, r *http.Request) {
	var arg thumbArg
	if err := decodeArg(r, &arg); err != nil {
		badRequest(w, "invalid Dropbox-API-Arg: %v", err)
		return
	}
	m, data, failure, err := s.renderThumbnail(arg)
	if errors.Is(err, errBadSize) {
		badRequest(w, "Dropbox-API-Arg: size: unsupported value %q", arg.Size)
		return
	}
	if err != nil {
		writeLookupError(w, "path", err)
		return
	}
	if failure != nil {
		writeError(w, http.StatusConflict, failure[".tag"].(string), nil)
		return
	}
	result, _ := json.Marshal(map[string]any{"file_metadata": m})
	w.Header().Set("Dropbox-API-Result", escapeNonASCII(string(result)))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) handleThumbnailBatch(w http.ResponseWriter, r *http.Request) {
	var arg struct {
		Entries []thumbArg `json:"entries"`
	}
	if err := decodeArg(r, &arg); err != nil {
		badRequest(w, "invalid JSON: %v", err)
		return
	}
	if len(arg.Entries) > 25 {
		badRequest(w, "request body: entries: too many entries (max 25)")
		return
	}
	out := make([]map[string]any, 0, len(arg.Entries))
	for _, e := range arg.Entries {
		m, data, failure, err := s.renderThumbnail(e)
		switch {
		case errors.Is(err, errBadSize):
			badRequest(w, "request body: entries: size: unsupported value %q", e.Size)
			return
		case err != nil:
			out = append(out, map[string]any{".tag": "failure", "failure": map[string]any{".tag": "path", "path": map[string]any{".tag": "not_found"}}})
		case failure != nil:
			out = append(out, map[string]any{".tag": "failure", "failure": failure})
		default:
			out = append(out, map[string]any{".tag": "success", "metadata": m, "thumbnail": base64.StdEncoding.EncodeToString(data)})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

// escapeNonASCII makes a JSON string safe for an HTTP header, as Dropbox does for API results.
func escapeNonASCII(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x80 {
			b.WriteRune(r)
			continue
		}
		if r > 0xffff {
			r -= 0x10000
			b.WriteString(`\u` + strconv.FormatInt(int64(0xd800+(r>>10)), 16))
			b.WriteString(`\u` + strconv.FormatInt(int64(0xdc00+(r&0x3ff)), 16))
			continue
		}
		b.WriteString(`\u` + leftPad(strconv.FormatInt(int64(r), 16)))
	}
	return b.String()
}

func leftPad(h string) string {
	for len(h) < 4 {
		h = "0" + h
	}
	return h
}
