package monitoring

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"math"
	"strconv"
	"time"

	"github.com/Real-kia/payesh/internal/contracts"
)

// AuthorityState is the durable ingest state of one server on this store.
type AuthorityState string

const (
	// AuthorityActive means this store accepts ingest for the server.
	AuthorityActive AuthorityState = "active"
	// AuthorityFrozen means ingest is refused while a cutover reads the final tail.
	AuthorityFrozen AuthorityState = "frozen"
	// AuthorityRelinquished means another store now owns the server.
	AuthorityRelinquished AuthorityState = "relinquished"
)

var (
	// ErrServerNotAuthoritative is returned, without acknowledging any batch,
	// when this store no longer owns ingest for the server.
	ErrServerNotAuthoritative = errors.New("monitoring: this store is not authoritative for the server")
	// ErrAuthorityConflict reports an invalid or conflicting authority transition.
	ErrAuthorityConflict = errors.New("monitoring: server authority conflict")
	// ErrAuthorityGenerationExhausted refuses a transition at the largest generation.
	ErrAuthorityGenerationExhausted = errors.New("monitoring: server authority generation exhausted")
)

// ServerAuthority is one server's durable ingest authority record.
type ServerAuthority struct {
	ServerID         contracts.ServerID
	Generation       uint64
	Owner            string
	State            AuthorityState
	CutoverID        string
	TransitionDigest string
	// FrontierDigest is the ingest-state digest recorded by the freeze (source)
	// or verified by the activation (destination) for this cutover.
	FrontierDigest string
}

// AuthorityTransition binds a freeze or relinquish to one cutover and its
// immutable request. FrontierDigest commits to the final ingest state that the
// cutover transfers. When it is empty on a freeze, the store computes it inside
// the freeze transaction; on a relinquish it defaults to the recorded freeze.
type AuthorityTransition struct {
	ServerID       contracts.ServerID
	CutoverID      string
	RequestDigest  string
	FrontierDigest string
}

// AuthorityHandoff is the destination's evidence that a source relinquished a
// server: the cutover, the source's final generation and request digest, and the
// frontier digest of the transferred state. Transport authentication of this
// record belongs to the peer protocol; the store verifies the frontier itself.
type AuthorityHandoff struct {
	ServerID            contracts.ServerID
	CutoverID           string
	SourceGeneration    uint64
	FrontierDigest      string
	SourceRequestDigest string
	NewOwner            string
}

func validAuthorityID(value string) bool {
	return len(value) >= 8 && len(value) <= 128 && isSafeServerID(value)
}

func validAuthorityDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validAuthorityServerID(id contracts.ServerID) bool {
	return len(id) >= 16 && len(id) <= 128 && isSafeServerID(string(id))
}

func (t AuthorityTransition) validate() error {
	if !validAuthorityServerID(t.ServerID) {
		return errors.New("authority transition server_id must be a bounded URL-safe identifier")
	}
	if !validAuthorityID(t.CutoverID) {
		return errors.New("authority transition cutover_id must be a bounded URL-safe identifier")
	}
	if !validAuthorityDigest(t.RequestDigest) || (t.FrontierDigest != "" && !validAuthorityDigest(t.FrontierDigest)) {
		return errors.New("authority transition digests must be lowercase SHA-256 hex")
	}
	return nil
}

func (h AuthorityHandoff) validate() error {
	if !validAuthorityServerID(h.ServerID) {
		return errors.New("handoff server_id must be a bounded URL-safe identifier")
	}
	if !validAuthorityID(h.CutoverID) || !validAuthorityID(h.NewOwner) {
		return errors.New("handoff cutover_id and owner must be bounded URL-safe identifiers")
	}
	if h.SourceGeneration < 1 || h.SourceGeneration >= math.MaxInt64 {
		return errors.New("handoff source generation is out of range")
	}
	if !validAuthorityDigest(h.FrontierDigest) || !validAuthorityDigest(h.SourceRequestDigest) {
		return errors.New("handoff digests must be lowercase SHA-256 hex")
	}
	return nil
}

// digest binds every handoff field, so the stored activation record cannot be
// reused for a different cutover, owner, source generation or frontier.
func (h AuthorityHandoff) digest() string {
	sum := sha256.Sum256([]byte("payesh-authority-handoff-v1\n" + string(h.ServerID) + "\n" + h.CutoverID + "\n" +
		strconv.FormatUint(h.SourceGeneration, 10) + "\n" + h.FrontierDigest + "\n" + h.SourceRequestDigest + "\n" + h.NewOwner))
	return hex.EncodeToString(sum[:])
}

// requireServerIngestAuthorityTx runs inside the ingest write transaction. A
// server with no authority row keeps legacy ingest; otherwise only an active
// record accepts batches, so a committed freeze serializes with every ingest.
func requireServerIngestAuthorityTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID) error {
	var state string
	err := tx.QueryRowContext(ctx, `SELECT state FROM server_authority WHERE server_id=?`, string(serverID)).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if AuthorityState(state) != AuthorityActive {
		return ErrServerNotAuthoritative
	}
	return nil
}

// requireServerCommandAuthorityTx is the same fence for command and desired-state
// mutations: creating or leasing jobs, applying action results, and changing
// module, control-policy or configuration state. It must run in the transaction
// that performs the effect so a committed freeze cannot be overtaken.
func requireServerCommandAuthorityTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID) error {
	return requireServerIngestAuthorityTx(ctx, tx, serverID)
}

func readServerAuthorityTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID) (ServerAuthority, bool, error) {
	var (
		a          ServerAuthority
		generation int64
		state      string
	)
	err := tx.QueryRowContext(ctx, `SELECT server_id,generation,owner_id,state,cutover_id,transition_digest FROM server_authority WHERE server_id=?`, string(serverID)).
		Scan(&a.ServerID, &generation, &a.Owner, &state, &a.CutoverID, &a.TransitionDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return ServerAuthority{}, false, nil
	}
	if err != nil {
		return ServerAuthority{}, false, err
	}
	a.Generation = uint64(generation)
	a.State = AuthorityState(state)
	if a.CutoverID != "" {
		err := tx.QueryRowContext(ctx, `SELECT frontier_digest FROM server_authority_transitions WHERE cutover_id=? AND server_id=? AND to_state IN ('frozen','activated') LIMIT 1`, a.CutoverID, string(serverID)).Scan(&a.FrontierDigest)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return ServerAuthority{}, false, err
		}
	}
	return a, true, nil
}

// RequireServerAuthority returns ErrServerNotAuthoritative when this store no
// longer owns the server. Layers that act outside the store, such as the
// certificate authority and enrollment issuance, call it before acting. The check
// is a point-in-time read, so it narrows rather than closes the window; every
// store write that follows is fenced inside its own transaction.
func (s *Store) RequireServerAuthority(ctx context.Context, serverID contracts.ServerID) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return requireServerCommandAuthorityTx(ctx, tx, serverID)
}

// GetServerAuthority reports a server's durable authority record, if any.
func (s *Store) GetServerAuthority(ctx context.Context, serverID contracts.ServerID) (ServerAuthority, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ServerAuthority{}, false, err
	}
	defer tx.Rollback()
	return readServerAuthorityTx(ctx, tx, serverID)
}

// InitializeServerAuthority explicitly makes ownerID the active ingest owner of
// a registered server. It is idempotent for the same owner and refuses any
// other owner; an ingest can never create or claim authority implicitly.
func (s *Store) InitializeServerAuthority(ctx context.Context, serverID contracts.ServerID, ownerID string) (ServerAuthority, error) {
	if !validAuthorityServerID(serverID) {
		return ServerAuthority{}, errors.New("server_id must be a bounded URL-safe identifier")
	}
	if !validAuthorityID(ownerID) {
		return ServerAuthority{}, errors.New("authority owner must be a bounded URL-safe identifier")
	}
	if err := s.ensureWritable(ctx); err != nil {
		return ServerAuthority{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ServerAuthority{}, err
	}
	defer tx.Rollback()
	existing, found, err := readServerAuthorityTx(ctx, tx, serverID)
	if err != nil {
		return ServerAuthority{}, err
	}
	if found {
		if existing.Owner != ownerID {
			return ServerAuthority{}, fmt.Errorf("%w: server already has a different authority owner", ErrAuthorityConflict)
		}
		return existing, nil
	}
	if err := requireRegisteredServerTx(ctx, tx, serverID); err != nil {
		return ServerAuthority{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO server_authority(server_id,generation,owner_id,state,updated_at) VALUES(?,?,?,?,?)`,
		string(serverID), 1, ownerID, string(AuthorityActive), FormatPersistedTime(time.Now())); err != nil {
		return ServerAuthority{}, err
	}
	if err := tx.Commit(); err != nil {
		return ServerAuthority{}, err
	}
	return ServerAuthority{ServerID: serverID, Generation: 1, Owner: ownerID, State: AuthorityActive}, nil
}

func requireRegisteredServerTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID) error {
	var registered int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM servers WHERE id=?`, string(serverID)).Scan(&registered); err != nil {
		return err
	}
	if registered == 0 {
		return errors.New("server is not registered")
	}
	return nil
}

type authorityTransitionRecord struct {
	requestDigest, frontierDigest string
	toGeneration                  uint64
}

func readAuthorityTransitionTx(ctx context.Context, tx *sql.Tx, cutoverID string, serverID contracts.ServerID, toState string) (authorityTransitionRecord, bool, error) {
	var (
		r  authorityTransitionRecord
		to int64
	)
	err := tx.QueryRowContext(ctx, `SELECT request_digest,frontier_digest,to_generation FROM server_authority_transitions WHERE cutover_id=? AND server_id=? AND to_state=?`,
		cutoverID, string(serverID), toState).Scan(&r.requestDigest, &r.frontierDigest, &to)
	if errors.Is(err, sql.ErrNoRows) {
		return authorityTransitionRecord{}, false, nil
	}
	if err != nil {
		return authorityTransitionRecord{}, false, err
	}
	r.toGeneration = uint64(to)
	return r, true, nil
}

// frameDigest writes a length-prefixed field so concatenation cannot be ambiguous.
func frameDigest(h hash.Hash, label string, fields ...string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(label)))
	h.Write(length[:])
	h.Write([]byte(label))
	for _, field := range fields {
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		h.Write(length[:])
		h.Write([]byte(field))
	}
}

// ingestFrontierDigestTx streams a deterministic digest of the durable ingest
// state of one server: every retained sample (epoch, sequence, observation time
// and values), every coverage gap and every retired epoch identifier. Receipt
// times are excluded so an exact transfer verifies even when the destination
// stamped its own receipt. The caller must hold a write transaction.
func ingestFrontierDigestTx(ctx context.Context, tx *sql.Tx, serverID contracts.ServerID) (string, error) {
	h := sha256.New()
	frameDigest(h, "payesh-ingest-frontier-v1", string(serverID))
	rows, err := tx.QueryContext(ctx, `SELECT collector_epoch,sequence,observed_at,values_json FROM metric_samples WHERE server_id=? ORDER BY collector_epoch,length(sequence),sequence`, string(serverID))
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var epoch, sequence, observed, values string
		if err := rows.Scan(&epoch, &sequence, &observed, &values); err != nil {
			_ = rows.Close()
			return "", err
		}
		frameDigest(h, "sample", epoch, sequence, observed, values)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return "", err
	}
	_ = rows.Close()
	gaps, err := tx.QueryContext(ctx, `SELECT collector_epoch,from_sequence,to_sequence,reason FROM coverage_gaps WHERE server_id=? ORDER BY collector_epoch,length(from_sequence),from_sequence,length(to_sequence),to_sequence,reason`, string(serverID))
	if err != nil {
		return "", err
	}
	for gaps.Next() {
		var epoch, from, to, reason string
		if err := gaps.Scan(&epoch, &from, &to, &reason); err != nil {
			_ = gaps.Close()
			return "", err
		}
		frameDigest(h, "gap", epoch, from, to, reason)
	}
	if err := gaps.Err(); err != nil {
		_ = gaps.Close()
		return "", err
	}
	_ = gaps.Close()
	retired, err := tx.QueryContext(ctx, `SELECT collector_epoch FROM collector_epoch_retirements WHERE server_id=? ORDER BY collector_epoch`, string(serverID))
	if err != nil {
		return "", err
	}
	for retired.Next() {
		var epoch string
		if err := retired.Scan(&epoch); err != nil {
			_ = retired.Close()
			return "", err
		}
		frameDigest(h, "retired", epoch)
	}
	if err := retired.Err(); err != nil {
		_ = retired.Close()
		return "", err
	}
	_ = retired.Close()
	return hex.EncodeToString(h.Sum(nil)), nil
}

// IngestFrontierDigest reports the current ingest-state digest of a server. A
// frozen or relinquished server's digest is stable, so a destination can verify
// that it holds exactly the state the source froze.
func (s *Store) IngestFrontierDigest(ctx context.Context, serverID contracts.ServerID) (string, error) {
	if !validAuthorityServerID(serverID) {
		return "", errors.New("server_id must be a bounded URL-safe identifier")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	return ingestFrontierDigestTx(ctx, tx, serverID)
}

// FreezeServerAuthority atomically stops ingest for the server and records the
// final frontier digest. The same cutover and request may be replayed; any
// other freeze of a non-active server is a conflict.
func (s *Store) FreezeServerAuthority(ctx context.Context, request AuthorityTransition) (ServerAuthority, error) {
	return s.transitionServerAuthority(ctx, request, AuthorityActive, AuthorityFrozen)
}

// RelinquishServerAuthority records that the cutover handed ownership to the
// destination. It requires the exact freeze performed by the same cutover.
func (s *Store) RelinquishServerAuthority(ctx context.Context, request AuthorityTransition) (ServerAuthority, error) {
	return s.transitionServerAuthority(ctx, request, AuthorityFrozen, AuthorityRelinquished)
}

func (s *Store) transitionServerAuthority(ctx context.Context, request AuthorityTransition, from, to AuthorityState) (ServerAuthority, error) {
	if err := request.validate(); err != nil {
		return ServerAuthority{}, err
	}
	if err := s.ensureWritable(ctx); err != nil {
		return ServerAuthority{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ServerAuthority{}, err
	}
	defer tx.Rollback()
	current, found, err := readServerAuthorityTx(ctx, tx, request.ServerID)
	if err != nil {
		return ServerAuthority{}, err
	}
	if !found {
		return ServerAuthority{}, fmt.Errorf("%w: server authority is not initialized", ErrAuthorityConflict)
	}
	if recorded, replay, err := readAuthorityTransitionTx(ctx, tx, request.CutoverID, request.ServerID, string(to)); err != nil {
		return ServerAuthority{}, err
	} else if replay {
		if current.CutoverID != request.CutoverID {
			return ServerAuthority{}, fmt.Errorf("%w: this cutover was aborted and its identifier cannot be reused", ErrAuthorityConflict)
		}
		if recorded.requestDigest != request.RequestDigest || (request.FrontierDigest != "" && recorded.frontierDigest != request.FrontierDigest) {
			return ServerAuthority{}, fmt.Errorf("%w: cutover already used with a different request", ErrAuthorityConflict)
		}
		return ServerAuthority{ServerID: request.ServerID, Generation: recorded.toGeneration, Owner: current.Owner, State: to, CutoverID: request.CutoverID, TransitionDigest: recorded.requestDigest, FrontierDigest: recorded.frontierDigest}, nil
	}
	if current.State != from {
		return ServerAuthority{}, fmt.Errorf("%w: server authority is %s, not %s", ErrAuthorityConflict, current.State, from)
	}
	frontier := request.FrontierDigest
	switch to {
	case AuthorityFrozen:
		// Computed in this write transaction, so no ingest can slip between the
		// digest and the state change.
		if frontier == "" {
			if frontier, err = ingestFrontierDigestTx(ctx, tx, request.ServerID); err != nil {
				return ServerAuthority{}, err
			}
		}
	case AuthorityRelinquished:
		frozen, ok, err := readAuthorityTransitionTx(ctx, tx, request.CutoverID, request.ServerID, string(AuthorityFrozen))
		if err != nil {
			return ServerAuthority{}, err
		}
		if frontier == "" {
			frontier = frozen.frontierDigest
		}
		if !ok || current.CutoverID != request.CutoverID || frozen.requestDigest != request.RequestDigest || frozen.frontierDigest != frontier {
			return ServerAuthority{}, fmt.Errorf("%w: relinquish does not match the freeze for this cutover", ErrAuthorityConflict)
		}
	}
	if current.Generation >= math.MaxInt64 {
		return ServerAuthority{}, ErrAuthorityGenerationExhausted
	}
	next := current.Generation + 1
	now := FormatPersistedTime(time.Now())
	if _, err := tx.ExecContext(ctx, `UPDATE server_authority SET generation=?,state=?,cutover_id=?,transition_digest=?,updated_at=? WHERE server_id=? AND generation=?`,
		int64(next), string(to), request.CutoverID, request.RequestDigest, now, string(request.ServerID), int64(current.Generation)); err != nil {
		return ServerAuthority{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO server_authority_transitions(cutover_id,server_id,to_state,request_digest,frontier_digest,from_generation,to_generation,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		request.CutoverID, string(request.ServerID), string(to), request.RequestDigest, frontier, int64(current.Generation), int64(next), now); err != nil {
		return ServerAuthority{}, err
	}
	if err := tx.Commit(); err != nil {
		return ServerAuthority{}, err
	}
	return ServerAuthority{ServerID: request.ServerID, Generation: next, Owner: current.Owner, State: to, CutoverID: request.CutoverID, TransitionDigest: request.RequestDigest, FrontierDigest: frontier}, nil
}

// AbortServerFreeze resumes ingest for a frozen server whose cutover failed
// before the destination took over. Authority stays monotonic: the generation
// advances, the record becomes active again with no cutover attached, and the
// aborted cutover identifier is burned, so a later attempt needs a new one. A
// relinquished server cannot be resumed. An exact replay is idempotent.
func (s *Store) AbortServerFreeze(ctx context.Context, request AuthorityTransition) (ServerAuthority, error) {
	if err := request.validate(); err != nil {
		return ServerAuthority{}, err
	}
	if err := s.ensureWritable(ctx); err != nil {
		return ServerAuthority{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ServerAuthority{}, err
	}
	defer tx.Rollback()
	current, found, err := readServerAuthorityTx(ctx, tx, request.ServerID)
	if err != nil {
		return ServerAuthority{}, err
	}
	if !found {
		return ServerAuthority{}, fmt.Errorf("%w: server authority is not initialized", ErrAuthorityConflict)
	}
	frozen, hadFreeze, err := readAuthorityTransitionTx(ctx, tx, request.CutoverID, request.ServerID, string(AuthorityFrozen))
	if err != nil {
		return ServerAuthority{}, err
	}
	if !hadFreeze || frozen.requestDigest != request.RequestDigest {
		return ServerAuthority{}, fmt.Errorf("%w: no matching freeze exists for this cutover", ErrAuthorityConflict)
	}
	if recorded, replay, err := readAuthorityTransitionTx(ctx, tx, request.CutoverID, request.ServerID, "aborted"); err != nil {
		return ServerAuthority{}, err
	} else if replay {
		return ServerAuthority{ServerID: request.ServerID, Generation: recorded.toGeneration, Owner: current.Owner, State: AuthorityActive}, nil
	}
	if current.State != AuthorityFrozen || current.CutoverID != request.CutoverID {
		return ServerAuthority{}, fmt.Errorf("%w: server authority is %s for another state of this cutover", ErrAuthorityConflict, current.State)
	}
	if current.Generation >= math.MaxInt64 {
		return ServerAuthority{}, ErrAuthorityGenerationExhausted
	}
	next := current.Generation + 1
	now := FormatPersistedTime(time.Now())
	if _, err := tx.ExecContext(ctx, `UPDATE server_authority SET generation=?,state=?,cutover_id='',transition_digest='',updated_at=? WHERE server_id=? AND generation=?`,
		int64(next), string(AuthorityActive), now, string(request.ServerID), int64(current.Generation)); err != nil {
		return ServerAuthority{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO server_authority_transitions(cutover_id,server_id,to_state,request_digest,frontier_digest,from_generation,to_generation,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		request.CutoverID, string(request.ServerID), "aborted", request.RequestDigest, frozen.frontierDigest, int64(current.Generation), int64(next), now); err != nil {
		return ServerAuthority{}, err
	}
	if err := tx.Commit(); err != nil {
		return ServerAuthority{}, err
	}
	return ServerAuthority{ServerID: request.ServerID, Generation: next, Owner: current.Owner, State: AuthorityActive}, nil
}

// ActivateServerAuthority makes this store the active owner of a server that a
// source relinquished. It verifies, inside the activation transaction, that the
// state this store holds hashes to exactly the frontier the source froze; any
// missing or extra data refuses the activation and leaves no authority behind.
// The new generation is the source's final generation plus one, so authority
// stays monotonic across stores. An exact replay is idempotent; any other
// existing authority record for the server is a conflict.
func (s *Store) ActivateServerAuthority(ctx context.Context, handoff AuthorityHandoff) (ServerAuthority, error) {
	if err := handoff.validate(); err != nil {
		return ServerAuthority{}, err
	}
	if err := s.ensureWritable(ctx); err != nil {
		return ServerAuthority{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ServerAuthority{}, err
	}
	defer tx.Rollback()
	if err := requireRegisteredServerTx(ctx, tx, handoff.ServerID); err != nil {
		return ServerAuthority{}, err
	}
	bound := handoff.digest()
	existing, found, err := readServerAuthorityTx(ctx, tx, handoff.ServerID)
	if err != nil {
		return ServerAuthority{}, err
	}
	if found {
		if existing.State == AuthorityActive && existing.CutoverID == handoff.CutoverID && existing.Owner == handoff.NewOwner && existing.TransitionDigest == bound {
			return existing, nil
		}
		return ServerAuthority{}, fmt.Errorf("%w: this store already has authority state for the server", ErrAuthorityConflict)
	}
	local, err := ingestFrontierDigestTx(ctx, tx, handoff.ServerID)
	if err != nil {
		return ServerAuthority{}, err
	}
	if local != handoff.FrontierDigest {
		return ServerAuthority{}, fmt.Errorf("%w: the transferred state does not match the source freeze", ErrAuthorityConflict)
	}
	generation := handoff.SourceGeneration + 1
	now := FormatPersistedTime(time.Now())
	if _, err := tx.ExecContext(ctx, `INSERT INTO server_authority(server_id,generation,owner_id,state,cutover_id,transition_digest,updated_at) VALUES(?,?,?,?,?,?,?)`,
		string(handoff.ServerID), int64(generation), handoff.NewOwner, string(AuthorityActive), handoff.CutoverID, bound, now); err != nil {
		return ServerAuthority{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO server_authority_transitions(cutover_id,server_id,to_state,request_digest,frontier_digest,from_generation,to_generation,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		handoff.CutoverID, string(handoff.ServerID), "activated", bound, handoff.FrontierDigest, int64(handoff.SourceGeneration), int64(generation), now); err != nil {
		return ServerAuthority{}, err
	}
	if err := tx.Commit(); err != nil {
		return ServerAuthority{}, err
	}
	return ServerAuthority{ServerID: handoff.ServerID, Generation: generation, Owner: handoff.NewOwner, State: AuthorityActive, CutoverID: handoff.CutoverID, TransitionDigest: bound, FrontierDigest: handoff.FrontierDigest}, nil
}
