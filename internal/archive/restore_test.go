package archive

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"bot-bvk/internal/domain"
	"bot-bvk/internal/postgres"
	"github.com/jackc/pgx/v5"
)

func TestBackupRestoresDatabaseAndFiles(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" || os.Getenv("TEST_BACKUP_TOOLS") == "" {
		t.Skip("TEST_DATABASE_URL and TEST_BACKUP_TOOLS required (pg_dump/pg_restore on PATH)")
	}
	ctx := context.Background()
	base, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close(ctx)
	name := fmt.Sprintf("bvk_backup_test_%x", time.Now().UnixNano())
	if _, err = base.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatal(err)
	}
	defer base.Exec(ctx, `DROP DATABASE "`+name+`" WITH (FORCE)`)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	dsn = u.String()
	store, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = store.Bootstrap(ctx, 1, []string{"Example"}); err != nil {
		t.Fatal(err)
	}
	companies, _ := store.Companies(ctx)
	archiveDir := t.TempDir()
	backupDir := t.TempDir()
	source := filepath.Join(archiveDir, "original.bin")
	if err = os.WriteFile(source, []byte("attachment contents"), 0600); err != nil {
		t.Fatal(err)
	}
	hash, _ := checksum(source)
	closedAt := time.Now().Add(-31 * 24 * time.Hour)
	ticket := domain.Ticket{ClientID: 1, CompanyID: companies[0].ID, Department: domain.IT, Status: domain.Closed, CreatedAt: closedAt, ClosedAt: &closedAt, TopicChatID: -1001, TopicID: 100}
	msg := domain.Message{AuthorID: 1, Direction: "inbound", Text: "original message", FileID: "telegram-file", ArchivePath: source, SHA256: hash}
	if err = store.Transact(ctx, func(tx domain.Tx) error {
		if e := tx.SaveTicket(ctx, &ticket); e != nil {
			return e
		}
		msg.TicketID = ticket.ID
		return tx.AddMessage(ctx, &msg)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = Create(ctx, dsn, archiveDir, backupDir); err != nil {
		t.Fatal(err)
	}
	manifest, err := Latest(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	messages, _ := store.Messages(ctx, ticket.ID)
	events, _ := store.Events(ctx, ticket.ID)
	if !manifest.Covers(ticket, messages, events) {
		t.Fatal("verified restored backup did not cover original ticket")
	}
	// The copied file stays independent from the live archive directory.
	if err = os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if !manifest.Covers(ticket, messages, events) {
		t.Fatal("backup depended on original file")
	}
	if err = os.WriteFile(filepath.Join(manifest.Directory, "database.dump"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if manifest.Covers(ticket, messages, events) {
		t.Fatal("corrupted database dump accepted")
	}
}
