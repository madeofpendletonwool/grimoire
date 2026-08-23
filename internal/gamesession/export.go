// Markdown export of a whole session log: the DM's take-home record of what
// was played, from what material, and what was ruled.

package gamesession

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ExportMarkdown renders a session — header, sources with their verbatim
// content, and the event log in play order — as one Markdown document.
func (s *Store) ExportMarkdown(ctx context.Context, sessionID string) (string, error) {
	ses, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return "", err
	}
	campaignName := ""
	if err := s.db.QueryRowContext(ctx,
		`SELECT name FROM campaigns WHERE id = ?`, ses.Campaign).Scan(&campaignName); err != nil {
		return "", fmt.Errorf("campaign name: %w", err)
	}
	sources, err := s.ListSources(ctx, sessionID, true)
	if err != nil {
		return "", err
	}
	events, err := s.ListEvents(ctx, sessionID)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %s — Session %d: %s\n\n", campaignName, ses.Ordinal, ses.Name)
	fmt.Fprintf(&b, "- **Status:** %s\n", ses.Status)
	fmt.Fprintf(&b, "- **Started:** %s\n", msOrUnknown(ses.StartedAt))
	fmt.Fprintf(&b, "- **Ended:** %s\n", msOrUnknown(ses.EndedAt))
	fmt.Fprintf(&b, "- **Exported:** %s\n\n", time.Now().UTC().Format(time.RFC3339))

	b.WriteString("## Sources\n\n")
	if len(sources) == 0 {
		b.WriteString("_No sources recorded._\n\n")
	}
	for i := range sources {
		// The list projection carries no content (it exists to keep list
		// views cheap); an export is the one place every byte is wanted.
		full, err := s.GetSource(ctx, sources[i].ID)
		if err != nil {
			return "", err
		}
		src := full
		label := src.Title
		if label == "" {
			label = src.Kind
		}
		by := src.Author
		if by == "" {
			by = "unknown"
		}
		fmt.Fprintf(&b, "### %s\n\n", label)
		fmt.Fprintf(&b, "_%s_, recorded by %s — %d bytes, sha256 `%s`\n\n",
			src.Kind, by, src.ByteSize, src.Checksum)
		if len(src.Timing) > 0 {
			first, last := src.Timing[0], src.Timing[len(src.Timing)-1]
			fmt.Fprintf(&b, "_Timed source: %d cues, %.1f–%.1f minutes._\n\n",
				len(src.Timing), float64(first.StartMS)/60000, float64(last.EndMS)/60000)
		}
		// Fenced verbatim; widen the fence if the content contains one so
		// the export can never break out of its own block.
		fence := strings.Repeat("`", fenceLen(src.Content)+1)
		b.WriteString(fence + "\n")
		b.WriteString(src.Content)
		if !strings.HasSuffix(src.Content, "\n") {
			b.WriteString("\n")
		}
		b.WriteString(fence + "\n\n")
	}

	b.WriteString("## Log\n\n")
	if len(events) == 0 {
		b.WriteString("_No events recorded._\n\n")
	}
	for i := range events {
		ev := &events[i]
		head := ev.Summary
		if head == "" {
			head = strings.ToUpper(ev.Kind[:1]) + ev.Kind[1:]
		}
		fmt.Fprintf(&b, "**#%d %s** — %s\n\n", ev.Seq, ev.Kind, head)
		if ev.Detail != "" {
			for _, line := range strings.Split(strings.TrimRight(ev.Detail, "\n"), "\n") {
				fmt.Fprintf(&b, "> %s\n", line)
			}
			b.WriteString("\n")
		}
		for _, k := range payloadKeys(ev.Payload) {
			fmt.Fprintf(&b, "- %s: %v\n", k, ev.Payload[k])
		}
		if len(payloadKeys(ev.Payload)) > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "_%s_\n\n", ev.CreatedAt.Format("15:04:05 MST"))
	}
	return strings.TrimRight(b.String(), "\n") + "\n", nil
}

// msOrUnknown renders a session timestamp, or "unknown" while it is unset.
func msOrUnknown(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Format(time.RFC3339)
}

// fenceLen returns the longest backtick run in content, so the fence around
// it can always be one longer.
func fenceLen(content string) int {
	longest, cur := 0, 0
	for _, r := range content {
		if r == '`' {
			cur++
			if cur > longest {
				longest = cur
			}
			continue
		}
		cur = 0
	}
	return longest
}

// payloadKeys renders payload keys in a stable order (they come back from
// the map in random order otherwise).
func payloadKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
