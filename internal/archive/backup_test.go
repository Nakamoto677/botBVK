package archive

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"bot-bvk/internal/domain"
)

func TestManifestRejectsMissingAndChangedArchives(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "database.dump"), []byte("snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "attachment"), []byte("valid"), 0600); err != nil {
		t.Fatal(err)
	}
	hash, _ := checksum(filepath.Join(dir, "attachment"))
	ticket := domain.Ticket{ID: 1, Version: 3}
	msg := domain.Message{ID: 2, FileID: "remote", SHA256: hash}
	dumpHash, _ := checksum(filepath.Join(dir, "database.dump"))
	m := Manifest{Verified: true, DumpSHA256: dumpHash, Directory: dir, Tickets: map[int64]TicketSnapshot{1: {Version: 3, MessageID: 2, EventID: 5, Complete: true}}, Files: map[int64]File{2: {Path: "attachment", SHA256: hash}}}
	if !m.Covers(ticket, []domain.Message{msg}, nil) {
		t.Fatal("valid backup rejected")
	}
	if m.Covers(ticket, []domain.Message{msg}, []domain.Event{{ID: 6, Kind: "message"}}) {
		t.Fatal("new event not backed up")
	}
	os.WriteFile(filepath.Join(dir, "attachment"), []byte("corrupted"), 0600)
	if m.Covers(ticket, []domain.Message{msg}, nil) {
		t.Fatal("corrupted file accepted")
	}
}
func TestRejectPathEscape(t *testing.T) {
	if _, err := safeRelative("/tmp/archive", "/tmp/secrets"); err == nil {
		t.Fatal("escaped archive path accepted")
	}
}

func TestCompletionProofSurvivesNewCycleButRejectsTranscriptChanges(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "database.dump"), []byte("verified"), 0600); err != nil {
		t.Fatal(err)
	}
	dumpHash, _ := checksum(filepath.Join(dir, "database.dump"))
	at := time.Now().UTC()
	completion := domain.Event{ID: 3, Kind: "completion_cleanup_requested", CreatedAt: at}
	original := domain.Message{ID: 1, TicketID: 1, Text: "saved closing transcript", CreatedAt: at.Add(-time.Minute)}
	manifest := Manifest{Verified: true, Directory: dir, DumpSHA256: dumpHash, Tickets: map[int64]TicketSnapshot{1: {Version: 1, MessageID: 1, EventID: 3, Complete: true, MessageHashes: map[int64]string{1: messageHash(original)}}}}
	reopened := domain.Ticket{ID: 1, Version: 9, Status: domain.InProgress, TopicID: 99}
	events := []domain.Event{completion, {ID: 4, Kind: "reopened", CreatedAt: at.Add(time.Second)}, {ID: 5, Kind: "cleanup_queued"}, {ID: 6, Kind: "cleanup_blocked"}, {ID: 7, Kind: "topic_deleted"}}
	newCycle := domain.Message{ID: 2, TicketID: 1, Text: "new active cycle", CreatedAt: at.Add(time.Minute)}
	if !manifest.CoversCompletion(reopened, []domain.Message{original, newCycle}, events, completion) {
		t.Fatal("detached completion proof blocked by new cycle or housekeeping")
	}
	lateReplacement := append(append([]domain.Event{}, events...), domain.Event{ID: 8, Kind: "media_edit_unsupported", CreatedAt: at.Add(time.Hour)})
	if manifest.CoversCompletion(reopened, []domain.Message{original}, lateReplacement, completion) {
		t.Fatal("late unarchived media replacement accepted")
	}
	original.Text = "edited old transcript"
	if manifest.CoversCompletion(reopened, []domain.Message{original}, events, completion) {
		t.Fatal("late original-message edit accepted")
	}
	if manifest.CoversCompletion(reopened, nil, events, domain.Event{ID: 8, Kind: "completion_cleanup_requested", CreatedAt: at}) {
		t.Fatal("completion absent from backup accepted")
	}
}
