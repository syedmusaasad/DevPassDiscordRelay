package transcript_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/devpass/devpass-discord-relay/internal/transcript"
	_ "modernc.org/sqlite"
)

func createTestDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	schema := `
	CREATE TABLE project (id text PRIMARY KEY, name text NOT NULL DEFAULT '');
	CREATE TABLE session (
		id text PRIMARY KEY,
		project_id text NOT NULL,
		workspace_id text,
		parent_id text,
		slug text NOT NULL,
		directory text NOT NULL,
		path text,
		title text NOT NULL,
		version text NOT NULL,
		share_url text,
		summary_additions integer,
		summary_deletions integer,
		summary_files integer,
		summary_diffs text,
		metadata text,
		cost real DEFAULT 0 NOT NULL,
		tokens_input integer DEFAULT 0 NOT NULL,
		tokens_output integer DEFAULT 0 NOT NULL,
		tokens_reasoning integer DEFAULT 0 NOT NULL,
		tokens_cache_read integer DEFAULT 0 NOT NULL,
		tokens_cache_write integer DEFAULT 0 NOT NULL,
		revert text,
		permission text,
		agent text,
		model text,
		time_created integer NOT NULL,
		time_updated integer NOT NULL,
		time_compacting integer,
		time_archived integer,
		CONSTRAINT fk_session_project_id_project_id_fk FOREIGN KEY (project_id) REFERENCES project(id) ON DELETE CASCADE
	);
	CREATE TABLE message (
		id text PRIMARY KEY,
		session_id text NOT NULL,
		time_created integer NOT NULL,
		time_updated integer NOT NULL,
		data text NOT NULL,
		CONSTRAINT fk_message_session_id_session_id_fk FOREIGN KEY (session_id) REFERENCES session(id) ON DELETE CASCADE
	);
	CREATE TABLE part (
		id text PRIMARY KEY,
		message_id text NOT NULL,
		session_id text NOT NULL,
		time_created integer NOT NULL,
		time_updated integer NOT NULL,
		data text NOT NULL,
		CONSTRAINT fk_part_message_id_message_id_fk FOREIGN KEY (message_id) REFERENCES message(id) ON DELETE CASCADE
	);
	INSERT INTO project(id,name) VALUES('proj1','test');
	INSERT INTO session(id,project_id,slug,directory,title,version,time_created,time_updated)
		VALUES('ses_test1','proj1','test-slug','/tmp','Test Session','1.18.11',1000,1000);
	INSERT INTO message(id,session_id,time_created,time_updated,data)
		VALUES('msg1','ses_test1',1001,1001,'{"role":"user"}');
	INSERT INTO part(id,message_id,session_id,time_created,time_updated,data)
		VALUES('prt1','msg1','ses_test1',1002,1002,'{"type":"text","text":"hello"}');
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return path, db
}

func TestOpenDB(t *testing.T) {
	path, db := createTestDB(t)
	db.Close()

	rodb, err := transcript.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rodb.Close()
}

func TestGetSession(t *testing.T) {
	_, db := createTestDB(t)
	defer db.Close()

	ctx := context.Background()
	s, err := transcript.GetSession(ctx, db, "ses_test1")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "ses_test1" {
		t.Errorf("got session id %q want ses_test1", s.ID)
	}
}

func TestGetSession_NotFound(t *testing.T) {
	_, db := createTestDB(t)
	defer db.Close()

	ctx := context.Background()
	_, err := transcript.GetSession(ctx, db, "ses_notexist")
	if err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestTailer(t *testing.T) {
	path, writeDB := createTestDB(t)
	defer writeDB.Close()

	// Open a read-only connection for the tailer
	roDB, err := transcript.OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer roDB.Close()

	tailer := transcript.NewTailer(roDB, "ses_test1", 50*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Start from before the existing part
	ch, err := tailer.Tail(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case p := <-ch:
		if p.ID != "prt1" {
			t.Errorf("got part id %q want prt1", p.ID)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for part")
	}
}

func TestWaitForSession(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wait.db")

	// Write DB with a delay
	go func() {
		time.Sleep(150 * time.Millisecond)
		db, _ := sql.Open("sqlite", path)
		defer db.Close()
		db.Exec(`CREATE TABLE project (id text PRIMARY KEY, name text NOT NULL DEFAULT '');`)
		db.Exec(`CREATE TABLE session (
			id text PRIMARY KEY,
			project_id text NOT NULL,
			workspace_id text,
			parent_id text,
			slug text NOT NULL,
			directory text NOT NULL,
			path text,
			title text NOT NULL,
			version text NOT NULL,
			share_url text,
			summary_additions integer,
			summary_deletions integer,
			summary_files integer,
			summary_diffs text,
			metadata text,
			cost real DEFAULT 0 NOT NULL,
			tokens_input integer DEFAULT 0 NOT NULL,
			tokens_output integer DEFAULT 0 NOT NULL,
			tokens_reasoning integer DEFAULT 0 NOT NULL,
			tokens_cache_read integer DEFAULT 0 NOT NULL,
			tokens_cache_write integer DEFAULT 0 NOT NULL,
			revert text,
			permission text,
			agent text,
			model text,
			time_created integer NOT NULL,
			time_updated integer NOT NULL,
			time_compacting integer,
			time_archived integer,
			CONSTRAINT fk_session_project_id_project_id_fk FOREIGN KEY (project_id) REFERENCES project(id) ON DELETE CASCADE
		);`)
		db.Exec(`INSERT INTO project(id,name) VALUES('p1','x');`)
		db.Exec(`INSERT INTO session(id,project_id,slug,directory,title,version,time_created,time_updated)
			VALUES('ses_wait1','p1','s','/tmp','T','1.18.11',1000,1000);`)
	}()

	// The DB file may not exist yet; we need a writable open for WaitForSession
	// Use a write-capable connection that waits for file to appear
	var waitDB *sql.DB
	for i := 0; i < 20; i++ {
		if _, err := os.Stat(path); err == nil {
			waitDB, _ = sql.Open("sqlite", path)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if waitDB == nil {
		// File not created yet, open anyway and let it retry
		waitDB, _ = sql.Open("sqlite", path)
	}
	defer waitDB.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	s, err := transcript.WaitForSession(ctx, waitDB, "ses_wait1", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForSession: %v", err)
	}
	if s.ID != "ses_wait1" {
		t.Errorf("got id %q want ses_wait1", s.ID)
	}
}
