package connector

import (
	"bufio"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// SessionRecord is one conversation found in the local Claude transcripts.
type SessionRecord struct {
	// ExternalID is the id Claude Code uses, which is what --resume accepts.
	ExternalID string
	CWD        string
	Title      string
	Model      string
	UpdatedAt  int64
	Version    string
	Path       string
}

// transcriptLine is the subset of a transcript record discovery needs.
//
// Transcripts carry a dozen record kinds and full message bodies; decoding only
// these fields keeps a scan of a 13,000-line file cheap.
type transcriptLine struct {
	Type        string `json:"type"`
	SessionID   string `json:"sessionId"`
	CWD         string `json:"cwd"`
	Timestamp   string `json:"timestamp"`
	CustomTitle string `json:"customTitle"`
	LastPrompt  string `json:"lastPrompt"`
	Slug        string `json:"slug"`
	Model       string `json:"requestedModel"`
	IsMeta      bool   `json:"isMeta"`
	Message     struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type cachedTranscript struct {
	size    int64
	modTime int64
	record  SessionRecord
	ok      bool
}

// Discovery lists the sessions Claude Code has on this machine.
//
// Parsing every transcript on every cycle would be wasteful — some are tens of
// thousands of lines — so a file is re-read only when its size or modification
// time changes.
type Discovery struct {
	Root string
	Log  *slog.Logger

	mu    sync.Mutex
	cache map[string]cachedTranscript
}

// NewDiscovery points at the default transcript tree.
func NewDiscovery(log *slog.Logger) *Discovery {
	root := os.Getenv("AA_TRANSCRIPT_ROOT")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		root = filepath.Join(home, ".claude", "projects")
	}
	return &Discovery{Root: root, Log: log, cache: map[string]cachedTranscript{}}
}

// Scan returns every session, most recently active first.
func (d *Discovery) Scan() ([]SessionRecord, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	seen := map[string]bool{}
	var out []SessionRecord

	err := filepath.WalkDir(d.Root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// A project directory can disappear while walking; skip it rather
			// than aborting the whole scan.
			return nil
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		seen[path] = true

		if cached, ok := d.cache[path]; ok && cached.size == info.Size() && cached.modTime == info.ModTime().UnixNano() {
			if cached.ok {
				out = append(out, cached.record)
			}
			return nil
		}

		record, ok := parseTranscript(path, info.ModTime())
		record.Version = strconv.FormatInt(info.Size(), 10) + ":" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
		d.cache[path] = cachedTranscript{
			size: info.Size(), modTime: info.ModTime().UnixNano(), record: record, ok: ok,
		}
		if ok {
			out = append(out, record)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	for path := range d.cache {
		if !seen[path] {
			delete(d.cache, path)
		}
	}

	return dedupeBySession(out), nil
}

// dedupeBySession keeps one record per conversation.
//
// Several files can claim the same session id: a resumed session, or a
// subagent transcript that carries its parent's id. Letting the last one win
// moves the conversation into whichever project that file happened to live in,
// and replaces its title with whatever that file's last prompt was. The file
// named after the session is the canonical transcript, so it wins; the others
// only fill in fields it lacks.
func dedupeBySession(records []SessionRecord) []SessionRecord {
	byID := map[string]SessionRecord{}
	primary := map[string]bool{}

	for _, record := range records {
		existing, seen := byID[record.ExternalID]
		if !seen {
			byID[record.ExternalID] = record
			primary[record.ExternalID] = isCanonicalTranscript(record)
			continue
		}
		recordIsPrimary := isCanonicalTranscript(record)
		// A canonical file replaces a non-canonical one; otherwise the newer
		// record wins, and any gap is filled from the one being replaced.
		if (recordIsPrimary && !primary[record.ExternalID]) ||
			(recordIsPrimary == primary[record.ExternalID] && record.UpdatedAt > existing.UpdatedAt) {
			if record.CWD == "" {
				record.CWD = existing.CWD
			}
			if record.Model == "" {
				record.Model = existing.Model
			}
			if record.Title == "" || record.Title == record.ExternalID {
				record.Title = existing.Title
			}
			byID[record.ExternalID] = record
			primary[record.ExternalID] = recordIsPrimary
			continue
		}
		if existing.CWD == "" {
			existing.CWD = record.CWD
			byID[record.ExternalID] = existing
		}
	}

	out := make([]SessionRecord, 0, len(byID))
	for _, record := range byID {
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out
}

// isCanonicalTranscript reports whether the file is named after its session,
// which is how Claude Code writes the transcript a person actually used.
func isCanonicalTranscript(record SessionRecord) bool {
	return strings.TrimSuffix(filepath.Base(record.Path), ".jsonl") == record.ExternalID
}

// parseTranscript reads one transcript file and summarises it.
func parseTranscript(path string, modTime time.Time) (SessionRecord, bool) {
	file, err := os.Open(path)
	if err != nil {
		return SessionRecord{}, false
	}
	defer file.Close()

	record := SessionRecord{
		ExternalID: strings.TrimSuffix(filepath.Base(path), ".jsonl"),
		Path:       path,
		UpdatedAt:  modTime.UnixMilli(),
	}

	var firstUser string
	var firstGoodUser string
	var lastUser string
	var lastPrompt string
	var lastStamp int64
	var customTitle string
	var slug string
	var model string
	var cwd string
	sawRecord := false

	reader := bufio.NewReaderSize(file, 1<<16)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			var rec transcriptLine
			// A record that does not decode is almost always a line that was
			// being written while we read; ignoring it is correct.
			if json.Unmarshal(line, &rec) == nil {
				sawRecord = true
				if rec.SessionID != "" {
					record.ExternalID = rec.SessionID
				}
				if cwd == "" && rec.CWD != "" {
					cwd = rec.CWD
				}
				if rec.CustomTitle != "" {
					customTitle = rec.CustomTitle
				}
				if rec.Slug != "" {
					slug = rec.Slug
				}
				if !isSyntheticPrompt(rec.LastPrompt) {
					lastPrompt = rec.LastPrompt
				}
				if rec.Model != "" {
					model = rec.Model
				}
				if stamp := parseStamp(rec.Timestamp); stamp > lastStamp {
					lastStamp = stamp
				}
				if rec.Type == "user" && rec.Message.Role == "user" && !rec.IsMeta {
					if text := userText(rec.Message.Content); !isSyntheticPrompt(text) {
						if firstUser == "" {
							firstUser = text
						}
						if firstGoodUser == "" && isUsableTitle(text) {
							firstGoodUser = text
						}
						lastUser = text
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
	}
	if !sawRecord {
		return SessionRecord{}, false
	}

	record.CWD = cwd
	record.Model = model
	if lastStamp > record.UpdatedAt {
		record.UpdatedAt = lastStamp
	}
	// A slug is the least informative label Claude generates, so it only wins
	// when the transcript has nothing a person would recognise.
	// The first real question names the conversation; the last one changes every
	// turn and would make the list jump. A slug is the least informative label
	// Claude generates, so it only wins when nothing else exists.
	switch {
	case customTitle != "":
		record.Title = customTitle
	case firstGoodUser != "":
		record.Title = firstLine(firstGoodUser, 120)
	case isUsableTitle(lastPrompt):
		record.Title = firstLine(lastPrompt, 120)
	case isUsableTitle(lastUser):
		record.Title = firstLine(lastUser, 120)
	case slug != "":
		record.Title = slug
	case firstUser != "":
		record.Title = firstLine(firstUser, 120)
	default:
		record.Title = record.ExternalID
	}
	return record, true
}

// userText pulls readable text out of a message content field, which is either
// a string or a list of typed blocks.
func userText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		for _, block := range blocks {
			if block.Type == "text" && block.Text != "" {
				return block.Text
			}
		}
	}
	return ""
}

// firstLine collapses whitespace and clips a title.
func firstLine(value string, limit int) string {
	value = stripLeadingReference(value)
	value = strings.Join(strings.Fields(value), " ")
	value = strings.TrimSpace(value)
	// Clip by rune, not by byte: cutting a UTF-8 sequence in half is what turns
	// a readable Chinese title into replacement characters.
	if utf8.RuneCountInString(value) > limit {
		runes := []rune(value)
		value = strings.TrimSpace(string(runes[:limit])) + "…"
	}
	if value == "" {
		return "Untitled"
	}
	return value
}

// parseStamp converts an ISO-8601 timestamp to Unix milliseconds.
func parseStamp(value string) int64 {
	if value == "" {
		return 0
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z0700",
		time.RFC3339,
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UnixMilli()
		}
	}
	return 0
}

// isSyntheticPrompt reports whether a line of text was injected by tooling
// rather than typed by the person.
//
// Transcripts carry system reminders, slash-command frames, cross-session
// notices and harness envelopes. Using one of those as a session title gives
// the list an entry nobody can recognise.
func isSyntheticPrompt(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return true
	}
	lower := strings.ToLower(trimmed)
	for _, prefix := range []string{
		"<",
		"caveat:",
		"[workflow harness",
		"the user sent a new message",
		"another claude session sent a message",
		"this session is being continued",
		"[request interrupted",
		"[request cancelled",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// isUsableTitle reports whether a line of text would make a name a person can
// recognise in a list.
//
// "你好", a bare file path, or a slash command are all technically the first
// thing that was typed, and all useless as a label.
func isUsableTitle(text string) bool {
	trimmed := strings.TrimSpace(text)
	// Weight by information, not by rune count: a five-character Chinese
	// question says as much as eight Latin characters, and counting runes
	// flatly threw away most Chinese prompts.
	if titleWeight(trimmed) < 8 {
		return false
	}
	lower := strings.ToLower(trimmed)
	switch lower {
	case "hello", "hi", "hey", "test", "testing", "ok", "okay":
		return false
	}
	if strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "!") {
		return false
	}
	// A prompt that is only a path or a command line names no task.
	if looksLikePath(trimmed) || strings.HasPrefix(lower, "claude ") {
		return false
	}
	return true
}

// looksLikePath reports whether the text is dominated by a filesystem path.
func looksLikePath(text string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}
	pathy := 0
	for _, field := range fields {
		if strings.ContainsAny(field, "/\\") && !strings.Contains(field, " ") {
			pathy++
		}
	}
	return pathy*2 >= len(fields) && !strings.ContainsAny(text, "的吗呢吧怎么为什")
}

// stripLeadingReference removes a path or URL glued to the front of a prompt.
//
// People often paste a file then type the question, so the transcript line reads
// like `"C:\x\y.xls"这是要填写的表…`. The filename is already implied by the
// project; the question is what names the conversation.
func stripLeadingReference(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return trimmed
	}
	if trimmed[0] == '"' || trimmed[0] == '\'' {
		quote := trimmed[0]
		if end := strings.IndexByte(trimmed[1:], quote); end >= 0 {
			reference := trimmed[1 : end+1]
			rest := strings.TrimSpace(trimmed[end+2:])
			if looksLikeReference(reference) && titleWeight(rest) >= 8 {
				return rest
			}
		}
		return trimmed
	}
	if field, rest, found := strings.Cut(trimmed, " "); found {
		if looksLikeReference(field) && titleWeight(strings.TrimSpace(rest)) >= 8 {
			return strings.TrimSpace(rest)
		}
	}
	// A reference glued straight onto the question: D:\a\b.xls这是要填写的表
	// The scan stops at the first space, because a reference never contains one
	// and the question after it often does.
	for i, r := range trimmed {
		if r == ' ' || r == '\t' {
			break
		}
		if isCJK(r) {
			prefix := trimmed[:i]
			if i >= 8 && looksLikeReference(prefix) && titleWeight(trimmed[i:]) >= 8 {
				return strings.TrimSpace(trimmed[i:])
			}
			break
		}
	}
	return trimmed
}

// looksLikeReference reports whether a token is a path or a URL.
func looksLikeReference(token string) bool {
	if token == "" {
		return false
	}
	if strings.HasPrefix(token, "http://") || strings.HasPrefix(token, "https://") {
		return true
	}
	if len(token) >= 2 && token[1] == ':' {
		return true
	}
	return strings.ContainsAny(token, `/\`)
}

func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3400 && r <= 0x4DBF)
}

// titleWeight scores how much a line says: CJK characters carry a word each,
// Latin text roughly a fifth of a word per character.
func titleWeight(text string) int {
	weight := 0
	for _, r := range text {
		switch {
		case isCJK(r):
			weight += 2
		default:
			weight++
		}
	}
	return weight
}
