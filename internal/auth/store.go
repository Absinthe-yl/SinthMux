package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var ErrDenied = errors.New("access denied")

type Store struct{ DB *sql.DB }

type User struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	GithubID int64  `json:"githubId,omitempty"`
}

type Space struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	Role string `json:"role"`
}

type Session struct {
	User   User
	CSRF   string
	IDHash []byte
}

type Device struct {
	ID      string `json:"id"`
	SpaceID string `json:"spaceId"`
	Name    string `json:"name"`
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{DB: db}
	if err = s.Migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(768172395)`); err != nil {
		return err
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS users (id text PRIMARY KEY, name text NOT NULL, github_id bigint UNIQUE, status text NOT NULL DEFAULT 'active', created_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS spaces (id text PRIMARY KEY, name text NOT NULL, kind text NOT NULL CHECK (kind IN ('personal','team')), owner_user_id text NOT NULL REFERENCES users(id), created_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE UNIQUE INDEX IF NOT EXISTS personal_space_owner ON spaces(owner_user_id) WHERE kind = 'personal'`,
		`CREATE TABLE IF NOT EXISTS memberships (space_id text NOT NULL REFERENCES spaces(id) ON DELETE CASCADE, user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE, role text NOT NULL CHECK (role IN ('owner','admin','operator','viewer')), PRIMARY KEY (space_id,user_id))`,
		`CREATE TABLE IF NOT EXISTS login_tokens (id text PRIMARY KEY, user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE, name text NOT NULL, secret_hash bytea NOT NULL, expires_at timestamptz NOT NULL, revoked_at timestamptz, created_at timestamptz NOT NULL DEFAULT now(), last_used_at timestamptz)`,
		`CREATE TABLE IF NOT EXISTS web_sessions (secret_hash bytea PRIMARY KEY, user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE, source_token_id text REFERENCES login_tokens(id), csrf text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), last_used_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL, revoked_at timestamptz)`,
		`CREATE TABLE IF NOT EXISTS devices (id text PRIMARY KEY, space_id text NOT NULL REFERENCES spaces(id) ON DELETE CASCADE, name text NOT NULL, secret_hash bytea NOT NULL, revoked_at timestamptz, created_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS audit_events (id bigserial PRIMARY KEY, actor_user_id text, space_id text, target text, action text NOT NULL, result text NOT NULL, created_at timestamptz NOT NULL DEFAULT now())`,
	}
	for _, query := range statements {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func Random() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func ID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func digest(secret string) []byte { value := sha256.Sum256([]byte(secret)); return value[:] }
func matches(stored []byte, secret string) bool {
	return subtle.ConstantTimeCompare(stored, digest(secret)) == 1
}

func (s *Store) GithubUser(ctx context.Context, githubID int64, name string) (User, error) {
	if githubID <= 0 {
		return User{}, ErrDenied
	}
	id, err := ID()
	if err != nil {
		return User{}, err
	}
	var user User
	err = s.DB.QueryRowContext(ctx, `INSERT INTO users(id,name,github_id) VALUES($1,$2,$3) ON CONFLICT(github_id) DO UPDATE SET name=excluded.name RETURNING id,name,status`, id, name, githubID).Scan(&user.ID, &user.Name, &user.Status)
	if err != nil || user.Status != "active" {
		return User{}, ErrDenied
	}
	user.GithubID = githubID
	if err = s.ensurePersonalSpace(ctx, user); err != nil {
		return User{}, err
	}
	return user, nil
}

func (s *Store) ensurePersonalSpace(ctx context.Context, user User) error {
	id, err := ID()
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO spaces(id,name,kind,owner_user_id) VALUES($1,$2,'personal',$3) ON CONFLICT DO NOTHING`, id, user.Name+" 的空间", user.ID)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO memberships(space_id,user_id,role) SELECT id,$1,'owner' FROM spaces WHERE owner_user_id=$1 AND kind='personal' ON CONFLICT DO NOTHING`, user.ID)
	return err
}

func (s *Store) NewToken(ctx context.Context, userID, name string, duration time.Duration) (string, error) {
	id, err := ID()
	if err != nil {
		return "", err
	}
	secret, err := Random()
	if err != nil {
		return "", err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO login_tokens(id,user_id,name,secret_hash,expires_at) VALUES($1,$2,$3,$4,$5)`, id, userID, name, digest(secret), time.Now().Add(duration))
	if err != nil {
		return "", err
	}
	return "smt_" + id + "_" + secret, nil
}

func (s *Store) TokenUser(ctx context.Context, token string) (User, string, error) {
	parts := strings.Split(token, "_")
	if len(parts) != 3 || parts[0] != "smt" || len(parts[1]) != 32 || len(parts[2]) != 64 {
		return User{}, "", ErrDenied
	}
	var u User
	var stored []byte
	err := s.DB.QueryRowContext(ctx, `SELECT u.id,u.name,u.status,coalesce(u.github_id,0),t.secret_hash FROM login_tokens t JOIN users u ON u.id=t.user_id WHERE t.id=$1 AND t.revoked_at IS NULL AND t.expires_at>now()`, parts[1]).Scan(&u.ID, &u.Name, &u.Status, &u.GithubID, &stored)
	if err != nil || u.Status != "active" || !matches(stored, parts[2]) {
		return User{}, "", ErrDenied
	}
	if err = s.ensurePersonalSpace(ctx, u); err != nil {
		return User{}, "", err
	}
	_, _ = s.DB.ExecContext(ctx, `UPDATE login_tokens SET last_used_at=now() WHERE id=$1`, parts[1])
	return u, parts[1], nil
}

func (s *Store) NewSession(ctx context.Context, userID, sourceTokenID string) (string, string, error) {
	secret, err := Random()
	if err != nil {
		return "", "", err
	}
	csrf, err := Random()
	if err != nil {
		return "", "", err
	}
	if sourceTokenID == "" {
		_, err = s.DB.ExecContext(ctx, `INSERT INTO web_sessions(secret_hash,user_id,csrf,expires_at) VALUES($1,$2,$3,now()+interval '7 days')`, digest(secret), userID, csrf)
		return secret, csrf, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	var owner string
	if err = tx.QueryRowContext(ctx, `SELECT user_id FROM login_tokens WHERE id=$1 AND revoked_at IS NULL AND expires_at>now() FOR UPDATE`, sourceTokenID).Scan(&owner); err != nil || owner != userID {
		return "", "", ErrDenied
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO web_sessions(secret_hash,user_id,source_token_id,csrf,expires_at) VALUES($1,$2,$3,$4,now()+interval '7 days')`, digest(secret), userID, sourceTokenID, csrf); err != nil {
		return "", "", err
	}
	if err = tx.Commit(); err != nil {
		return "", "", err
	}
	return secret, csrf, nil
}

func (s *Store) Session(ctx context.Context, secret string) (Session, error) {
	var result Session
	if len(secret) != 64 {
		return result, ErrDenied
	}
	result.IDHash = digest(secret)
	err := s.DB.QueryRowContext(ctx, `SELECT u.id,u.name,u.status,coalesce(u.github_id,0),w.csrf FROM web_sessions w JOIN users u ON u.id=w.user_id WHERE w.secret_hash=$1 AND w.revoked_at IS NULL AND w.expires_at>now() AND w.last_used_at>now()-interval '12 hours'`, result.IDHash).Scan(&result.User.ID, &result.User.Name, &result.User.Status, &result.User.GithubID, &result.CSRF)
	if err != nil || result.User.Status != "active" {
		return Session{}, ErrDenied
	}
	_, _ = s.DB.ExecContext(ctx, `UPDATE web_sessions SET last_used_at=now() WHERE secret_hash=$1`, result.IDHash)
	return result, nil
}

func (s *Store) RevokeSession(ctx context.Context, hash []byte) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE web_sessions SET revoked_at=now() WHERE secret_hash=$1`, hash)
	return err
}

func (s *Store) RevokeToken(ctx context.Context, userID, tokenID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE login_tokens SET revoked_at=now() WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL`, tokenID, userID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	_, err = tx.ExecContext(ctx, `UPDATE web_sessions SET revoked_at=now() WHERE source_token_id=$1`, tokenID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) BootstrapOwner(ctx context.Context, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", ErrDenied
	}
	userID, err := ID()
	if err != nil {
		return "", err
	}
	spaceID, err := ID()
	if err != nil {
		return "", err
	}
	tokenID, err := ID()
	if err != nil {
		return "", err
	}
	secret, err := Random()
	if err != nil {
		return "", err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(768172394)`); err != nil {
		return "", err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		return "", err
	}
	if count != 0 {
		return "", ErrDenied
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO users(id,name) VALUES($1,$2)`, userID, name); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO spaces(id,name,kind,owner_user_id) VALUES($1,$2,'personal',$3)`, spaceID, name+" 的空间", userID); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO memberships(space_id,user_id,role) VALUES($1,$2,'owner')`, spaceID, userID); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO login_tokens(id,user_id,name,secret_hash,expires_at) VALUES($1,$2,'初始登录令牌',$3,now()+interval '1 day')`, tokenID, userID, digest(secret)); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return "smt_" + tokenID + "_" + secret, nil
}

func (s *Store) Tokens(ctx context.Context, userID string) ([]map[string]any, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,name,expires_at,created_at,last_used_at FROM login_tokens WHERE user_id=$1 AND revoked_at IS NULL ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		var id, name string
		var expires, created time.Time
		var used sql.NullTime
		if err := rows.Scan(&id, &name, &expires, &created, &used); err != nil {
			return nil, err
		}
		item := map[string]any{"id": id, "name": name, "expiresAt": expires, "createdAt": created}
		if used.Valid {
			item["lastUsedAt"] = used.Time
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) Spaces(ctx context.Context, userID string) ([]Space, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT s.id,s.name,s.kind,m.role FROM spaces s JOIN memberships m ON m.space_id=s.id WHERE m.user_id=$1 ORDER BY s.kind,s.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Space{}
	for rows.Next() {
		var x Space
		if err := rows.Scan(&x.ID, &x.Name, &x.Kind, &x.Role); err != nil {
			return nil, err
		}
		result = append(result, x)
	}
	return result, rows.Err()
}

func (s *Store) Role(ctx context.Context, userID, spaceID string) (string, error) {
	var role string
	err := s.DB.QueryRowContext(ctx, `SELECT m.role FROM memberships m JOIN users u ON u.id=m.user_id WHERE m.user_id=$1 AND m.space_id=$2 AND u.status='active'`, userID, spaceID).Scan(&role)
	if err != nil {
		return "", ErrDenied
	}
	return role, nil
}

func Allowed(role, action string) bool {
	switch action {
	case "list":
		return role == "owner" || role == "admin" || role == "operator" || role == "viewer"
	case "input", "create", "rename":
		return role == "owner" || role == "admin" || role == "operator"
	case "close", "device", "audit":
		return role == "owner" || role == "admin"
	case "member", "space":
		return role == "owner"
	default:
		return false
	}
}

func (s *Store) DeviceRole(ctx context.Context, userID, deviceID string) (string, string, error) {
	var spaceID, role string
	err := s.DB.QueryRowContext(ctx, `SELECT d.space_id,m.role FROM devices d JOIN memberships m ON m.space_id=d.space_id AND m.user_id=$1 WHERE d.id=$2 AND d.revoked_at IS NULL`, userID, deviceID).Scan(&spaceID, &role)
	if err != nil {
		return "", "", ErrDenied
	}
	return spaceID, role, nil
}

func (s *Store) Devices(ctx context.Context, userID string) (map[string]Device, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT d.id,d.space_id,d.name FROM devices d JOIN memberships m ON m.space_id=d.space_id WHERE m.user_id=$1 AND d.revoked_at IS NULL`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]Device{}
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.SpaceID, &d.Name); err != nil {
			return nil, err
		}
		result[d.ID] = d
	}
	return result, rows.Err()
}

func (s *Store) NewDevice(ctx context.Context, spaceID, name string) (Device, string, error) {
	id, err := ID()
	if err != nil {
		return Device{}, "", err
	}
	secret, err := Random()
	if err != nil {
		return Device{}, "", err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO devices(id,space_id,name,secret_hash) VALUES($1,$2,$3,$4)`, id, spaceID, name, digest(secret))
	return Device{ID: id, SpaceID: spaceID, Name: name}, "smd_" + id + "_" + secret, err
}

func (s *Store) AuthenticateDevice(ctx context.Context, deviceID, token string) bool {
	parts := strings.Split(token, "_")
	if len(parts) != 3 || parts[0] != "smd" || parts[1] != deviceID || len(parts[2]) != 64 {
		return false
	}
	var stored []byte
	if s.DB.QueryRowContext(ctx, `SELECT secret_hash FROM devices WHERE id=$1 AND revoked_at IS NULL`, deviceID).Scan(&stored) != nil {
		return false
	}
	return matches(stored, parts[2])
}

func (s *Store) CreateSpace(ctx context.Context, userID, name string) (Space, error) {
	id, err := ID()
	if err != nil {
		return Space{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Space{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO spaces(id,name,kind,owner_user_id) VALUES($1,$2,'team',$3)`, id, name, userID); err != nil {
		return Space{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO memberships(space_id,user_id,role) VALUES($1,$2,'owner')`, id, userID); err != nil {
		return Space{}, err
	}
	if err = tx.Commit(); err != nil {
		return Space{}, err
	}
	return Space{ID: id, Name: name, Kind: "team", Role: "owner"}, nil
}

func (s *Store) AddMember(ctx context.Context, spaceID, name, role string) (User, error) {
	if role != "admin" && role != "operator" && role != "viewer" {
		return User{}, ErrDenied
	}
	id, err := ID()
	if err != nil {
		return User{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO users(id,name) VALUES($1,$2)`, id, name); err != nil {
		return User{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO memberships(space_id,user_id,role) VALUES($1,$2,$3)`, spaceID, id, role); err != nil {
		return User{}, err
	}
	if err = tx.Commit(); err != nil {
		return User{}, err
	}
	return User{ID: id, Name: name, Status: "active"}, nil
}

type Member struct {
	UserID string `json:"userId"`
	Name   string `json:"name"`
	Role   string `json:"role"`
}

func (s *Store) Members(ctx context.Context, spaceID string) ([]Member, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT u.id,u.name,m.role FROM memberships m JOIN users u ON u.id=m.user_id WHERE m.space_id=$1 ORDER BY m.role,u.name`, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Name, &m.Role); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

func (s *Store) AddGithubMember(ctx context.Context, spaceID string, githubID int64, role string) (User, error) {
	if githubID <= 0 || role == "owner" || !Allowed(role, "list") {
		return User{}, ErrDenied
	}
	var user User
	err := s.DB.QueryRowContext(ctx, `SELECT id,name,status FROM users WHERE github_id=$1`, githubID).Scan(&user.ID, &user.Name, &user.Status)
	if err != nil || user.Status != "active" {
		return User{}, ErrDenied
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO memberships(space_id,user_id,role) VALUES($1,$2,$3)`, spaceID, user.ID, role)
	return user, err
}

func (s *Store) ChangeMemberRole(ctx context.Context, spaceID, userID, role string) error {
	if role == "owner" || !Allowed(role, "list") {
		return ErrDenied
	}
	result, err := s.DB.ExecContext(ctx, `UPDATE memberships SET role=$3 WHERE space_id=$1 AND user_id=$2 AND role<>'owner'`, spaceID, userID, role)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrDenied
	}
	return nil
}

func (s *Store) RemoveMember(ctx context.Context, spaceID, userID string) error {
	result, err := s.DB.ExecContext(ctx, `DELETE FROM memberships WHERE space_id=$1 AND user_id=$2 AND role<>'owner'`, spaceID, userID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrDenied
	}
	return nil
}

func (s *Store) RevokeDevice(ctx context.Context, spaceID, deviceID string) error {
	result, err := s.DB.ExecContext(ctx, `UPDATE devices SET revoked_at=now() WHERE space_id=$1 AND id=$2 AND revoked_at IS NULL`, spaceID, deviceID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrDenied
	}
	return nil
}

func (s *Store) Audit(ctx context.Context, actor, space, target, action, result string) {
	_, _ = s.DB.ExecContext(ctx, `INSERT INTO audit_events(actor_user_id,space_id,target,action,result) VALUES($1,$2,$3,$4,$5)`, actor, space, target, action, result)
}

type AuditEvent struct {
	ActorUserID string    `json:"actorUserId"`
	Target      string    `json:"target"`
	Action      string    `json:"action"`
	Result      string    `json:"result"`
	CreatedAt   time.Time `json:"createdAt"`
}

func (s *Store) AuditEvents(ctx context.Context, spaceID string) ([]AuditEvent, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT coalesce(actor_user_id,''),coalesce(target,''),action,result,created_at FROM audit_events WHERE space_id=$1 ORDER BY id DESC LIMIT 100`, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []AuditEvent{}
	for rows.Next() {
		var e AuditEvent
		if err := rows.Scan(&e.ActorUserID, &e.Target, &e.Action, &e.Result, &e.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

func (s *Store) TerminalAllowed(ctx context.Context, userID, deviceID, spaceID string, sessionHash []byte) bool {
	var role string
	err := s.DB.QueryRowContext(ctx, `SELECT m.role FROM web_sessions w JOIN users u ON u.id=w.user_id JOIN memberships m ON m.user_id=u.id JOIN devices d ON d.space_id=m.space_id WHERE w.secret_hash=$1 AND w.user_id=$2 AND d.id=$3 AND d.space_id=$4 AND w.revoked_at IS NULL AND w.expires_at>now() AND w.last_used_at>now()-interval '12 hours' AND u.status='active' AND d.revoked_at IS NULL`, sessionHash, userID, deviceID, spaceID).Scan(&role)
	return err == nil && Allowed(role, "input")
}
