package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Write action names recorded in a plan's history.
const (
	ActionCreate  = "create"
	ActionRevise  = "revise"
	ActionAppend  = "append"
	ActionStatus  = "status"
	ActionRestore = "restore"
)

// MaxPlanBytes is the per-plan size cap (sum of all version files).
const MaxPlanBytes = 50 << 20 // 50 MB

// ErrNotFound is returned when a plan or version does not exist.
var ErrNotFound = errors.New("not found")

// ErrTooLarge is returned when a write would exceed the per-plan size cap.
var ErrTooLarge = errors.New("plan size cap exceeded")

// ErrLatestVersion is returned when a caller tries to delete a plan's latest
// version through the history-deletion API. The latest version is the plan;
// removing it is DELETE /api/plans/{slug} (which trashes the whole thing).
var ErrLatestVersion = errors.New("the latest version cannot be deleted")

// ErrConflict is returned by Revise on an optimistic-locking failure.
// It carries the current latest version so the caller can report it.
type ErrConflict struct {
	Current int
}

func (e ErrConflict) Error() string {
	return fmt.Sprintf("version conflict: current is %d", e.Current)
}

// VersionEntry is one row of a plan's history.
type VersionEntry struct {
	Version   int       `json:"version"`
	Timestamp time.Time `json:"timestamp"`
	Agent     string    `json:"agent,omitempty"`
	Action    string    `json:"action"`
	Note      string    `json:"note,omitempty"`
	// Forced marks a PUT that omitted base_version (see agent-loop §5).
	Forced bool `json:"forced,omitempty"`
}

// Plan is the per-plan metadata held in index.json.
type Plan struct {
	ID         string         `json:"id"`
	Slug       string         `json:"slug"`
	Title      string         `json:"title"`
	Created    time.Time      `json:"created"`
	Updated    time.Time      `json:"updated"`
	Latest     int            `json:"latest"`
	Agent      string         `json:"agent,omitempty"` // source agent (creator)
	Repo       string         `json:"repo,omitempty"`
	Branch     string         `json:"branch,omitempty"`
	Status     string         `json:"status,omitempty"`
	Meta       map[string]any `json:"meta,omitempty"` // parsed plan-meta block
	ShareToken *string        `json:"share_token,omitempty"`
	History    []VersionEntry `json:"history"`
	// TrashedAt is set only for entries in the trash section.
	TrashedAt *time.Time `json:"trashed_at,omitempty"`
}

// index is the serialized form of the whole store.
type index struct {
	Plans     map[string]*Plan  `json:"plans"`               // slug -> plan
	Trash     map[string]*Plan  `json:"trash"`               // slug -> plan
	Redirects map[string]string `json:"redirects,omitempty"` // old slug -> new slug
}

// Store is the versioned file store. It is safe for concurrent use.
type Store struct {
	dir string // data directory

	mu  sync.Mutex // guards idx and planLocks
	idx *index

	planLocks map[string]*sync.Mutex // per-slug write serialization

	// Events fans committed versions out to live viewers (SSE). Always
	// non-nil for a store built by NewStore; Publish tolerates nil anyway so
	// a zero-value Store in a test doesn't panic.
	Events *Hub
}

// WriteParams carries the optional write metadata for create/revise.
type WriteParams struct {
	Slug   string
	Title  string
	Agent  string
	Repo   string
	Branch string
	Note   string
}

// NewStore opens (or initializes) the store rooted at dir.
func NewStore(dir string) (*Store, error) {
	s := &Store{dir: dir, planLocks: map[string]*sync.Mutex{}, Events: NewHub()}
	for _, sub := range []string{"files", "trash"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) indexPath() string { return filepath.Join(s.dir, "index.json") }

func (s *Store) load() error {
	data, err := os.ReadFile(s.indexPath())
	if os.IsNotExist(err) {
		s.idx = &index{Plans: map[string]*Plan{}, Trash: map[string]*Plan{}, Redirects: map[string]string{}}
		return nil
	}
	if err != nil {
		return err
	}
	var idx index
	if err := json.Unmarshal(data, &idx); err != nil {
		return fmt.Errorf("parse index.json: %w", err)
	}
	if idx.Plans == nil {
		idx.Plans = map[string]*Plan{}
	}
	if idx.Trash == nil {
		idx.Trash = map[string]*Plan{}
	}
	if idx.Redirects == nil {
		idx.Redirects = map[string]string{}
	}
	s.idx = &idx
	return nil
}

// saveLocked persists the index. Callers must hold s.mu.
func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.idx, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.indexPath(), append(data, '\n'), 0o644)
}

// planLock returns the per-slug lock, creating it on first use.
func (s *Store) planLock(slug string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.planLocks[slug]
	if !ok {
		l = &sync.Mutex{}
		s.planLocks[slug] = l
	}
	return l
}

// ---- file helpers ----

func (s *Store) planDir(slug string) string { return filepath.Join(s.dir, "files", slug) }

func versionFile(n int) string { return fmt.Sprintf("v%04d.html", n) }

func (s *Store) versionPath(slug string, n int) string {
	return filepath.Join(s.planDir(slug), versionFile(n))
}

// planSizeOnDisk sums the byte size of a plan's version files.
func (s *Store) planSizeOnDisk(slug string) int64 {
	var total int64
	entries, err := os.ReadDir(s.planDir(slug))
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}

// writeVersionFile writes a version file (written once, never mutated).
func (s *Store) writeVersionFile(slug string, n int, htmlBody []byte) error {
	if err := os.MkdirAll(s.planDir(slug), 0o755); err != nil {
		return err
	}
	return atomicWrite(s.versionPath(slug, n), htmlBody, 0o644)
}

// ---- id / slug / token generation ----

func newID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newShareToken() string {
	b := make([]byte, 16) // 128 bits
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

var slugStrip = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify converts a title to a kebab-case slug.
func Slugify(title string) string {
	s := strings.ToLower(strings.TrimSpace(title))
	s = slugStrip.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "plan"
	}
	return s
}

// uniqueSlugLocked returns base, or base-2, base-3… avoiding live+trash+redirects.
// Callers must hold s.mu.
func (s *Store) uniqueSlugLocked(base string) string {
	taken := func(sl string) bool {
		if _, ok := s.idx.Plans[sl]; ok {
			return true
		}
		if _, ok := s.idx.Trash[sl]; ok {
			return true
		}
		if _, ok := s.idx.Redirects[sl]; ok {
			return true
		}
		return false
	}
	if !taken(base) {
		return base
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		if !taken(cand) {
			return cand
		}
	}
}

// clone returns a deep-ish copy of a plan safe to hand out without locks held.
func clone(p *Plan) *Plan {
	cp := *p
	cp.History = append([]VersionEntry(nil), p.History...)
	if p.Meta != nil {
		b, _ := json.Marshal(p.Meta)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		cp.Meta = m
	}
	if p.ShareToken != nil {
		t := *p.ShareToken
		cp.ShareToken = &t
	}
	return &cp
}

// ---- HTML parsing / rewriting ----

var (
	reMeta  = regexp.MustCompile(`(?is)<script[^>]*\bid=["']plan-meta["'][^>]*>(.*?)</script>`)
	reTitle = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

// extractMeta parses the plan-meta JSON block, if present and valid.
func extractMeta(body []byte) map[string]any {
	m := reMeta.FindSubmatch(body)
	if m == nil {
		return nil
	}
	var meta map[string]any
	if err := json.Unmarshal(m[1], &meta); err != nil {
		return nil
	}
	return meta
}

// extractTitle returns the trimmed <title> text, if any.
func extractTitle(body []byte) string {
	m := reTitle.FindSubmatch(body)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(string(m[1])))
}

// setMetaBlock rewrites (or inserts) the plan-meta script block with meta.
func setMetaBlock(body []byte, meta map[string]any) []byte {
	pretty, _ := json.MarshalIndent(meta, "", "  ")
	block := []byte(`<script type="application/json" id="plan-meta">` + "\n" + string(pretty) + "\n</script>")
	if reMeta.Match(body) {
		return reMeta.ReplaceAll(body, replaceLiteral(block))
	}
	// No existing block: insert before </main>, else </body>, else append.
	return insertBefore(body, block, "</main>", "</body>")
}

// replaceLiteral escapes $ so ReplaceAll treats block as a literal.
func replaceLiteral(block []byte) []byte {
	return []byte(strings.ReplaceAll(string(block), "$", "$$"))
}

// insertBefore inserts frag before the first of the given markers found
// (case-insensitive); if none match, frag is appended at the end.
func insertBefore(body, frag []byte, markers ...string) []byte {
	lower := strings.ToLower(string(body))
	for _, mk := range markers {
		if i := strings.Index(lower, mk); i >= 0 {
			out := make([]byte, 0, len(body)+len(frag)+1)
			out = append(out, body[:i]...)
			out = append(out, frag...)
			out = append(out, '\n')
			out = append(out, body[i:]...)
			return out
		}
	}
	return append(append(append([]byte{}, body...), '\n'), frag...)
}

// mergeMeta merges partial into current (deep for phases by id, LWW otherwise).
func mergeMeta(current, partial map[string]any) map[string]any {
	if current == nil {
		current = map[string]any{}
	}
	for k, v := range partial {
		if k == "phases" {
			current["phases"] = mergePhases(current["phases"], v)
			continue
		}
		current[k] = v
	}
	return current
}

// mergePhases merges phase objects by "id"; unmatched incoming phases append.
func mergePhases(currentAny, incomingAny any) any {
	toList := func(a any) []any {
		if l, ok := a.([]any); ok {
			return l
		}
		return nil
	}
	current := toList(currentAny)
	incoming := toList(incomingAny)
	if incoming == nil {
		return currentAny
	}
	idOf := func(p any) (float64, bool) {
		m, ok := p.(map[string]any)
		if !ok {
			return 0, false
		}
		id, ok := m["id"].(float64)
		return id, ok
	}
	for _, inc := range incoming {
		incMap, ok := inc.(map[string]any)
		if !ok {
			current = append(current, inc)
			continue
		}
		incID, hasID := idOf(inc)
		matched := false
		if hasID {
			for i, cur := range current {
				curID, curOK := idOf(cur)
				if curOK && curID == incID {
					curMap := cur.(map[string]any)
					for k, v := range incMap {
						curMap[k] = v
					}
					current[i] = curMap
					matched = true
					break
				}
			}
		}
		if !matched {
			current = append(current, incMap)
		}
	}
	return current
}

// ---- text view ----

var (
	reStyle   = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	reScript  = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	reTag     = regexp.MustCompile(`(?s)<[^>]+>`)
	reBlanks  = regexp.MustCompile(`[ \t]+`)
	reNewline = regexp.MustCompile(`\n{3,}`)
)

// ToText renders a style/script-stripped plain-text view (~half the tokens),
// keeping the plan-meta JSON content as readable text.
func ToText(body []byte) string {
	s := string(body)
	// Preserve plan-meta JSON as plain text before scripts are stripped.
	if m := reMeta.FindStringSubmatch(s); m != nil {
		s = reMeta.ReplaceAllString(s, "\n[plan-meta]\n"+strings.ReplaceAll(strings.TrimSpace(m[1]), "$", "$$")+"\n")
	}
	s = reStyle.ReplaceAllString(s, "")
	s = reScript.ReplaceAllString(s, "")
	// Turn block-ish tags into line breaks for readability.
	s = regexp.MustCompile(`(?i)</(title|p|div|h[1-6]|li|tr|td|th|caption|blockquote|pre|section|article|header|footer|main|nav|table|ul|ol)>`).ReplaceAllString(s, "\n")
	s = regexp.MustCompile(`(?i)<br\s*/?>`).ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = reBlanks.ReplaceAllString(s, " ")
	// Trim trailing spaces on each line.
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight(strings.TrimLeft(ln, " "), " ")
	}
	s = strings.Join(lines, "\n")
	s = reNewline.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s) + "\n"
}

// ---- promote meta into index fields ----

func metaString(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	if v, ok := meta[key].(string); ok {
		return v
	}
	return ""
}

// applyMeta updates a plan's parsed meta and promoted repo/branch/status.
// Explicit repo/branch params (non-empty) win over meta values.
func applyMeta(p *Plan, meta map[string]any, repoParam, branchParam string) {
	p.Meta = meta
	if repoParam != "" {
		p.Repo = repoParam
	} else if r := metaString(meta, "repo"); r != "" {
		p.Repo = r
	}
	if branchParam != "" {
		p.Branch = branchParam
	} else if b := metaString(meta, "branch"); b != "" {
		p.Branch = b
	}
	if st := metaString(meta, "status"); st != "" {
		p.Status = st
	}
}

// ---- write operations ----

// Create makes a new plan, or a new version if the slug already exists.
// Returns the plan (copy) and the new version number.
func (s *Store) Create(body []byte, p WriteParams) (*Plan, int, error) {
	// Resolve the target slug up front so we can take the right plan lock.
	slug := s.resolveCreateSlug(p, body)

	lock := s.planLock(slug)
	lock.Lock()
	defer lock.Unlock()

	s.mu.Lock()
	existing := s.idx.Plans[slug]
	s.mu.Unlock()

	if existing != nil {
		// New version of an existing plan (full replacement).
		return s.commitFullBody(slug, body, p, ActionRevise, false)
	}
	return s.createNew(slug, body, p)
}

// resolveCreateSlug decides the slug for a create call (explicit ?slug=,
// else from title/param, else from <title>, deduped for brand-new plans).
func (s *Store) resolveCreateSlug(p WriteParams, body []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Slug != "" {
		base := Slugify(p.Slug)
		if _, ok := s.idx.Plans[base]; ok {
			return base // append a version to the existing plan
		}
		return s.uniqueSlugLocked(base)
	}
	title := p.Title
	if title == "" {
		title = extractTitle(body)
	}
	base := Slugify(title)
	if _, ok := s.idx.Plans[base]; ok {
		return base
	}
	return s.uniqueSlugLocked(base)
}

// createNew writes v1 of a brand-new plan. Caller holds the plan lock.
func (s *Store) createNew(slug string, body []byte, p WriteParams) (*Plan, int, error) {
	if int64(len(body)) > MaxPlanBytes {
		return nil, 0, ErrTooLarge
	}
	if err := s.writeVersionFile(slug, 1, body); err != nil {
		return nil, 0, err
	}
	now := time.Now().UTC()
	title := p.Title
	if title == "" {
		title = extractTitle(body)
	}
	if title == "" {
		title = slug
	}
	pl := &Plan{
		ID:      newID(),
		Slug:    slug,
		Title:   title,
		Created: now,
		Updated: now,
		Latest:  1,
		Agent:   p.Agent,
		History: []VersionEntry{{Version: 1, Timestamp: now, Agent: p.Agent, Action: ActionCreate, Note: p.Note}},
	}
	applyMeta(pl, extractMeta(body), p.Repo, p.Branch)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.idx.Plans[slug] = pl
	if err := s.saveLocked(); err != nil {
		return nil, 0, err
	}
	return clone(pl), 1, nil
}

// commitFullBody writes a new version that fully replaces the document.
// Used by POST-on-existing (revise) and PUT. Caller holds the plan lock.
func (s *Store) commitFullBody(slug string, body []byte, p WriteParams, action string, forced bool) (*Plan, int, error) {
	pl, n, err := s.commitFullBodyInner(slug, body, p, action, forced)
	if err == nil {
		s.Events.Publish(slug, n)
	}
	return pl, n, err
}

func (s *Store) commitFullBodyInner(slug string, body []byte, p WriteParams, action string, forced bool) (*Plan, int, error) {
	if s.planSizeOnDisk(slug)+int64(len(body)) > MaxPlanBytes {
		return nil, 0, ErrTooLarge
	}
	s.mu.Lock()
	pl := s.idx.Plans[slug]
	s.mu.Unlock()
	if pl == nil {
		return nil, 0, ErrNotFound
	}
	// Monotonic by construction: derived from Latest, never from len(History).
	// History can be sparse (versions are deletable), so a length-based next
	// would reuse a number that was already handed out.
	next := pl.Latest + 1
	if err := s.writeVersionFile(slug, next, body); err != nil {
		return nil, 0, err
	}
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	pl.Latest = next
	pl.Updated = now
	if p.Title != "" {
		pl.Title = p.Title
	} else if t := extractTitle(body); t != "" {
		pl.Title = t
	}
	applyMeta(pl, extractMeta(body), p.Repo, p.Branch)
	pl.History = append(pl.History, VersionEntry{
		Version: next, Timestamp: now, Agent: p.Agent, Action: action, Note: p.Note, Forced: forced,
	})
	if err := s.saveLocked(); err != nil {
		return nil, 0, err
	}
	return clone(pl), next, nil
}

// Revise performs a full revision (PUT) with optimistic locking.
// If baseVersion >= 0 and differs from latest, it returns ErrConflict.
// If baseVersion < 0 (omitted), the write is accepted and flagged forced.
func (s *Store) Revise(slug string, body []byte, baseVersion int, p WriteParams) (*Plan, int, error) {
	lock := s.planLock(slug)
	lock.Lock()
	defer lock.Unlock()

	s.mu.Lock()
	pl := s.idx.Plans[slug]
	var latest int
	if pl != nil {
		latest = pl.Latest
	}
	s.mu.Unlock()
	if pl == nil {
		return nil, 0, ErrNotFound
	}
	forced := baseVersion < 0
	if !forced && baseVersion != latest {
		return nil, 0, ErrConflict{Current: latest}
	}
	return s.commitFullBody(slug, body, p, ActionRevise, forced)
}

// Append inserts an HTML fragment before </main> (fallbacks: </body>, end)
// and snapshots a new version. Never conflicts (serialized per plan).
func (s *Store) Append(slug string, fragment []byte, p WriteParams) (*Plan, int, error) {
	lock := s.planLock(slug)
	lock.Lock()
	defer lock.Unlock()

	cur, err := s.readLatest(slug)
	if err != nil {
		return nil, 0, err
	}
	body := insertBefore(cur, fragment, "</main>", "</body>")
	return s.commitDerived(slug, body, p, ActionAppend)
}

// Status merges a partial meta update into the plan-meta block and index,
// snapshots a new version. Never conflicts.
func (s *Store) Status(slug string, partial map[string]any, p WriteParams) (*Plan, int, error) {
	lock := s.planLock(slug)
	lock.Lock()
	defer lock.Unlock()

	cur, err := s.readLatest(slug)
	if err != nil {
		return nil, 0, err
	}
	s.mu.Lock()
	pl := s.idx.Plans[slug]
	var curMeta map[string]any
	if pl != nil && pl.Meta != nil {
		b, _ := json.Marshal(pl.Meta)
		_ = json.Unmarshal(b, &curMeta)
	}
	s.mu.Unlock()
	if pl == nil {
		return nil, 0, ErrNotFound
	}
	if curMeta == nil {
		curMeta = extractMeta(cur)
	}
	merged := mergeMeta(curMeta, partial)
	body := setMetaBlock(cur, merged)
	return s.commitDerived(slug, body, p, ActionStatus)
}

// Restore copies version n forward as a new latest version.
func (s *Store) Restore(slug string, n int, p WriteParams) (*Plan, int, error) {
	lock := s.planLock(slug)
	lock.Lock()
	defer lock.Unlock()

	body, err := s.readVersion(slug, n)
	if err != nil {
		return nil, 0, err
	}
	if p.Note == "" {
		p.Note = fmt.Sprintf("restore v%d", n)
	}
	return s.commitDerived(slug, body, p, ActionRestore)
}

// commitDerived writes a new version derived from prior content (append/
// status/restore), updating index meta from the resulting body.
// Caller holds the plan lock.
func (s *Store) commitDerived(slug string, body []byte, p WriteParams, action string) (*Plan, int, error) {
	pl, n, err := s.commitDerivedInner(slug, body, p, action)
	if err == nil {
		s.Events.Publish(slug, n)
	}
	return pl, n, err
}

func (s *Store) commitDerivedInner(slug string, body []byte, p WriteParams, action string) (*Plan, int, error) {
	if s.planSizeOnDisk(slug)+int64(len(body)) > MaxPlanBytes {
		return nil, 0, ErrTooLarge
	}
	s.mu.Lock()
	pl := s.idx.Plans[slug]
	s.mu.Unlock()
	if pl == nil {
		return nil, 0, ErrNotFound
	}
	// Monotonic by construction: derived from Latest, never from len(History).
	// History can be sparse (versions are deletable), so a length-based next
	// would reuse a number that was already handed out.
	next := pl.Latest + 1
	if err := s.writeVersionFile(slug, next, body); err != nil {
		return nil, 0, err
	}
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	pl.Latest = next
	pl.Updated = now
	if t := extractTitle(body); t != "" {
		pl.Title = t
	}
	applyMeta(pl, extractMeta(body), "", "")
	pl.History = append(pl.History, VersionEntry{
		Version: next, Timestamp: now, Agent: p.Agent, Action: action, Note: p.Note,
	})
	if err := s.saveLocked(); err != nil {
		return nil, 0, err
	}
	return clone(pl), next, nil
}

// ---- history deletion ----
//
// History deletion is a hard delete: the vNNNN.html blob is unlinked and the
// VersionEntry drops out of the index. There is no second trash tier for it.
//
// Version numbers are never renumbered or reused. History goes sparse
// (v1, v4, v5) and every reader must tolerate the gaps. The next version
// number is always derived from Plan.Latest (see commitFullBodyInner /
// commitDerivedInner), never from len(History) — deleting v2 out of v1,v2,v3
// therefore still yields v4 on the next write.

// versionIndex returns the position of version n in h, or -1.
func versionIndex(h []VersionEntry, n int) int {
	for i, e := range h {
		if e.Version == n {
			return i
		}
	}
	return -1
}

// removeVersionFile unlinks a version blob. A blob that is already gone is
// not an error: the index entry must still be removable.
func (s *Store) removeVersionFile(slug string, n int) error {
	if err := os.Remove(s.versionPath(slug, n)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// DeleteVersion hard-deletes a single historical version. The latest version
// is refused with ErrLatestVersion; an unknown version with ErrNotFound.
// Returns the updated plan (copy).
func (s *Store) DeleteVersion(slug string, n int) (*Plan, error) {
	lock := s.planLock(slug)
	lock.Lock()
	defer lock.Unlock()

	pl, err := s.deleteVersionLocked(slug, n)
	if err != nil {
		return nil, err
	}
	s.Events.Publish(slug, pl.Latest)
	return pl, nil
}

func (s *Store) deleteVersionLocked(slug string, n int) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pl := s.idx.Plans[slug]
	if pl == nil {
		return nil, ErrNotFound
	}
	if n == pl.Latest {
		return nil, ErrLatestVersion
	}
	i := versionIndex(pl.History, n)
	if i < 0 {
		return nil, ErrNotFound
	}
	if err := s.removeVersionFile(slug, n); err != nil {
		return nil, err
	}
	pl.History = append(pl.History[:i:i], pl.History[i+1:]...)
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return clone(pl), nil
}

// PruneHistory hard-deletes all but the keep most recent versions of a plan.
// keep < 1 is treated as 1 (keep only the latest). The latest version is
// always retained, whatever the history says. Returns the updated plan (copy)
// and the version numbers that were removed, ascending.
func (s *Store) PruneHistory(slug string, keep int) (*Plan, []int, error) {
	if keep < 1 {
		keep = 1
	}
	lock := s.planLock(slug)
	lock.Lock()
	defer lock.Unlock()

	pl, removed, err := s.pruneHistoryLocked(slug, keep)
	if err != nil {
		return nil, nil, err
	}
	s.Events.Publish(slug, pl.Latest)
	return pl, removed, nil
}

func (s *Store) pruneHistoryLocked(slug string, keep int) (*Plan, []int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pl := s.idx.Plans[slug]
	if pl == nil {
		return nil, nil, ErrNotFound
	}

	// Decide what survives by version number, newest first — history may
	// already be sparse, so "the keep most recent" is a sort, not a slice.
	nums := make([]int, 0, len(pl.History))
	for _, e := range pl.History {
		nums = append(nums, e.Version)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(nums)))
	survives := map[int]bool{pl.Latest: true} // never deletable
	for i := 0; i < len(nums) && i < keep; i++ {
		survives[nums[i]] = true
	}

	var removed []int
	kept := make([]VersionEntry, 0, len(pl.History))
	for _, e := range pl.History {
		if survives[e.Version] {
			kept = append(kept, e)
			continue
		}
		if err := s.removeVersionFile(slug, e.Version); err != nil {
			return nil, nil, err
		}
		removed = append(removed, e.Version)
	}
	if len(removed) == 0 {
		return clone(pl), nil, nil
	}
	sort.Ints(removed)
	pl.History = kept
	if err := s.saveLocked(); err != nil {
		return nil, nil, err
	}
	return clone(pl), removed, nil
}

// ---- read operations ----

func (s *Store) readVersion(slug string, n int) ([]byte, error) {
	body, err := os.ReadFile(s.versionPath(slug, n))
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	return body, err
}

func (s *Store) readLatest(slug string) ([]byte, error) {
	s.mu.Lock()
	pl := s.idx.Plans[slug]
	s.mu.Unlock()
	if pl == nil {
		return nil, ErrNotFound
	}
	return s.readVersion(slug, pl.Latest)
}

// GetVersion returns a plan (copy) and the HTML for version n (0 = latest).
func (s *Store) GetVersion(slug string, n int) (*Plan, []byte, error) {
	s.mu.Lock()
	pl := s.idx.Plans[slug]
	if pl != nil {
		pl = clone(pl)
	}
	s.mu.Unlock()
	if pl == nil {
		return nil, nil, ErrNotFound
	}
	if n <= 0 {
		n = pl.Latest
	}
	body, err := s.readVersion(slug, n)
	if err != nil {
		return nil, nil, err
	}
	return pl, body, nil
}

// GetPlan returns a plan's metadata by slug (copy).
func (s *Store) GetPlan(slug string) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pl := s.idx.Plans[slug]
	if pl == nil {
		return nil, ErrNotFound
	}
	return clone(pl), nil
}

// Redirect returns the new slug an old slug points to, if any.
func (s *Store) Redirect(oldSlug string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ns, ok := s.idx.Redirects[oldSlug]
	return ns, ok
}

// ListFilter narrows List results. Empty fields match everything.
type ListFilter struct {
	Repo, Branch, Status, Q string
}

// List returns live plans (copies) matching the filter, newest first.
func (s *Store) List(f ListFilter) []*Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Plan
	q := strings.ToLower(f.Q)
	for _, pl := range s.idx.Plans {
		if f.Repo != "" && pl.Repo != f.Repo {
			continue
		}
		if f.Branch != "" && pl.Branch != f.Branch {
			continue
		}
		if f.Status != "" && pl.Status != f.Status {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(pl.Title), q) && !strings.Contains(strings.ToLower(pl.Slug), q) {
			continue
		}
		out = append(out, clone(pl))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out
}

// History returns a plan's version history (copy), oldest first.
func (s *Store) History(slug string) ([]VersionEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pl := s.idx.Plans[slug]
	if pl == nil {
		return nil, ErrNotFound
	}
	return append([]VersionEntry(nil), pl.History...), nil
}

// FindBySlugOrID resolves a live plan by slug or by id (copy).
func (s *Store) FindBySlugOrID(key string) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pl, ok := s.idx.Plans[key]; ok {
		return clone(pl), nil
	}
	for _, pl := range s.idx.Plans {
		if pl.ID == key {
			return clone(pl), nil
		}
	}
	return nil, ErrNotFound
}

// FindByShareToken resolves a live, shared plan by its token (copy).
func (s *Store) FindByShareToken(token string) (*Plan, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, pl := range s.idx.Plans {
		if pl.ShareToken != nil && *pl.ShareToken == token {
			return clone(pl), nil
		}
	}
	return nil, ErrNotFound
}

// ---- rename / trash / share ----

// Rename updates a plan's title and optionally its slug. On a slug change
// the folder is moved, the index re-keyed, and a redirect recorded.
// Returns the updated plan (copy).
func (s *Store) Rename(key, newTitle, newSlug string) (*Plan, error) {
	s.mu.Lock()
	pl, oldSlug := s.findLocked(key)
	s.mu.Unlock()
	if pl == nil {
		return nil, ErrNotFound
	}

	lock := s.planLock(oldSlug)
	lock.Lock()
	defer lock.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	pl = s.idx.Plans[oldSlug]
	if pl == nil {
		return nil, ErrNotFound
	}
	if newTitle != "" {
		pl.Title = newTitle
	}
	if newSlug != "" {
		target := Slugify(newSlug)
		if target != oldSlug {
			if _, taken := s.idx.Plans[target]; taken {
				return nil, fmt.Errorf("slug %q already in use", target)
			}
			if err := os.Rename(s.planDir(oldSlug), s.planDir(target)); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			delete(s.idx.Plans, oldSlug)
			pl.Slug = target
			s.idx.Plans[target] = pl
			s.idx.Redirects[oldSlug] = target
			// Collapse any redirect chains that pointed at oldSlug.
			for from, to := range s.idx.Redirects {
				if to == oldSlug {
					s.idx.Redirects[from] = target
				}
			}
		}
	}
	pl.Updated = time.Now().UTC()
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return clone(pl), nil
}

// findLocked resolves a live plan by slug or id. Caller holds s.mu.
func (s *Store) findLocked(key string) (*Plan, string) {
	if pl, ok := s.idx.Plans[key]; ok {
		return pl, key
	}
	for slug, pl := range s.idx.Plans {
		if pl.ID == key {
			return pl, slug
		}
	}
	return nil, ""
}

// Delete archives a plan to trash. Returns whether it had an active share.
func (s *Store) Delete(key string) (wasShared bool, err error) {
	s.mu.Lock()
	_, slug := s.findLocked(key)
	s.mu.Unlock()
	if slug == "" {
		return false, ErrNotFound
	}

	lock := s.planLock(slug)
	lock.Lock()
	defer lock.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	pl := s.idx.Plans[slug]
	if pl == nil {
		return false, ErrNotFound
	}
	wasShared = pl.ShareToken != nil
	dst := filepath.Join(s.dir, "trash", slug)
	_ = os.RemoveAll(dst) // clear any stale trash for this slug
	if err := os.Rename(s.planDir(slug), dst); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	now := time.Now().UTC()
	pl.TrashedAt = &now
	delete(s.idx.Plans, slug)
	s.idx.Trash[slug] = pl
	if err := s.saveLocked(); err != nil {
		return false, err
	}
	return wasShared, nil
}

// ListTrash returns trashed plans (copies), newest-trashed first.
func (s *Store) ListTrash() []*Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Plan
	for _, pl := range s.idx.Trash {
		out = append(out, clone(pl))
	}
	sort.Slice(out, func(i, j int) bool {
		var ti, tj time.Time
		if out[i].TrashedAt != nil {
			ti = *out[i].TrashedAt
		}
		if out[j].TrashedAt != nil {
			tj = *out[j].TrashedAt
		}
		return ti.After(tj)
	})
	return out
}

// RestoreTrash moves a plan back from trash to live. Returns the plan (copy).
func (s *Store) RestoreTrash(slug string) (*Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pl := s.idx.Trash[slug]
	if pl == nil {
		return nil, ErrNotFound
	}
	target := slug
	if _, taken := s.idx.Plans[slug]; taken {
		target = s.uniqueSlugLocked(slug)
	}
	if err := os.Rename(filepath.Join(s.dir, "trash", slug), s.planDir(target)); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	pl.TrashedAt = nil
	pl.Slug = target
	delete(s.idx.Trash, slug)
	s.idx.Plans[target] = pl
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return clone(pl), nil
}

// PurgeTrash permanently deletes a trashed plan (homepage-only per spec).
func (s *Store) PurgeTrash(slug string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.idx.Trash[slug]; !ok {
		return ErrNotFound
	}
	if err := os.RemoveAll(filepath.Join(s.dir, "trash", slug)); err != nil {
		return err
	}
	delete(s.idx.Trash, slug)
	return s.saveLocked()
}

// Share generates (or returns the existing) share token for a plan.
// Returns the token and whether this was the first active share overall.
func (s *Store) Share(key string) (token string, firstOverall bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pl, _ := s.findLocked(key)
	if pl == nil {
		return "", false, ErrNotFound
	}
	previousToken := pl.ShareToken
	firstOverall = s.activeShareCountLocked() == 0
	if pl.ShareToken == nil {
		t := newShareToken()
		pl.ShareToken = &t
	}
	if err := s.saveLocked(); err != nil {
		pl.ShareToken = previousToken
		return "", false, err
	}
	return *pl.ShareToken, firstOverall, nil
}

// Unshare revokes a plan's share token.
// Returns whether that was the last active share overall.
func (s *Store) Unshare(key string) (lastOverall bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pl, _ := s.findLocked(key)
	if pl == nil {
		return false, ErrNotFound
	}
	previousToken := pl.ShareToken
	pl.ShareToken = nil
	lastOverall = s.activeShareCountLocked() == 0
	if err := s.saveLocked(); err != nil {
		pl.ShareToken = previousToken
		return false, err
	}
	return lastOverall, nil
}

// ActiveShareCount reports how many live plans currently have a share token.
func (s *Store) ActiveShareCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeShareCountLocked()
}

func (s *Store) activeShareCountLocked() int {
	n := 0
	for _, pl := range s.idx.Plans {
		if pl.ShareToken != nil {
			n++
		}
	}
	return n
}
