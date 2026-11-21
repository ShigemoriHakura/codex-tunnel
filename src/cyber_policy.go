package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"time"
)

var errCyberPolicy = &responsesError{400, "cyber_policy", "Codex stopped this request for cybersecurity risk. Do not retry this request."}

const cyberReplayWindow = 24 * time.Hour

// Hash the effective model input and conversation scope. Delivery options and
// per-attempt client metadata are deliberately outside the fingerprint.
func cyberReplayKey(owner, session, route, model, head string, input any, instructions string, tools []M) []byte {
	h := sha256.New()
	_ = json.NewEncoder(h).Encode([]any{owner, session, route, model, head, input, instructions, tools})
	return h.Sum(nil)
}

func cyberReplayBlocked(ctx context.Context, db *sql.DB, key []byte) (bool, error) {
	var blocked bool
	err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM cyber_replay_blocks WHERE fingerprint=? AND expires>?)", key, now()).Scan(&blocked)
	return blocked, err
}

func rememberCyberBlock(db *sql.DB, key []byte) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM cyber_replay_blocks WHERE expires<=?", now()); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT OR REPLACE INTO cyber_replay_blocks(fingerprint,expires) VALUES(?,?)", key, time.Now().Add(cyberReplayWindow).Unix()); err != nil {
		return err
	}
	return tx.Commit()
}
