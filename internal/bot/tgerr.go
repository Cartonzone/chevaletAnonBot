package bot

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/jackc/pgx/v5/pgconn"
)

// These predicates classify gotgbot API errors the way modules/Global/
// decorators.py classified PTB exceptions via str(e). They match the raw
// Telegram error descriptions case-insensitively (substring), which is
// equivalent to — and a little more robust than — the original exact-string
// checks.

func asTelegramError(err error) *gotgbot.TelegramError {
	var te *gotgbot.TelegramError
	if errors.As(err, &te) {
		return te
	}
	return nil
}

func descContains(err error, sub string) bool {
	te := asTelegramError(err)
	if te == nil {
		return false
	}
	return strings.Contains(strings.ToLower(te.Description), strings.ToLower(sub))
}

// errBotBlocked: "Forbidden: bot was blocked by the user".
func errBotBlocked(err error) bool { return descContains(err, "bot was blocked by the user") }

// errBotNotMember: "Forbidden: bot is not a member of the channel chat".
func errBotNotMember(err error) bool {
	return descContains(err, "bot is not a member of the channel chat")
}

// errReplyNotFound: "Bad Request: message to be replied not found".
func errReplyNotFound(err error) bool { return descContains(err, "message to be replied not found") }

// errMessageIDInvalid: "Bad Request: MESSAGE_ID_INVALID".
func errMessageIDInvalid(err error) bool { return descContains(err, "MESSAGE_ID_INVALID") }

// errQueryTooOld: "Bad Request: query is too old and response timeout expired …".
func errQueryTooOld(err error) bool { return descContains(err, "query is too old") }

// errMessageNotModified: "Bad Request: message is not modified: specified new
// message content and reply markup are exactly the same …". Harmless — it just
// means the user re-triggered a menu/state they were already on (e.g. a double
// tap, or pressing "back" while already at that menu), so the edit is a no-op
// Telegram refuses rather than a real failure.
func errMessageNotModified(err error) bool { return descContains(err, "message is not modified") }

// errForbidden matches any HTTP 403 "Forbidden: …" Telegram error, mirroring the
// PTB `except Forbidden` in is_reply_to_channel (e.g. a private channel the bot
// was never added to).
func errForbidden(err error) bool {
	te := asTelegramError(err)
	return te != nil && te.Code == 403
}

// isDBError reports whether err is a PostgreSQL/database failure, mirroring the
// Python prep's `except (psycopg2.Error, psycopg2.DatabaseError)`: a server-side
// SQL error (PgError) or a failure to reach/use the database (see
// isDBUnavailable). Both warrant the user-facing "database problem" reply and an
// ERROR_CHAT_ID report. (A clean separation from isNetworkError: DB failures are
// pgconn/pgx types, never a *url.Error.)
func isDBError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return true
	}
	return isDBUnavailable(err)
}

// isDBUnavailable reports whether err means the database is UNREACHABLE or not
// answering — as opposed to a PgError, which is the server answering with an
// error (a bad statement, a constraint violation) and so proves the connection
// is alive.
//
// Why this distinction exists (the 2026-11-23 incident): a storage-I/O stall made
// established connections hit the caller's 30 s deadline. pgx returns that as its
// internal *errTimeout ("timeout: context deadline exceeded") — neither a PgError
// nor a ConnectError — and pgxpool.Acquire returns a bare context.DeadlineExceeded
// when it cannot hand out a connection in time. isDBError used to match only the
// two exported pgconn types, so both classes fell through handleErr's default
// branch into onError: one UNTHROTTLED incident report plus a tracking-code reply
// for every failing update — the error-channel flood the audit investigated (C-1).
//
// It is deliberately NOT "every DB error": a unique-violation or SQL error must
// not trip the bot's fast-fail gate (db.DegradedFor), because the database is
// demonstrably working and retrying other users' unrelated updates would hurt.
func isDBUnavailable(err error) bool {
	// A server-side PgError is normally proof the connection is alive — EXCEPT for
	// the connection-exception SQLSTATE class: class 08 (connection exception) and
	// the admin/shutdown 57P0x codes mean the established connection is going away
	// (e.g. a planned PostgreSQL restart), so the next updates should fast-fail
	// rather than each waiting out an interactive timeout. Ordinary SQL errors
	// (unique violation, bad statement, ...) deliberately do NOT match.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case strings.HasPrefix(pgErr.Code, "08"):
			return true
		case pgErr.Code == "57P01", pgErr.Code == "57P02", pgErr.Code == "57P03":
			// admin shutdown / crash shutdown / cannot connect now
			return true
		}
		return false
	}
	// *pgconn.ConnectError: the attempt to open a connection failed (DNS lookup,
	// dial, TLS, authentication) — the database is effectively down for us.
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return true
	}
	// pgconn.Timeout matches the unexported *errTimeout (the incident's exact
	// "timeout: context deadline exceeded") anywhere in the wrap chain.
	if pgconn.Timeout(err) {
		return true
	}
	// A bare ctx deadline is pgxpool.Acquire returning ctx.Err() unchanged. It is
	// only a DB symptom when it did not come from net/http: an HTTP timeout is a
	// *url.Error wrapping the same sentinel and belongs to isNetworkError.
	if isNetworkError(err) {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// isContextCanceled reports whether err is a context CANCELLATION (not a
// deadline) — in this bot that is the shutdown path: Run's context is cancelled,
// in-flight DB/API calls fail with context.Canceled, and every one of them would
// otherwise be filed as an incident while the process is going down (a report
// that usually cannot even be delivered). handleErr swallows it as a no-op.
func isContextCanceled(err error) bool {
	return errors.Is(err, context.Canceled)
}

// errSummary is the short, REDACTED category for a database error that is sent to
// Telegram's ERROR_CHAT_ID. The full error string goes to the process logs only.
//
// Why (M-8): handleErr used to paste "PostgreSQL ERROR: " + err.Error() into the
// error channel. pgx's ConnectError renders its config — "failed to connect to
// `user=root database=mydatabase`: hostname resolving error: lookup
// telegram-bot-db on 127.0.0.11:53: … connection refused" — so an infrastructure
// hiccup published the DB user/database/host and Docker DNS internals to everyone
// who can read that channel, which is typically wider than the ops team. The
// SQLSTATE is the only server detail worth surfacing: it identifies the class of
// failure without any row data, query text or connection string.
func errSummary(err error) string {
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return "database connection/DNS failure"
	}
	if pgconn.Timeout(err) || errors.Is(err, context.DeadlineExceeded) {
		return "database timeout"
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code != "" {
		return "SQLSTATE " + pgErr.Code
	}
	return "database error"
}

// isNetworkError reports whether err is a transport-level failure talking to
// Telegram — not a Telegram API error (those carry a TelegramError) — mirroring
// the Python prep's `except NetworkError`. gotgbot speaks HTTP via net/http, so
// such failures surface as a *url.Error.
func isNetworkError(err error) bool {
	if asTelegramError(err) != nil {
		return false
	}
	var uerr *url.Error
	return errors.As(err, &uerr)
}

// errTooManyRequests matches a Telegram 429 flood wait, and returns how long
// Telegram asked us to wait.
//
// This is NOT a bug, and treating it as one made a flood worse: every 429 was
// filed as an incident, and each incident posts to ERROR_CHAT_ID and replies a
// tracking code to the user — so a rate limit produced two more sends per
// occurrence, on a bot that was already over its limit. 321 of them were logged in
// nine hours of normal traffic.
//
// Telegram reports the wait in TelegramError.ResponseParams.RetryAfter, and also
// in the description ("Too Many Requests: retry after 251") — the description is
// parsed as a fallback because the field is only populated for some errors.
func errTooManyRequests(err error) (retryAfter int64, ok bool) {
	te := asTelegramError(err)
	if te == nil {
		return 0, false
	}
	if te.Code != 429 && !strings.Contains(strings.ToLower(te.Description), "too many requests") {
		return 0, false
	}
	if te.ResponseParams != nil && te.ResponseParams.RetryAfter > 0 {
		return te.ResponseParams.RetryAfter, true
	}
	if i := strings.LastIndex(strings.ToLower(te.Description), "retry after "); i >= 0 {
		if n, cerr := strconv.ParseInt(strings.TrimSpace(te.Description[i+len("retry after "):]), 10, 64); cerr == nil {
			return n, true
		}
	}
	return 0, true
}

// floodSeconds returns the retry_after of a 429, or 0 if err is not one.
func floodSeconds(err error) int64 {
	n, ok := errTooManyRequests(err)
	if !ok {
		return 0
	}
	return n
}

// isFloodErr reports whether err is a 429 at all, including one Telegram sent
// without a retry_after value.
func isFloodErr(err error) bool {
	_, ok := errTooManyRequests(err)
	return ok
}

// errNoSendRights: "Bad Request: not enough rights to send text messages to the
// chat" (and the media variants).
//
// This is a FACT ABOUT A CHAT, not a fault. prep deliberately allows supergroups
// (Python parity — the bot answers commands and the catch-all there), so once
// somebody adds the bot to a group where members are restricted from posting, every
// message in that group makes the catch-all try to reply and be refused. Treating
// that as an incident filed one report per message into ERROR_CHAT_ID and tried to
// reply to the user with a tracking code — a reply that also could not be sent.
//
// Same family as errForbidden: nothing to fix, nothing to page anyone about.
func errNoSendRights(err error) bool {
	return descContains(err, "not enough rights to send")
}
