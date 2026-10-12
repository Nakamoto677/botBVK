// Package archive creates independent backups and verifies them by restoring
// PostgreSQL into a disposable database before issuing a cleanup manifest.
package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bot-bvk/internal/domain"
	"bot-bvk/internal/postgres"
	"github.com/jackc/pgx/v5"
)

type TicketSnapshot struct {
	Version       int64            `json:"version"`
	MessageID     int64            `json:"message_id"`
	EventID       int64            `json:"event_id"`
	Complete      bool             `json:"complete"`
	MessageHashes map[int64]string `json:"message_hashes,omitempty"`
}
type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type Manifest struct {
	CreatedAt  time.Time                `json:"created_at"`
	Verified   bool                     `json:"verified"`
	DumpSHA256 string                   `json:"dump_sha256"`
	Tickets    map[int64]TicketSnapshot `json:"tickets"`
	Files      map[int64]File           `json:"files"`
	Directory  string                   `json:"-"`
}

func checksum(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func safeRelative(base, path string) (string, error) {
	rel, e := filepath.Rel(base, path)
	if e != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("archive file outside configured directory")
	}
	return rel, nil
}
func run(ctx context.Context, dsn, name string, args ...string) error {
	u, e := url.Parse(dsn)
	if e != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return fmt.Errorf("backup requires postgres:// connection URL")
	}
	c := exec.CommandContext(ctx, name, args...)
	env := map[string]string{"PGHOST": u.Hostname(), "PGPORT": u.Port(), "PGDATABASE": strings.TrimPrefix(u.Path, "/"), "PGUSER": "", "PGPASSWORD": ""}
	if env["PGPORT"] == "" {
		env["PGPORT"] = "5432"
	}
	if u.User != nil {
		env["PGUSER"] = u.User.Username()
		env["PGPASSWORD"], _ = u.User.Password()
	}
	if mode := u.Query().Get("sslmode"); mode != "" {
		env["PGSSLMODE"] = mode
	}
	for _, existing := range os.Environ() {
		key, _, _ := strings.Cut(existing, "=")
		if _, replace := env[key]; !replace {
			c.Env = append(c.Env, existing)
		}
	}
	for key, value := range env {
		c.Env = append(c.Env, key+"="+value)
	}
	// Command output may contain connection credentials. Never echo it to logs.
	if e := c.Run(); e != nil {
		return fmt.Errorf("%s failed (check PostgreSQL tools and database privileges): %w", name, e)
	}
	return nil
}

var backupMu sync.Mutex

func Create(ctx context.Context, dsn, archiveDir, backupDir string) (string, error) {
	backupMu.Lock()
	defer backupMu.Unlock()
	stamp := time.Now().UTC()
	dir := filepath.Join(backupDir, stamp.Format("20060102T150405.000000000Z"))
	if e := os.MkdirAll(dir, 0700); e != nil {
		return "", e
	}
	dump := filepath.Join(dir, "database.dump")
	if e := run(ctx, dsn, "pg_dump", "--format=custom", "--file", dump); e != nil {
		return "", e
	}
	u, e := url.Parse(dsn)
	if e != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", fmt.Errorf("backup requires postgres:// connection URL")
	}
	conn, e := pgx.Connect(ctx, dsn)
	if e != nil {
		return "", fmt.Errorf("connect for backup verification: %w", e)
	}
	defer conn.Close(context.Background())
	name := fmt.Sprintf("bvk_verify_%x", stamp.UnixNano())
	if _, e = conn.Exec(ctx, `CREATE DATABASE "`+name+`"`); e != nil {
		return "", fmt.Errorf("backup verification requires CREATE DATABASE privilege: %w", e)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = conn.Exec(cleanupCtx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
	}()
	u.Path = "/" + name
	restoreDSN := u.String()
	if e = run(ctx, restoreDSN, "pg_restore", "--exit-on-error", "--no-owner", "--no-privileges", "--dbname", name, dump); e != nil {
		return "", e
	}
	restored, e := postgres.New(ctx, restoreDSN)
	if e != nil {
		return "", e
	}
	defer restored.Close()
	all, e := restored.Tickets(ctx)
	if e != nil {
		return "", e
	}
	manifest := Manifest{CreatedAt: stamp, Tickets: map[int64]TicketSnapshot{}, Files: map[int64]File{}}
	for _, t := range all {
		snap := TicketSnapshot{Version: t.Version, Complete: true, MessageHashes: map[int64]string{}}
		msgs, e := restored.Messages(ctx, t.ID)
		if e != nil {
			return "", e
		}
		for _, m := range msgs {
			snap.MessageHashes[m.ID] = messageHash(m)
			if m.ID > snap.MessageID {
				snap.MessageID = m.ID
			}
			if m.FileID == "" {
				continue
			}
			if m.ArchivePath == "" || m.SHA256 == "" {
				snap.Complete = false
				continue
			}
			rel, e := safeRelative(archiveDir, m.ArchivePath)
			if e != nil {
				return "", e
			}
			destination := filepath.Join(dir, "files", rel)
			if e = os.MkdirAll(filepath.Dir(destination), 0700); e != nil {
				return "", e
			}
			if e = copyFile(m.ArchivePath, destination); e != nil {
				return "", e
			}
			actual, e := checksum(destination)
			if e != nil || actual != m.SHA256 {
				return "", fmt.Errorf("backup checksum mismatch for attachment %d", m.ID)
			}
			manifest.Files[m.ID] = File{Path: filepath.Join("files", rel), SHA256: actual}
		}
		events, e := restored.Events(ctx, t.ID)
		if e != nil {
			return "", e
		}
		for _, event := range events {
			if event.ID > snap.EventID {
				snap.EventID = event.ID
			}
			if event.Kind == "media_edit_unsupported" {
				snap.Complete = false
			}
		}
		manifest.Tickets[t.ID] = snap
	}
	manifest.DumpSHA256, e = checksum(dump)
	if e != nil {
		return "", e
	}
	manifest.Verified = true
	b, e := json.MarshalIndent(manifest, "", "  ")
	if e != nil {
		return "", e
	}
	if e = os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0600); e != nil {
		return "", e
	}
	return dir, nil
}
func copyFile(source, destination string) error {
	in, e := os.Open(source)
	if e != nil {
		return e
	}
	defer in.Close()
	out, e := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = io.Copy(out, in)
	closeErr := out.Close()
	if e != nil {
		return e
	}
	return closeErr
}

func Latest(dir string) (Manifest, error) {
	entries, e := os.ReadDir(dir)
	if e != nil {
		return Manifest{}, e
	}
	var latest Manifest
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name(), "manifest.json")
		b, e := os.ReadFile(path)
		if e != nil {
			continue
		}
		var m Manifest
		if json.Unmarshal(b, &m) != nil || !m.Verified {
			continue
		}
		m.Directory = filepath.Dir(path)
		if m.CreatedAt.After(latest.CreatedAt) {
			latest = m
		}
	}
	if !latest.Verified {
		return Manifest{}, domain.ErrNotFound
	}
	return latest, nil
}
func (m Manifest) Covers(t domain.Ticket, msgs []domain.Message, events []domain.Event) bool {
	snap, ok := m.Tickets[t.ID]
	if !m.Verified || !ok || !snap.Complete || snap.Version != t.Version {
		return false
	}
	dumpHash, e := checksum(filepath.Join(m.Directory, "database.dump"))
	if e != nil || m.DumpSHA256 == "" || dumpHash != m.DumpSHA256 {
		return false
	}
	for _, msg := range msgs {
		if msg.ID > snap.MessageID {
			return false
		}
		if msg.FileID != "" {
			f, ok := m.Files[msg.ID]
			if !ok || !filepath.IsLocal(f.Path) || f.SHA256 != msg.SHA256 {
				return false
			}
			actual, e := checksum(filepath.Join(m.Directory, f.Path))
			if e != nil || actual != f.SHA256 {
				return false
			}
		}
	}
	for _, ev := range events {
		if ev.Kind == "media_edit_unsupported" {
			return false
		}
		if ev.ID > snap.EventID && !housekeeping(ev.Kind) {
			return false
		}
	}
	return true
}

func housekeeping(kind string) bool {
	switch kind {
	case "cleanup_queued", "cleanup_deferred", "cleanup_blocked", "cleanup_recovered", "topic_deleted":
		return true
	}
	return false
}
func messageHash(m domain.Message) string {
	data, _ := json.Marshal(m)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Load reads a specific durable verification proof, rather than replacing it
// with a possibly newer snapshot while an old topic waits in the outbox.
func Load(dir string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err = json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	if !m.Verified {
		return m, domain.ErrConflict
	}
	m.Directory = dir
	return m, nil
}

// CoversCompletion protects the transcript accepted before this completion.
// Later routing/claim changes can safely coexist with deletion of a detached
// old topic. Fingerprints detect later edits to its saved messages.
func (m Manifest) CoversCompletion(t domain.Ticket, msgs []domain.Message, events []domain.Event, completion domain.Event) bool {
	snap, ok := m.Tickets[t.ID]
	if !m.Verified || !ok || !snap.Complete || completion.Kind != "completion_cleanup_requested" || completion.CreatedAt.IsZero() || completion.ID == 0 || completion.ID > snap.EventID {
		return false
	}
	hash, err := checksum(filepath.Join(m.Directory, "database.dump"))
	if err != nil || m.DumpSHA256 == "" || hash != m.DumpSHA256 {
		return false
	}
	for _, msg := range msgs {
		if !msg.CreatedAt.IsZero() && msg.CreatedAt.After(completion.CreatedAt) {
			continue
		}
		if msg.ID > snap.MessageID || snap.MessageHashes[msg.ID] != messageHash(msg) {
			return false
		}
		if msg.FileID != "" {
			f, ok := m.Files[msg.ID]
			if !ok || !filepath.IsLocal(f.Path) || f.SHA256 != msg.SHA256 {
				return false
			}
			actual, err := checksum(filepath.Join(m.Directory, f.Path))
			if err != nil || actual != f.SHA256 {
				return false
			}
		}
	}
	for _, event := range events {
		if housekeeping(event.Kind) {
			continue
		}
		if event.CreatedAt.After(completion.CreatedAt) {
			continue
		}
		if event.Kind == "media_edit_unsupported" || event.ID > snap.EventID {
			return false
		}
	}
	return true
}

// CheckMessages checks live attachment availability before a new backup. This
// is separate from proof validation, whose copied files survive live-file loss.
func CheckMessages(msgs []domain.Message) error {
	for _, msg := range msgs {
		if msg.FileID == "" {
			continue
		}
		if msg.ArchivePath == "" || msg.SHA256 == "" {
			return fmt.Errorf("attachment of message %d is not archived", msg.ID)
		}
		actual, err := checksum(msg.ArchivePath)
		if err != nil {
			return fmt.Errorf("attachment of message %d unavailable: %w", msg.ID, err)
		}
		if actual != msg.SHA256 {
			return fmt.Errorf("attachment of message %d checksum mismatch", msg.ID)
		}
	}
	return nil
}
