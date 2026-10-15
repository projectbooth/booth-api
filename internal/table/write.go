package table

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/projectbooth/booth-api/internal/apidef"
)

// Writes (ADR 0117, docs/decisions/0008): one row per request, one statement per write, every
// identifier from the stored snapshot and every value a bound parameter cast to its column's type,
// exactly as reads. Which operations and columns a caller may use is decided by the caller of
// Write (internal/writes); this file validates the row against the snapshot and runs it.

// Write error codes, shared by REST and GraphQL.
const (
	CodeInvalidRequest       = "invalid_request"
	CodeOperationNotAllowed  = "operation_not_allowed"
	CodeNotFound             = "not_found"
	CodeConflict             = "conflict"
	CodeDefinitionStale      = "definition_stale"
	CodeVersionMismatch      = "version_mismatch"
	CodeInvalidValue         = "invalid_value"
	CodeUnknownColumn        = "unknown_column"
	CodeColumnNotWritable    = "column_not_writable"
	CodeRequired             = "required"
	CodeConstraint           = "constraint"
	CodeIdempotencyMismatch  = "idempotency_mismatch"
	CodeIdempotencyInFlight  = "idempotency_in_progress"
	CodeIdempotencyUnknown   = "idempotency_outcome_unknown"
	CodeRateLimited          = "rate_limited"
	CodeUnavailable          = "unavailable"
	CodeOwnerNoAccess        = "granting_owner_no_access"
	CodeWriteNotGranted      = "not_granted"
	CodeNoWritableColumns    = "no_writable_columns"
	CodeSnapshotNeedsRefresh = "regenerate_required"
)

// FieldProblem is one validation failure, by API field name. It never carries a value.
type FieldProblem struct {
	Field   string `json:"field"`
	Problem string `json:"problem"`
}

// WriteError is a refused or failed write, safe to show the caller: it never holds row values or
// database text beyond a constraint's name.
type WriteError struct {
	Status     int            `json:"-"`
	Code       string         `json:"code"`
	Msg        string         `json:"message"`
	Fields     []FieldProblem `json:"fields,omitempty"`
	Constraint string         `json:"constraint,omitempty"`
}

func (e *WriteError) Error() string { return e.Msg }

func writeErr(status int, code, format string, args ...any) *WriteError {
	return &WriteError{Status: status, Code: code, Msg: fmt.Sprintf(format, args...)}
}

var (
	// ErrInsufficientPrivilege: Postgres refused the write lease (SQLSTATE 42501). The lease's
	// group may lag the settings; the caller reissues the full scope once and retries.
	ErrInsufficientPrivilege = errors.New("the write lease lacks a privilege this write needs")
	// ErrOutcomeUnknown: the connection failed at COMMIT, so the write may or may not have landed.
	ErrOutcomeUnknown = errors.New("the database connection failed while committing; the write may or may not have been applied")
)

// WriteRequest is one row write.
type WriteRequest struct {
	Op string // apidef.OpInsert, OpUpdate or OpDelete
	// Key names the row to update or delete: every primary-key field, by API field name.
	Key map[string]any
	// Data is the values to insert or set, by API field name.
	Data map[string]any
	// ExpectedVersion, if set, must equal the row's current version (optimistic concurrency).
	ExpectedVersion string
	// Writable is the set of API field names the caller may set.
	Writable map[string]bool
}

// WriteResult is a written row: every field (as a read returns it), its version, and its primary
// key as text (for the audit log's key hash; never logged itself).
type WriteResult struct {
	Row     Row
	Version string
	KeyText []string
}

// versionExpr is a row's version: SHA-256 of its whole jsonb rendering, so any change to any
// column (exposed or not) changes it. An app reading the table directly computes the same value
// with this expression.
const versionExpr = `encode(sha256(convert_to(to_jsonb(_booth_row.*)::text, 'UTF8')), 'hex')`

const rowAlias = "_booth_row"

// WritableCandidate reports whether f may be made writable at all, and if not, why. Arrays,
// generated columns, GENERATED ALWAYS identity columns, and primary-key columns the database fills
// in (a default or an identity) are never writable.
func (m Model) WritableCandidate(f Field) (bool, string) {
	c, ok := m.column(f.Column)
	switch {
	case !ok:
		return false, "not in the snapshot"
	case f.List:
		return false, "arrays can't be written through the API"
	case c.Generated:
		return false, "a generated column"
	case c.Identity == "a":
		return false, "an identity column (GENERATED ALWAYS)"
	case m.isPK(f.Column) && (c.HasDefault || c.Identity != ""):
		return false, "a primary-key column the database fills in"
	}
	return true, ""
}

// WritableCandidates lists the columns (real names) WritableCandidate accepts, in column order.
func (m Model) WritableCandidates() []string {
	out := []string{}
	for _, f := range m.Fields {
		if ok, _ := m.WritableCandidate(f); ok {
			out = append(out, f.Column)
		}
	}
	return out
}

// RequiredOnInsert lists the columns (real names) an insert must set: NOT NULL with no default,
// not identity, not generated. Columns the API omits (unmappable types) are included, since an
// insert can then never succeed.
func (m Model) RequiredOnInsert() []string {
	var out []string
	for _, c := range m.Def.Columns {
		if !c.Nullable && !c.HasDefault && c.Identity == "" && !c.Generated {
			out = append(out, c.Name)
		}
	}
	return out
}

// FieldForColumn returns the field exposing a column.
func (m Model) FieldForColumn(column string) (Field, bool) {
	for _, f := range m.Fields {
		if f.Column == column {
			return f, true
		}
	}
	return Field{}, false
}

func (m Model) column(name string) (apidef.Column, bool) {
	for _, c := range m.Def.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return apidef.Column{}, false
}

func (m Model) isPK(column string) bool {
	for _, k := range m.Def.PrimaryKey {
		if k == column {
			return true
		}
	}
	return false
}

// Write validates req against the snapshot and runs it as one statement in one transaction.
func Write(ctx context.Context, db DB, m Model, req WriteRequest, timeout time.Duration) (WriteResult, error) {
	q := &sqlQuery{}
	var stmt string
	var err error
	switch req.Op {
	case apidef.OpInsert:
		stmt, err = m.insertSQL(q, req)
	case apidef.OpUpdate:
		stmt, err = m.updateSQL(q, req)
	case apidef.OpDelete:
		stmt, err = m.deleteSQL(q, req)
	default:
		return WriteResult{}, writeErr(400, CodeInvalidRequest, "unknown operation %q", req.Op)
	}
	if err != nil {
		return WriteResult{}, err
	}

	var res WriteResult
	var found bool
	err = withWriteTx(ctx, db, timeout, func(tx pgx.Tx) error {
		res, found, err = m.runReturning(ctx, tx, stmt, q.args)
		if err != nil || found || req.Op == apidef.OpInsert {
			return err
		}
		// Nothing matched: tell "no such row" from "the row changed" (only possible with a version).
		if req.ExpectedVersion != "" {
			kq := &sqlQuery{}
			cond, kerr := m.keyCondition(kq, req.Key)
			if kerr != nil {
				return kerr
			}
			var exists bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+quote(m.Def.Table.Schema, m.Def.Table.Name)+" WHERE "+cond+")", kq.args...).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return writeErr(412, CodeVersionMismatch, "the row has changed since that version; read it again and retry")
			}
		}
		return writeErr(404, CodeNotFound, "no row with that key")
	})
	if err != nil {
		return WriteResult{}, mapWriteError(err, timeout)
	}
	for _, f := range m.PK {
		res.KeyText = append(res.KeyText, string(res.Row[f.Name]))
	}
	return res, nil
}

func (m Model) insertSQL(q *sqlQuery, req WriteRequest) (string, error) {
	if len(req.Key) > 0 {
		return "", writeErr(400, CodeInvalidRequest, "an insert takes no key")
	}
	cols, vals, problems := m.assignments(q, req)
	present := map[string]bool{}
	for _, c := range cols {
		present[c] = true
	}
	for _, c := range m.RequiredOnInsert() {
		if !present[c] {
			name := c
			if f, ok := m.FieldForColumn(c); ok {
				name = f.Name
			}
			problems = append(problems, FieldProblem{Field: name, Problem: CodeRequired})
		}
	}
	if len(problems) > 0 {
		return "", validationErr(problems)
	}
	target := quote(m.Def.Table.Schema, m.Def.Table.Name) + " AS " + rowAlias
	if len(cols) == 0 {
		return "INSERT INTO " + target + " DEFAULT VALUES RETURNING " + m.returning(), nil
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = quote(c)
	}
	return "INSERT INTO " + target + " (" + strings.Join(quoted, ", ") + ") VALUES (" + strings.Join(vals, ", ") + ") RETURNING " + m.returning(), nil
}

func (m Model) updateSQL(q *sqlQuery, req WriteRequest) (string, error) {
	if len(req.Data) == 0 {
		return "", writeErr(400, CodeInvalidRequest, "an update needs at least one field to set")
	}
	cols, vals, problems := m.assignments(q, req)
	if len(problems) > 0 {
		return "", validationErr(problems)
	}
	cond, err := m.keyCondition(q, req.Key)
	if err != nil {
		return "", err
	}
	sets := make([]string, len(cols))
	for i, c := range cols {
		sets[i] = quote(c) + " = " + vals[i]
	}
	return "UPDATE " + quote(m.Def.Table.Schema, m.Def.Table.Name) + " AS " + rowAlias +
		" SET " + strings.Join(sets, ", ") + " WHERE " + cond + m.versionCondition(q, req) + " RETURNING " + m.returning(), nil
}

func (m Model) deleteSQL(q *sqlQuery, req WriteRequest) (string, error) {
	if len(req.Data) > 0 {
		return "", writeErr(400, CodeInvalidRequest, "a delete takes no data")
	}
	cond, err := m.keyCondition(q, req.Key)
	if err != nil {
		return "", err
	}
	return "DELETE FROM " + quote(m.Def.Table.Schema, m.Def.Table.Name) + " AS " + rowAlias +
		" WHERE " + cond + m.versionCondition(q, req) + " RETURNING " + m.returning(), nil
}

// assignments validates req.Data and binds each value: columns (real names) and the matching
// bound expressions, in field-name order so the SQL text is stable.
func (m Model) assignments(q *sqlQuery, req WriteRequest) (cols, vals []string, problems []FieldProblem) {
	names := make([]string, 0, len(req.Data))
	for n := range req.Data {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f, ok := m.ByName[n]
		if !ok {
			problems = append(problems, FieldProblem{Field: n, Problem: CodeUnknownColumn})
			continue
		}
		if !req.Writable[n] {
			problems = append(problems, FieldProblem{Field: n, Problem: CodeColumnNotWritable})
			continue
		}
		v := req.Data[n]
		if v == nil {
			if !f.Nullable {
				problems = append(problems, FieldProblem{Field: n, Problem: CodeInvalidValue + ": null for a NOT NULL column"})
				continue
			}
			cols = append(cols, f.Column)
			vals = append(vals, "NULL::"+f.PGType)
			continue
		}
		s, err := writeLiteral(f, v)
		if err != nil {
			problems = append(problems, FieldProblem{Field: n, Problem: CodeInvalidValue + ": " + err.Error()})
			continue
		}
		cols = append(cols, f.Column)
		vals = append(vals, q.bind(s)+"::"+f.PGType)
	}
	return cols, vals, problems
}

// writeLiteral renders a value as text for its column's cast: JSON columns take any JSON value,
// everything else the read path's strict per-type formats.
func writeLiteral(f Field, v any) (string, error) {
	if f.Scalar == JSON {
		b, err := json.Marshal(v)
		if err != nil {
			return "", errors.New("not a JSON value")
		}
		return string(b), nil
	}
	switch v.(type) {
	case map[string]any, []any:
		return "", fmt.Errorf("expected a %s", f.Scalar)
	}
	s, err := literal(f, v)
	if err != nil {
		// literal's message names the field and repeats the value; the problem list names the
		// field already and never carries values.
		return "", fmt.Errorf("not a valid %s", f.Scalar)
	}
	return s, nil
}

// keyCondition matches exactly one row by its whole primary key.
func (m Model) keyCondition(q *sqlQuery, key map[string]any) (string, error) {
	if len(m.PK) == 0 {
		return "", writeErr(422, CodeInvalidRequest, "this table has no primary key, so rows can't be updated or deleted through the API")
	}
	var problems []FieldProblem
	var parts []string
	for _, f := range m.PK {
		v, ok := key[f.Name]
		if !ok || v == nil {
			problems = append(problems, FieldProblem{Field: f.Name, Problem: "key field missing"})
			continue
		}
		s, err := literal(f, v)
		if err != nil {
			problems = append(problems, FieldProblem{Field: f.Name, Problem: CodeInvalidValue + ": not a valid " + string(f.Scalar)})
			continue
		}
		parts = append(parts, quote(f.Column)+" = "+q.bind(s)+"::"+f.PGType)
	}
	for n := range key {
		if !m.isPKField(n) {
			problems = append(problems, FieldProblem{Field: n, Problem: "not a primary-key field"})
		}
	}
	if len(problems) > 0 {
		sort.Slice(problems, func(i, j int) bool { return problems[i].Field < problems[j].Field })
		return "", &WriteError{Status: 400, Code: CodeInvalidRequest, Msg: "the key must name every primary-key field and nothing else", Fields: problems}
	}
	return strings.Join(parts, " AND "), nil
}

func (m Model) isPKField(name string) bool {
	for _, f := range m.PK {
		if f.Name == name {
			return true
		}
	}
	return false
}

func (m Model) versionCondition(q *sqlQuery, req WriteRequest) string {
	if req.ExpectedVersion == "" {
		return ""
	}
	return " AND " + versionExpr + " = " + q.bind(req.ExpectedVersion)
}

// returning selects every field as the read path serializes it, then the row's version.
func (m Model) returning() string {
	sel := make([]string, 0, len(m.Fields)+1)
	for _, f := range m.Fields {
		sel = append(sel, outExpr(f))
	}
	return strings.Join(append(sel, versionExpr), ", ")
}

func (m Model) runReturning(ctx context.Context, tx pgx.Tx, stmt string, args []any) (WriteResult, bool, error) {
	rows, err := tx.Query(ctx, stmt, args...)
	if err != nil {
		return WriteResult{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return WriteResult{}, false, rows.Err()
	}
	raws := make([][]byte, len(m.Fields))
	vals := make([]any, len(m.Fields)+1)
	for i := range raws {
		vals[i] = &raws[i]
	}
	var version string
	vals[len(m.Fields)] = &version
	if err := rows.Scan(vals...); err != nil {
		return WriteResult{}, false, err
	}
	rows.Close()
	return WriteResult{Row: toRow(m.Fields, raws), Version: version}, true, rows.Err()
}

func validationErr(problems []FieldProblem) error {
	sort.Slice(problems, func(i, j int) bool { return problems[i].Field < problems[j].Field })
	return &WriteError{Status: 422, Code: problems[0].codeOnly(), Msg: "the row isn't valid for this API", Fields: problems}
}

func (p FieldProblem) codeOnly() string {
	code, _, _ := strings.Cut(p.Problem, ":")
	return code
}

// withWriteTx runs fn in a read-write transaction with a statement timeout and commits. Starting
// is retried once on a connection error, as for reads (a pooled connection may have outlived its
// lease); a failure at COMMIT that isn't the server's own answer is ErrOutcomeUnknown.
func withWriteTx(ctx context.Context, db DB, timeout time.Duration, fn func(pgx.Tx) error) error {
	start := func() (pgx.Tx, error) {
		tx, err := db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadWrite})
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", timeout.Milliseconds())); err != nil {
			_ = tx.Rollback(context.Background())
			return nil, err
		}
		return tx, nil
	}
	tx, err := start()
	if err != nil && isConnectionError(err) && ctx.Err() == nil {
		tx, err = start()
	}
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // a no-op after Commit
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		if isConnectionError(err) {
			return ErrOutcomeUnknown
		}
		return err
	}
	return nil
}

// mapWriteError turns a database error into a WriteError (or a sentinel the caller acts on).
// Messages name constraints, never values; class 22 messages are Postgres's own, about the
// caller's own input.
func mapWriteError(err error, timeout time.Duration) error {
	var we *WriteError
	if errors.As(err, &we) || errors.Is(err, ErrOutcomeUnknown) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return writeErr(503, CodeUnavailable, "the write took longer than %s and was cancelled", timeout)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch {
	case pgErr.Code == "42501":
		return ErrInsufficientPrivilege
	case pgErr.Code == "23505":
		return &WriteError{Status: 409, Code: CodeConflict, Msg: "a row with the same unique value already exists", Constraint: pgErr.ConstraintName}
	case strings.HasPrefix(pgErr.Code, "23"):
		return &WriteError{Status: 422, Code: CodeConstraint, Msg: "the row breaks a database constraint", Constraint: pgErr.ConstraintName}
	case strings.HasPrefix(pgErr.Code, "22"):
		return &WriteError{Status: 422, Code: CodeInvalidValue, Msg: "a value isn't valid for its column: " + pgErr.Message}
	case pgErr.Code == "42703" || pgErr.Code == "42P01" || pgErr.Code == "42804":
		return writeErr(409, CodeDefinitionStale, "the table no longer matches this API's snapshot; an editor must regenerate the API")
	case pgErr.Code == "57014":
		return writeErr(503, CodeUnavailable, "the write took longer than %s and was cancelled", timeout)
	case pgErr.Code == "40001" || pgErr.Code == "40P01" || pgErr.Code == "55P03":
		return writeErr(503, CodeUnavailable, "the row is busy; retry")
	}
	return err
}
