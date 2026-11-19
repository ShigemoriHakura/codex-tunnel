package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type M = map[string]any

func id(prefix string) string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}
func digest(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }
func now() int64             { return time.Now().Unix() }
func openDB(dir string) (*sql.DB, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", filepath.Join(dir, "gateway.db")+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS keys(id TEXT PRIMARY KEY,name TEXT NOT NULL,hash TEXT UNIQUE NOT NULL,created INTEGER NOT NULL,revoked INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY,owner TEXT NOT NULL REFERENCES keys(id),name TEXT NOT NULL,thread TEXT NOT NULL DEFAULT '',model TEXT NOT NULL DEFAULT '',last_in INTEGER NOT NULL DEFAULT 0,last_out INTEGER NOT NULL DEFAULT 0,created INTEGER NOT NULL,updated INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS key_bindings(key_id TEXT PRIMARY KEY REFERENCES keys(id) ON DELETE CASCADE,session TEXT NOT NULL REFERENCES sessions(id) ON DELETE RESTRICT);
 CREATE INDEX IF NOT EXISTS key_bindings_session ON key_bindings(session);
 CREATE INDEX IF NOT EXISTS sessions_owner ON sessions(owner,updated DESC);
 CREATE TABLE IF NOT EXISTS response_turns(id TEXT PRIMARY KEY,owner TEXT NOT NULL,session TEXT REFERENCES sessions(id) ON DELETE CASCADE,parent TEXT NOT NULL,stored INTEGER NOT NULL,input BLOB NOT NULL,output BLOB NOT NULL,status TEXT NOT NULL,created INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS response_session ON response_turns(session,created);
 CREATE TABLE IF NOT EXISTS requests(id TEXT PRIMARY KEY,owner TEXT NOT NULL,session TEXT NOT NULL,kind TEXT NOT NULL,model TEXT NOT NULL,status INTEGER NOT NULL,input INTEGER NOT NULL,output INTEGER NOT NULL,metered INTEGER NOT NULL,latency_ms INTEGER NOT NULL,created INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS requests_owner ON requests(owner,created DESC);
 CREATE INDEX IF NOT EXISTS requests_created ON requests(created DESC);
 CREATE TABLE IF NOT EXISTS console_sessions(digest TEXT PRIMARY KEY,expires INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS settings(k TEXT PRIMARY KEY,v TEXT NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err = initDiagnostics(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

type querier interface {
	Query(string, ...any) (*sql.Rows, error)
}

func rows(db querier, q string, args ...any) ([]M, error) {
	r, e := db.Query(q, args...)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	cols, e := r.Columns()
	if e != nil {
		return nil, e
	}
	list := []M{}
	for r.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range ptrs {
			ptrs[i] = &values[i]
		}
		if e = r.Scan(ptrs...); e != nil {
			return nil, e
		}
		item := M{}
		for i, k := range cols {
			item[k] = values[i]
		}
		list = append(list, item)
	}
	return list, r.Err()
}
