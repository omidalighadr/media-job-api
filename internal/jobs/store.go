// Package jobs owns the durable queue and its state transitions.
package jobs

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("job not found")
var ErrConflict = errors.New("idempotency key belongs to a different upload")
var ErrBusy = errors.New("queue is full")
var ErrActive = errors.New("job is still active")

type Job struct {
	ID        string          `json:"id"`
	Status    string          `json:"status"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
	Error     string          `json:"error,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Input     []byte          `json:"-"`
}

type Store struct {
	db   *sql.DB
	lock *os.File
}

// Open takes an OS lock before recovering interrupted work. One process owns a
// data directory; SQLite durability does not make this a distributed queue.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "service.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("data directory already in use: %w", err)
	}
	// Create the database privately before the driver opens it.
	file, err := os.OpenFile(filepath.Join(dir, "jobs.db"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		lock.Close()
		return nil, err
	}
	file.Close()
	db, err := sql.Open("sqlite", filepath.Join(dir, "jobs.db"))
	if err != nil {
		lock.Close()
		return nil, err
	}
	s := &Store{db: db, lock: lock}
	db.SetMaxOpenConns(1)
	fail := func(e error) (*Store, error) { s.Close(); return nil, e }
	for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA busy_timeout=5000"} {
		if _, err = db.Exec(q); err != nil {
			return fail(err)
		}
	}
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fail(err)
	}
	if version > 1 {
		return fail(fmt.Errorf("unsupported database version %d", version))
	}
	if version == 0 {
		tx, e := db.Begin()
		if e != nil {
			return fail(e)
		}
		_, e = tx.Exec(`CREATE TABLE jobs (
   sequence INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, status TEXT NOT NULL CHECK(status IN ('queued','running','succeeded','failed','canceled')),
   created_at TEXT NOT NULL, updated_at TEXT NOT NULL, error TEXT NOT NULL DEFAULT '',
   result BLOB, input BLOB, digest TEXT NOT NULL, idempotency_key TEXT UNIQUE
  ); CREATE INDEX jobs_queue ON jobs(status,sequence); PRAGMA user_version=1;`)
		if e != nil {
			tx.Rollback()
			return fail(e)
		}
		if e = tx.Commit(); e != nil {
			return fail(e)
		}
	}
	_, err = db.Exec("UPDATE jobs SET status='queued',updated_at=? WHERE status='running'", now())
	if err != nil {
		return fail(err)
	}
	return s, nil
}
func (s *Store) Close() error { e := s.db.Close(); s.lock.Close(); return e }
func now() string             { return time.Now().UTC().Format(time.RFC3339Nano) }

const columns = "id,status,created_at,updated_at,error,result"

type scanner interface{ Scan(...any) error }

func scan(row scanner) (Job, error) {
	var j Job
	var b []byte
	e := row.Scan(&j.ID, &j.Status, &j.CreatedAt, &j.UpdatedAt, &j.Error, &b)
	j.Result = b
	if errors.Is(e, sql.ErrNoRows) {
		e = ErrNotFound
	}
	return j, e
}
func (s *Store) Get(id string) (Job, error) {
	return scan(s.db.QueryRow("SELECT "+columns+" FROM jobs WHERE id=?", id))
}

// Create stores bytes and queue state in the same transaction. Retried requests
// with the same key and digest return the original job, even when the queue is full.
func (s *Store) Create(id, key, digest string, input []byte, limit int) (Job, bool, error) {
	tx, e := s.db.Begin()
	if e != nil {
		return Job{}, false, e
	}
	defer tx.Rollback()
	if key != "" {
		var old, hash string
		e = tx.QueryRow("SELECT id,digest FROM jobs WHERE idempotency_key=?", key).Scan(&old, &hash)
		if e == nil {
			if hash != digest {
				return Job{}, false, ErrConflict
			}
			j, e := scan(tx.QueryRow("SELECT "+columns+" FROM jobs WHERE id=?", old))
			return j, false, e
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return Job{}, false, e
		}
	}
	var count int
	if e = tx.QueryRow("SELECT count(*) FROM jobs WHERE status IN ('queued','running')").Scan(&count); e != nil {
		return Job{}, false, e
	}
	if count >= limit {
		return Job{}, false, ErrBusy
	}
	var k any
	if key != "" {
		k = key
	}
	t := now()
	_, e = tx.Exec("INSERT INTO jobs(id,status,created_at,updated_at,input,digest,idempotency_key) VALUES(?,'queued',?,?,?,?,?)", id, t, t, input, digest, k)
	if e != nil {
		return Job{}, false, e
	}
	if e = tx.Commit(); e != nil {
		return Job{}, false, e
	}
	j, e := s.Get(id)
	return j, true, e
}
func (s *Store) Claim() (Job, error) {
	var j Job
	var result []byte
	e := s.db.QueryRow(`UPDATE jobs SET status='running',updated_at=? WHERE id=(SELECT id FROM jobs WHERE status='queued' ORDER BY sequence LIMIT 1) RETURNING id,status,created_at,updated_at,error,result,input`, now()).Scan(&j.ID, &j.Status, &j.CreatedAt, &j.UpdatedAt, &j.Error, &result, &j.Input)
	j.Result = result
	if errors.Is(e, sql.ErrNoRows) {
		e = ErrNotFound
	}
	return j, e
}

// Finish is conditional: a late subprocess result cannot overwrite cancellation.
func (s *Store) Finish(id, status, reason string, result []byte) error {
	if status != "succeeded" && status != "failed" && status != "queued" {
		return errors.New("invalid completion state")
	}
	_, e := s.db.Exec(`UPDATE jobs SET status=?,error=?,result=?,input=CASE WHEN ?='queued' THEN input ELSE NULL END,updated_at=? WHERE id=? AND status='running'`, status, reason, result, status, now(), id)
	return e
}
func (s *Store) Cancel(id string) (Job, error) {
	_, e := s.db.Exec("UPDATE jobs SET status='canceled',input=NULL,updated_at=? WHERE id=? AND status IN ('queued','running')", now(), id)
	if e != nil {
		return Job{}, e
	}
	return s.Get(id)
}
func (s *Store) Delete(id string) error {
	r, e := s.db.Exec("DELETE FROM jobs WHERE id=? AND status NOT IN ('queued','running')", id)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n == 0 {
		_, e = s.Get(id)
		if e != nil {
			return e
		}
		return ErrActive
	}
	return nil
}
func (s *Store) Ping() error { return s.db.Ping() }
