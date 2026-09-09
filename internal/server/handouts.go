package server

// The handout surface (MAD-490, stage 4 of MAD-319): the material the DM
// hands to the party — reading cards and maps, at party scope.
//
//	GET    /api/campaigns/{cid}/handouts              list (published for members, all for the DM)
//	POST   /api/campaigns/{cid}/handouts              create in draft (DM)
//	GET    /api/campaigns/{cid}/handouts/{hid}        read one (published, or any for the DM)
//	PATCH  /api/campaigns/{cid}/handouts/{hid}        edit title/kind/body (DM)
//	DELETE /api/campaigns/{cid}/handouts/{hid}        remove, image included (DM)
//	POST   /api/campaigns/{cid}/handouts/{hid}/publish    hand it to the party (DM)
//	POST   /api/campaigns/{cid}/handouts/{hid}/unpublish  take it back to draft (DM)
//	POST   /api/campaigns/{cid}/handouts/{hid}/retire      close it, keep the history (DM)
//	PUT    /api/campaigns/{cid}/handouts/{hid}/image      upload the image (DM, multipart)
//	GET    /api/campaigns/{cid}/handouts/{hid}/image      the image, same scope as the read
//
// The one rule this file exists to enforce (ADR 6): a non-DM caller's
// reads go through knowledge.PlayerView, whose handout query refuses
// every status but published in the SQL — a draft is invisible to a
// player by construction, not by the handler's good behaviour, and the
// handler test plus the reflection leak test hold it there. The image
// route walks the same line before any byte is served: a member's image
// read resolves the handout through the view first, so a draft's image is
// a 404 the same as its row.
//
// Images follow the house storage pattern (the transcription rule, ADR
// 5's shape): bytes land in the handouts directory beside the database
// file — never SQLite, never /tmp, never a third-party loader (design
// invariant 6) — and the row carries the reference, the sniffed type and
// the size. The reference is server-generated; no client input ever
// reaches a path.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
	"github.com/madeofpendletonwool/grimoire/internal/knowledge"
)

// maxHandoutBytes caps one handout's markdown. A handout is a letter read
// aloud, not an archive: a megabyte is a library's worth of party mail.
const maxHandoutBytes = 1 << 20

// defaultHandoutMaxImage caps one uploaded image (maps can be large
// rasters; a disk image dropped by accident must not be).
const defaultHandoutMaxImage = 32 << 20

// HandoutOptions carries the operator-tunable knobs for the image path.
// Zero values fall back to the defaults.
type HandoutOptions struct {
	// Dir is where handout images live — beside the database file by
	// default, never /tmp (a reboot must not eat the party's maps).
	Dir string
	// MaxImageBytes caps one uploaded image.
	MaxImageBytes int64
}

// WithHandouts wires the handout image directory. The routes work without
// it (text handouts need no files); an empty dir leaves image upload and
// serve reporting unavailable rather than writing somewhere unexpected.
func (s *Server) WithHandouts(opts HandoutOptions) *Server {
	if opts.MaxImageBytes <= 0 {
		opts.MaxImageBytes = defaultHandoutMaxImage
	}
	s.handoutOpts = opts
	return s
}

/* ---------- views ---------- */

// handoutView is one handout as the surfaces render it: the reading card
// (body rides the list — a handout is its words), the status, and the
// image URL when a file hangs off the row.
type handoutView struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	Status      string `json:"status"`
	ImageURL    string `json:"image_url,omitempty"`
	ImageMime   string `json:"image_mime,omitempty"`
	ImageBytes  int64  `json:"image_bytes,omitempty"`
	CreatedBy   string `json:"created_by"`
	CreatedAt   string `json:"created_at"`
	PublishedAt string `json:"published_at,omitempty"`
	UpdatedAt   string `json:"updated_at"`
}

func toHandoutView(cid string, h *campaign.Handout) handoutView {
	v := handoutView{
		ID: h.ID, Kind: h.Kind, Title: h.Title, Body: h.Body, Status: h.Status,
		ImageMime: h.ImageMime, ImageBytes: h.ImageBytes, CreatedBy: h.CreatedBy,
		CreatedAt: h.CreatedAt.Format(http.TimeFormat),
		UpdatedAt: h.UpdatedAt.Format(http.TimeFormat),
	}
	if h.ImageRef != "" {
		v.ImageURL = "/api/campaigns/" + cid + "/handouts/" + h.ID + "/image"
	}
	if !h.PublishedAt.IsZero() {
		v.PublishedAt = h.PublishedAt.Format(http.TimeFormat)
	}
	return v
}

/* ---------- reads ---------- */

// handoutKindFilter narrows a member's list by kind after the view read —
// a display concern, not a visibility one (the rows in hand are already
// published-only; dropping maps from a list cannot leak anything).
func handoutKindFilter(kind string) func(*campaign.Handout) bool {
	return func(h *campaign.Handout) bool { return kind == "" || h.Kind == kind }
}

// handleCampaignHandouts lists the campaign's handouts: every member
// reads the published material; the DM additionally sees drafts and
// retired rows — the working desk and the history.
func (s *Server) handleCampaignHandouts(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("cid"))
	if a == nil {
		return
	}
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if kind != "" && kind != campaign.HandoutKindHandout && kind != campaign.HandoutKindMap {
		writeError(w, http.StatusBadRequest, fmt.Errorf("kind must be handout or map"))
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status != "" && (status != campaign.HandoutStatusDraft && status != campaign.HandoutStatusPublished && status != campaign.HandoutStatusRetired) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("status must be draft, published or retired"))
		return
	}
	ctx := r.Context()
	var handouts []campaign.Handout
	if a.isDM() {
		list, err := s.knowledge.Handouts(ctx, knowledge.ScopeDM, a.campaign.ID, knowledge.HandoutFilter{Kind: kind, Status: status})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		handouts = list
	} else {
		// Status is the DM's question; a member's list is the published
		// material and nothing else — refusing beats silently answering
		// a different question.
		if status != "" {
			writeError(w, http.StatusBadRequest,
				fmt.Errorf("status filtering is the dm's question; a member's list is the published material"))
			return
		}
		list, err := a.view.Handouts(ctx, a.campaign.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		keep := handoutKindFilter(kind)
		for _, h := range list {
			if keep(&h) {
				handouts = append(handouts, h)
			}
		}
	}
	views := make([]handoutView, 0, len(handouts))
	for i := range handouts {
		views = append(views, toHandoutView(a.campaign.ID, &handouts[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"handouts": views})
}

// handleCampaignHandout reads one handout: published for every member,
// any status for the DM. A draft at a player scope is the same 404 a
// missing id produces — the view's query refuses it before the handler
// can even try.
func (s *Server) handleCampaignHandout(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("cid"))
	if a == nil {
		return
	}
	var h *campaign.Handout
	var err error
	if a.isDM() {
		h, err = s.knowledge.Handout(r.Context(), knowledge.ScopeDM, a.campaign.ID, r.PathValue("hid"))
	} else {
		h, err = a.view.Handout(r.Context(), a.campaign.ID, r.PathValue("hid"))
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"handout": toHandoutView(a.campaign.ID, h)})
}

/* ---------- writes (the DM paths) ---------- */

// decodeHandoutRequest reads one JSON request body under the handout cap,
// writing the error response itself when it cannot.
func decodeHandoutRequest(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxHandoutBytes+(1<<20))).Decode(into); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body"))
		return false
	}
	return true
}

func (s *Server) handleCreateHandout(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("cid"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Kind  string `json:"kind"`
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if !decodeHandoutRequest(w, r, &req) {
		return
	}
	if int64(len(req.Body)) > maxHandoutBytes {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Errorf("a handout exceeds %d MB", maxHandoutBytes>>20))
		return
	}
	h, err := s.knowledge.CreateHandout(r.Context(), a.campaign.ID, knowledge.HandoutInput{
		Kind: req.Kind, Title: req.Title, Body: req.Body, CreatedBy: userID(r),
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"handout": toHandoutView(a.campaign.ID, h)})
}

func (s *Server) handleUpdateHandout(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("cid"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	var req struct {
		Kind  *string `json:"kind"`
		Title *string `json:"title"`
		Body  *string `json:"body"`
	}
	if !decodeHandoutRequest(w, r, &req) {
		return
	}
	if req.Body != nil && int64(len(*req.Body)) > maxHandoutBytes {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Errorf("a handout exceeds %d MB", maxHandoutBytes>>20))
		return
	}
	h, err := s.knowledge.UpdateHandout(r.Context(), a.campaign.ID, r.PathValue("hid"), knowledge.HandoutUpdate{
		Kind: req.Kind, Title: req.Title, Body: req.Body,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"handout": toHandoutView(a.campaign.ID, h)})
}

func (s *Server) handleDeleteHandout(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("cid"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	// The file goes only after the row does — a failed delete leaves the
	// image orphaned at worst, never a row pointing at a removed file.
	current, err := s.knowledge.Handout(r.Context(), knowledge.ScopeDM, a.campaign.ID, r.PathValue("hid"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.knowledge.DeleteHandout(r.Context(), a.campaign.ID, r.PathValue("hid")); err != nil {
		writeStoreError(w, err)
		return
	}
	s.removeHandoutImage(current.ImageRef)
	w.WriteHeader(http.StatusNoContent)
}

// setHandoutStatus is the lifecycle's common spine: resolve, gate, move,
// report.
func (s *Server) setHandoutStatus(w http.ResponseWriter, r *http.Request, status string) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("cid"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	h, err := s.knowledge.SetHandoutStatus(r.Context(), a.campaign.ID, r.PathValue("hid"), status)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"handout": toHandoutView(a.campaign.ID, h)})
}

func (s *Server) handlePublishHandout(w http.ResponseWriter, r *http.Request) {
	s.setHandoutStatus(w, r, campaign.HandoutStatusPublished)
}

func (s *Server) handleUnpublishHandout(w http.ResponseWriter, r *http.Request) {
	s.setHandoutStatus(w, r, campaign.HandoutStatusDraft)
}

func (s *Server) handleRetireHandout(w http.ResponseWriter, r *http.Request) {
	s.setHandoutStatus(w, r, campaign.HandoutStatusRetired)
}

/* ---------- the image path ---------- */

// handoutImageExts maps the sniffed types the upload accepts onto the
// extension the stored reference carries. The list is deliberately short:
// what a browser renders natively and a DM actually has on disk.
var handoutImageExts = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// handlePutHandoutImage uploads a handout's image: DM only, multipart,
// sniffed before it is stored — only a real png/jpeg/gif/webp lands, and
// the row's reference is a server-generated name, so no client input
// reaches a path. Replacing an image removes the file it replaced.
func (s *Server) handlePutHandoutImage(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("cid"))
	if a == nil {
		return
	}
	if !a.requireDM(w) {
		return
	}
	if s.handoutOpts.Dir == "" {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("handout images are not configured (set HANDOUTS_DIR)"))
		return
	}
	current, err := s.knowledge.Handout(r.Context(), knowledge.ScopeDM, a.campaign.ID, r.PathValue("hid"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.handoutOpts.MaxImageBytes+(2<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Errorf("image too large (max %d MB)", s.handoutOpts.MaxImageBytes>>20))
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("file field is required"))
		return
	}
	defer file.Close()

	// Sniff the head, then stream the whole upload (head included) to a
	// generated name under the cap.
	head := make([]byte, 512)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("read image: %v", err))
		return
	}
	mime := http.DetectContentType(head[:n])
	ext, ok := handoutImageExts[mime]
	if !ok {
		writeError(w, http.StatusUnsupportedMediaType, fmt.Errorf("an image must be png, jpeg, gif or webp — this looks like %s", mime))
		return
	}
	if err := os.MkdirAll(s.handoutOpts.Dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("create handout dir"))
		return
	}
	ref := uuid.NewString() + ext
	path := filepath.Join(s.handoutOpts.Dir, ref)
	dst, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("store image"))
		return
	}
	size, err := io.Copy(dst, io.MultiReader(bytes.NewReader(head[:n]), io.LimitReader(file, s.handoutOpts.MaxImageBytes)))
	closeErr := dst.Close()
	if err != nil || closeErr != nil {
		_ = os.Remove(path)
		writeError(w, http.StatusInternalServerError, fmt.Errorf("store image"))
		return
	}
	if size == s.handoutOpts.MaxImageBytes && !atEOF(file) {
		_ = os.Remove(path)
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Errorf("image exceeds the %d MB handout cap (HANDOUT_MAX_IMAGE_MB)", s.handoutOpts.MaxImageBytes>>20))
		return
	}
	h, err := s.knowledge.SetHandoutImage(r.Context(), a.campaign.ID, r.PathValue("hid"), ref, mime, size)
	if err != nil {
		_ = os.Remove(path)
		writeStoreError(w, err)
		return
	}
	if current.ImageRef != "" && current.ImageRef != ref {
		s.removeHandoutImage(current.ImageRef)
	}
	writeJSON(w, http.StatusOK, map[string]any{"handout": toHandoutView(a.campaign.ID, h)})
}

// atEOF reports whether the reader is exhausted — the companion check to
// a copy that stopped exactly at the cap, so an image one byte over is
// still refused.
func atEOF(r io.Reader) bool {
	var one [1]byte
	_, err := r.Read(one[:])
	return errors.Is(err, io.EOF)
}

// handleGetHandoutImage serves a handout's image at the read's own scope:
// the handout resolves first (published for a member, any status for the
// DM), and only then does a byte move — a draft's image is as absent as
// its row.
func (s *Server) handleGetHandoutImage(w http.ResponseWriter, r *http.Request) {
	a := s.resolveCampaignAccess(w, r, r.PathValue("cid"))
	if a == nil {
		return
	}
	var h *campaign.Handout
	var err error
	if a.isDM() {
		h, err = s.knowledge.Handout(r.Context(), knowledge.ScopeDM, a.campaign.ID, r.PathValue("hid"))
	} else {
		h, err = a.view.Handout(r.Context(), a.campaign.ID, r.PathValue("hid"))
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if h.ImageRef == "" || s.handoutOpts.Dir == "" {
		writeError(w, http.StatusNotFound, fmt.Errorf("handout %s has no image", r.PathValue("hid")))
		return
	}
	// The reference is server-generated (uuid + fixed extension) and the
	// row is campaign-scoped; the join stays inside one directory.
	w.Header().Set("Content-Type", h.ImageMime)
	http.ServeFile(w, r, filepath.Join(s.handoutOpts.Dir, h.ImageRef))
}

// removeHandoutImage drops a stored image file, best-effort: a missing
// file is the operator's cleanup problem, not a failed request.
func (s *Server) removeHandoutImage(ref string) {
	if ref == "" || s.handoutOpts.Dir == "" {
		return
	}
	_ = os.Remove(filepath.Join(s.handoutOpts.Dir, ref))
}
