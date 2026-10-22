package archive

import (
	"os"
	"path/filepath"
	"testing"

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
