package knowledge

/*
Handouts and maps (MAD-490, stage 4 of MAD-319): the material the DM hands
to the party. The schema is owned by migration 0039; the shape is
campaign.Handout.

The one rule this file exists to enforce, same as every other scoped read
here (ADR 2): status is authorization, not filtering-after-the-fact. A
non-DM scope's SELECT carries `status = 'published'` in the WHERE clause,
so a draft or retired row is as unreachable as a secret fact the awareness
gate refuses — PlayerView.Handouts wraps exactly these reads, which is why
a draft is invisible to a player by construction rather than by the
handler's good behaviour.

The lifecycle is the DM's alone: create, edit, publish, unpublish, retire
are DM-gated at the handler (the store validates shape, not perspective,
like every write here). Retiring keeps the row — the DM's history of what
the party was once handed — while unpublishing returns a handout to draft
and clears the published-at stamp, so a re-publish re-stamps it.
*/

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/madeofpendletonwool/grimoire/internal/campaign"
)

/* ---------- vocabulary ---------- */

// validHandoutKind / validHandoutStatus validate the two controlled
// vocabularies migration 0039's CHECKs constrain. Duplicated here so a bad
// value surfaces as ErrInvalid before it surfaces as a constraint error.
var (
	validHandoutKind = map[string]bool{
		campaign.HandoutKindHandout: true, campaign.HandoutKindMap: true,
	}
	validHandoutStatus = map[string]bool{
		campaign.HandoutStatusDraft: true, campaign.HandoutStatusPublished: true, campaign.HandoutStatusRetired: true,
	}
)

/* ---------- the stored shape ---------- */

// handoutCols is the read; there is no DM-only column to split the way a
// rumour splits its truth; the row set itself is what the scope decides.
const handoutCols = `id, campaign_id, kind, title, body, image_ref, image_mime, image_bytes,
                     status, created_by, created_at, published_at, updated_at`

func scanHandout(row interface{ Scan(...any) error }) (*campaign.Handout, error) {
	var (
		h         campaign.Handout
		imageRef  sql.NullString
		published sql.NullInt64
		creation  int64
		updated   int64
	)
	if err := row.Scan(&h.ID, &h.CampaignID, &h.Kind, &h.Title, &h.Body, &imageRef,
		&h.ImageMime, &h.ImageBytes, &h.Status, &h.CreatedBy, &creation, &published, &updated); err != nil {
		return nil, err
	}
	h.ImageRef = imageRef.String
	h.CreatedAt = time.UnixMilli(creation).UTC()
	h.UpdatedAt = time.UnixMilli(updated).UTC()
	if published.Valid && published.Int64 > 0 {
		h.PublishedAt = time.UnixMilli(published.Int64).UTC()
	}
	return &h, nil
}

/* ---------- writes (the DM paths) ---------- */

// HandoutInput is one DM-authored handout. Kind is required; title is
// required (trim first); body is markdown and may be empty for a pure
// image map. Image fields are set by the image upload path, not the JSON
// create — the row starts text-shaped and gains its image when the DM
// uploads one.
type HandoutInput struct {
	Kind      string
	Title     string
	Body      string
	CreatedBy string
}

// CreateHandout writes one handout in draft. DM-gated at the handler.
func (s *Store) CreateHandout(ctx context.Context, campaignID string, in HandoutInput) (*campaign.Handout, error) {
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		return nil, fmt.Errorf("%w: a handout needs a title", ErrInvalid)
	}
	if !validHandoutKind[in.Kind] {
		return nil, fmt.Errorf("%w: kind %q", ErrInvalid, in.Kind)
	}
	now := time.Now().UTC()
	h := &campaign.Handout{
		ID: uuid.NewString(), CampaignID: campaignID, Kind: in.Kind, Title: in.Title,
		Body: in.Body, Status: campaign.HandoutStatusDraft, CreatedBy: in.CreatedBy,
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO handouts (id, campaign_id, kind, title, body, status, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.ID, h.CampaignID, h.Kind, h.Title, h.Body, h.Status, h.CreatedBy,
		h.CreatedAt.UnixMilli(), h.UpdatedAt.UnixMilli()); err != nil {
		return nil, fmt.Errorf("insert handout: %w", err)
	}
	return h, nil
}

// HandoutUpdate patches one handout. nil leaves a field alone.
type HandoutUpdate struct {
	Kind  *string
	Title *string
	Body  *string
}

// UpdateHandout applies a patch and returns the handout as it stands.
func (s *Store) UpdateHandout(ctx context.Context, campaignID, handoutID string, up HandoutUpdate) (*campaign.Handout, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("handout tx: %w", err)
	}
	defer tx.Rollback()
	current, err := handoutInCampaignTx(ctx, tx, handoutID, campaignID)
	if err != nil {
		return nil, err
	}
	if up.Kind != nil {
		if !validHandoutKind[*up.Kind] {
			return nil, fmt.Errorf("%w: kind %q", ErrInvalid, *up.Kind)
		}
		current.Kind = *up.Kind
	}
	if up.Title != nil {
		v := strings.TrimSpace(*up.Title)
		if v == "" {
			return nil, fmt.Errorf("%w: a handout needs a title", ErrInvalid)
		}
		current.Title = v
	}
	if up.Body != nil {
		current.Body = *up.Body
	}
	// A kind change can invalidate what publish requires (a map without
	// its image); publishing re-checks, so an edit only re-checks shape.
	if err := validateHandoutPublishable(current); err != nil {
		return nil, err
	}
	current.UpdatedAt = time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
		UPDATE handouts SET kind = ?, title = ?, body = ?, updated_at = ?
		 WHERE id = ? AND campaign_id = ?`,
		current.Kind, current.Title, current.Body, current.UpdatedAt.UnixMilli(),
		handoutID, campaignID); err != nil {
		return nil, fmt.Errorf("update handout: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("handout commit: %w", err)
	}
	return current, nil
}

// SetHandoutImage records an uploaded image on a handout: the reference
// (a generated file name inside the handouts directory), the sniffed MIME
// type and the size. The caller owns the file; the store owns the row.
func (s *Store) SetHandoutImage(ctx context.Context, campaignID, handoutID, ref, mime string, size int64) (*campaign.Handout, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("handout tx: %w", err)
	}
	defer tx.Rollback()
	current, err := handoutInCampaignTx(ctx, tx, handoutID, campaignID)
	if err != nil {
		return nil, err
	}
	current.ImageRef, current.ImageMime, current.ImageBytes = ref, mime, size
	current.UpdatedAt = time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
		UPDATE handouts SET image_ref = ?, image_mime = ?, image_bytes = ?, updated_at = ?
		 WHERE id = ? AND campaign_id = ?`,
		current.ImageRef, current.ImageMime, current.ImageBytes, current.UpdatedAt.UnixMilli(),
		handoutID, campaignID); err != nil {
		return nil, fmt.Errorf("set handout image: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("handout commit: %w", err)
	}
	return current, nil
}

// DeleteHandout removes a handout. The row and its history go together —
// unlike retirement this is the "never handed out, never will be" case,
// and the caller removes the image file beside it.
func (s *Store) DeleteHandout(ctx context.Context, campaignID, handoutID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM handouts WHERE id = ? AND campaign_id = ?`, handoutID, campaignID)
	if err != nil {
		return fmt.Errorf("delete handout: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: handout %s", ErrNotFound, handoutID)
	}
	return nil
}

// SetHandoutStatus moves a handout through its lifecycle:
//
//   - publish: draft (or retired, brought back) becomes published, the
//     hand-out is stamped now. Publishing requires the handout to have
//     something to show — a body or an image; a map specifically requires
//     its image.
//   - unpublish: published returns to draft, the stamp clears. What the
//     party holds they hold; the portal simply stops offering it.
//   - retire: any state becomes retired — the row stays for history and
//     no player read can address it again.
//
// Transitions the vocabulary cannot express are refused before the write.
func (s *Store) SetHandoutStatus(ctx context.Context, campaignID, handoutID, status string) (*campaign.Handout, error) {
	if !validHandoutStatus[status] {
		return nil, fmt.Errorf("%w: status %q", ErrInvalid, status)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("handout tx: %w", err)
	}
	defer tx.Rollback()
	current, err := handoutInCampaignTx(ctx, tx, handoutID, campaignID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	switch status {
	case campaign.HandoutStatusPublished:
		if err := validateHandoutPublishable(current); err != nil {
			return nil, err
		}
		current.Status = status
		current.PublishedAt = now
	case campaign.HandoutStatusDraft:
		current.Status = status
		current.PublishedAt = time.Time{}
	case campaign.HandoutStatusRetired:
		current.Status = status
		current.PublishedAt = time.Time{}
	}
	current.UpdatedAt = now
	if _, err := tx.ExecContext(ctx, `
		UPDATE handouts SET status = ?, published_at = ?, updated_at = ?
		 WHERE id = ? AND campaign_id = ?`,
		current.Status, nullTime(current.PublishedAt), current.UpdatedAt.UnixMilli(),
		handoutID, campaignID); err != nil {
		return nil, fmt.Errorf("set handout status: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("handout commit: %w", err)
	}
	return current, nil
}

// validateHandoutPublishable refuses the publish a DM would regret: a map
// without its image, and any handout with neither body nor image. The
// edit path re-checks so a kind change cannot leave a published shape
// invalid underneath a status that says otherwise.
func validateHandoutPublishable(h *campaign.Handout) error {
	if h.Kind == campaign.HandoutKindMap && h.ImageRef == "" {
		return fmt.Errorf("%w: a map needs its image before the party can see it", ErrInvalid)
	}
	if strings.TrimSpace(h.Body) == "" && h.ImageRef == "" {
		return fmt.Errorf("%w: a handout needs its words or an image before the party can see it", ErrInvalid)
	}
	return nil
}

/* ---------- scoped reads ---------- */

// HandoutFilter narrows Handouts. Zero values mean "no restriction".
// Status is a DM-only filter and is refused at non-DM scopes — a player
// asking for "the drafts" is the question the scope line exists to refuse.
type HandoutFilter struct {
	Kind   string
	Status string
}

// Handouts lists the handouts a scope may read, newest hand-out first for
// the party's material and newest created first for the DM's drafts. The
// DM reads every row in every status; every other scope reads published
// rows only — in the SQL, never after it.
func (s *Store) Handouts(ctx context.Context, scope Scope, campaignID string, filter HandoutFilter) ([]campaign.Handout, error) {
	if err := s.resolveScope(ctx, scope, campaignID); err != nil {
		return nil, err
	}
	if filter.Kind != "" && !validHandoutKind[filter.Kind] {
		return nil, fmt.Errorf("%w: kind filter %q", ErrInvalid, filter.Kind)
	}
	if filter.Status != "" {
		if !validHandoutStatus[filter.Status] {
			return nil, fmt.Errorf("%w: status filter %q", ErrInvalid, filter.Status)
		}
		if !scope.IsDM() {
			return nil, fmt.Errorf("%w: status filtering is the dm's question; a player scope cannot ask it", ErrScope)
		}
	}
	q := ` FROM handouts h WHERE h.campaign_id = ?`
	args := []any{campaignID}
	if !scope.IsDM() {
		q += ` AND h.status = 'published'`
	}
	if filter.Kind != "" {
		q += ` AND h.kind = ?`
		args = append(args, filter.Kind)
	}
	if filter.Status != "" {
		q += ` AND h.status = ?`
		args = append(args, filter.Status)
	}
	if scope.IsDM() {
		q += ` ORDER BY h.created_at DESC, h.id`
	} else {
		q += ` ORDER BY COALESCE(h.published_at, h.created_at) DESC, h.id`
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+handoutCols+q, args...)
	if err != nil {
		return nil, fmt.Errorf("scoped handouts: %w", err)
	}
	defer rows.Close()
	var out []campaign.Handout
	for rows.Next() {
		h, err := scanHandout(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *h)
	}
	return out, rows.Err()
}

// Handout returns one handout if the scope may read it. A draft or retired
// row at a non-DM scope is indistinguishable from a missing one.
func (s *Store) Handout(ctx context.Context, scope Scope, campaignID, handoutID string) (*campaign.Handout, error) {
	if err := s.resolveScope(ctx, scope, campaignID); err != nil {
		return nil, err
	}
	q := ` SELECT ` + handoutCols + ` FROM handouts h WHERE h.campaign_id = ? AND h.id = ?`
	args := []any{campaignID, handoutID}
	if !scope.IsDM() {
		q += ` AND h.status = 'published'`
	}
	h, err := scanHandout(s.db.QueryRowContext(ctx, q, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: handout %s", ErrNotFound, handoutID)
	}
	return h, err
}

/* ---------- tx helpers ---------- */

// nullTime renders a zero time as SQL NULL, any other as epoch millis.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

// handoutInCampaignTx loads one handout inside a tx; ErrNotFound when it
// is missing or belongs to another campaign.
func handoutInCampaignTx(ctx context.Context, tx *sql.Tx, id, campaignID string) (*campaign.Handout, error) {
	h, err := scanHandout(tx.QueryRowContext(ctx,
		`SELECT `+handoutCols+` FROM handouts WHERE id = ? AND campaign_id = ?`, id, campaignID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: handout %s", ErrNotFound, id)
	}
	return h, err
}
