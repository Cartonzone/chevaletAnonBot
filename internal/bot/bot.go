// Package bot wires the Telegram side of ChevaletAnonBot on top of gotgbot:
// the Bot client, the long-polling Updater, the Dispatcher, the central error
// handler, and the prep middleware that mirrors the Python @prep_function.
package bot

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"github.com/PaulSonOfLars/gotgbot/v2/ext/handlers/conversation"

	"github.com/aturzone/chevaletAnonBot/internal/config"
	"github.com/aturzone/chevaletAnonBot/internal/db"
	"github.com/aturzone/chevaletAnonBot/internal/dynset"
	"github.com/aturzone/chevaletAnonBot/internal/encoder"
	"github.com/aturzone/chevaletAnonBot/internal/texts"
)

// Database timeouts, split by who is waiting.
//
// dbOpTimeoutInteractive bounds a HANDLER's database work: the dispatcher runs
// with MaxRoutines=1, so every second spent waiting on the DB is a second every
// other user's update waits behind it. A wedged database would otherwise stack
// twenty 30-second waits into a ten-minute unresponsive bot — exactly the failure
// mode the watchdog exists to catch.
//
// dbOpTimeoutBackground bounds a background job's database work (connection
// check, mass broadcast), where nobody is waiting on an interactive reply and a
// bigger budget is harmless.
const (
	dbOpTimeoutInteractive = 5 * time.Second
	dbOpTimeoutBackground  = 30 * time.Second

	// dbOpTimeout is the OLD single name. It stays as an alias for background-path
	// call sites that were not part of the interactive-timeout split (outbox
	// replay, box flush) so their behaviour is unchanged; handler code should use
	// dbOpTimeoutInteractive (via b.bg()).
	dbOpTimeout = dbOpTimeoutBackground

	// Telegram request budgets. DefaultRequestOpts applies to EVERY API call;
	// 10s is long enough for a normal send through the production proxy and short
	// enough to fail a dead connection instead of stalling the serial dispatcher.
	// The old code raised it to 30s whenever PROXY was set, which turned one slow
	// proxy into a 30-second pipeline stall per update.
	telegramRequestTimeout = 10 * time.Second
	// Per-call budgets are explicit, so the transport-level timeouts below only
	// need to be generous upper bounds (the request context is the real limit).
	telegramDialTimeout           = 5 * time.Second
	telegramTLSHandshakeTimeout   = 10 * time.Second
	telegramResponseHeaderTimeout = 30 * time.Second

	// Shutdown phase bounds: the updater first (so no new update is dispatched),
	// then the delivery batcher (flush what was buffered), then background work
	// (drain before the DB pool closes). Each is bounded so one hung Telegram call
	// cannot make SIGTERM hang forever; compose gives the container 60s total.
	shutdownUpdaterTimeout    = 10 * time.Second
	shutdownBatcherTimeout    = 10 * time.Second
	shutdownBackgroundTimeout = 10 * time.Second

	// incidentWindow throttles the ERROR_CHAT_ID/user tracking-code replies. A
	// broken handler can fire thousands of times a minute; each incident used to
	// cost two MORE Telegram sends on a bot already in trouble.
	incidentWindow = 30 * time.Second
)

// Bot bundles everything a handler needs.
type Bot struct {
	TG         *gotgbot.Bot
	Dispatcher *ext.Dispatcher
	Updater    *ext.Updater

	DB    *db.DB
	Cfg   *config.Config
	Texts *texts.Loader
	Dyn   *dynset.Settings

	// Tokens seals/opens the per-emission callback tokens that carry a user id in
	// button callback_data (replacing the keyless, linkable EncodeChevaletID for
	// NEW messages; see encoder/token.go). Keyed off BOT_TOKEN + TOKEN_KEYS.
	Tokens *encoder.TokenCipher

	users      *userStore
	aiQueue    *aiQueue
	admins     map[string]bool
	errReports *errReportStore

	// limiter paces every outbound Telegram call and owns the flood pause. See
	// ratelimit.go for why this lives at the BotClient layer.
	limiter *sendLimiter

	// batcher buffers ordinary outbound sends for a short debounce window so a
	// burst of messages from one sender reaches one recipient as a single delivery
	// pass. See batcher.go. nil is impossible after New; the field exists here so
	// Run can drain it on shutdown and tests can drive it directly.
	batcher *batcher

	// convStores holds the three ConversationHandlers' state storages so a stale
	// flow can be dropped when the user starts a different one. Without this, a
	// user part-way through composing a message who goes to settings and types a
	// new name has that text delivered as the message: the start conversation is
	// registered first, so its send state claims the update. See dropConversations.
	convStores []conversation.Storage

	// dbErrMu/lastDBErr throttle the "PostgreSQL ERROR" reports to ERROR_CHAT_ID:
	// during a brief DB outage every one of (10k users × concurrent) updates hits
	// the error path, and an unthrottled flood can itself trip Telegram 429s at
	// the worst possible moment.
	dbErrMu   sync.Mutex
	lastDBErr time.Time

	// incidentMu/lastIncidentAt/suppressedIncidents throttle the tracking-code
	// incident reports (ERROR_CHAT_ID + user reply) shared by onError/onPanic.
	// See beginIncident: at most one incident message per incidentWindow, with the
	// suppressed count folded into the next one.
	incidentMu          sync.Mutex
	lastIncidentAt      time.Time
	suppressedIncidents int

	// bgWG tracks background goroutines (AI loop, GM/GN, an in-flight mass-msg) so
	// Run can drain them on shutdown before the DB pool is closed; runCtx is the
	// run-scoped context they derive from.
	bgWG   sync.WaitGroup
	runCtx context.Context
}

// goBG runs fn as a tracked background goroutine, awaited by Run on shutdown.
func (b *Bot) goBG(fn func()) {
	b.bgWG.Add(1)
	go func() {
		defer b.bgWG.Done()
		fn()
	}()
}

// waitBackground waits for tracked background work to finish, bounded by timeout
// so a long mass-broadcast can't hang shutdown indefinitely.
func (b *Bot) waitBackground(timeout time.Duration) {
	done := make(chan struct{})
	go func() { b.bgWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		slog.Warn("background work didn't finish within the shutdown grace period")
	}
}

// allowDBErrReport reports true at most once per 30s, so DB-outage error reports
// to ERROR_CHAT_ID are coalesced instead of flooding.
func (b *Bot) allowDBErrReport() bool {
	b.dbErrMu.Lock()
	defer b.dbErrMu.Unlock()
	now := time.Now()
	if !b.lastDBErr.IsZero() && now.Sub(b.lastDBErr) < 30*time.Second {
		return false
	}
	b.lastDBErr = now
	return true
}

// newTelegramTransport builds the HTTP transport for every Telegram API call.
//
// The explicit timeouts are the H-6 fix: previously, with PROXY set, the client
// was a bare http.Transport with NO dial/TLS/response timeouts and a 30s
// DefaultRequestOpts. A dead proxy therefore parked each request for up to 30s
// (the dispatcher is serial, so the whole bot stalled with it) and a hanging TLS
// handshake had no bound at all. The per-request context remains the real
// deadline; these are the transport-level backstops.
func newTelegramTransport(proxy *url.URL) *http.Transport {
	return &http.Transport{
		// http.ProxyURL(nil) is a valid "no proxy" function, so the same transport
		// shape is used whether or not PROXY is configured.
		Proxy: http.ProxyURL(proxy),
		DialContext: (&net.Dialer{
			Timeout:   telegramDialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   telegramTLSHandshakeTimeout,
		ResponseHeaderTimeout: telegramResponseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
	}
}

// newTelegramBotClient builds the gotgbot BotClient shared by every API call.
// Split out of New so the timeout contract (H-6: 10s default even when PROXY is
// set) is directly testable without a live Telegram token.
func newTelegramBotClient(proxy *url.URL) *gotgbot.BaseBotClient {
	return &gotgbot.BaseBotClient{
		Client:             http.Client{Transport: newTelegramTransport(proxy)},
		DefaultRequestOpts: &gotgbot.RequestOpts{Timeout: telegramRequestTimeout},
	}
}

// New builds the Telegram bot, dispatcher and updater, and registers handlers.
func New(cfg *config.Config, database *db.DB, txt *texts.Loader) (*Bot, error) {
	limiter := newSendLimiter()

	var proxyURL *url.URL
	if cfg.Proxy != "" {
		u, err := url.Parse(cfg.Proxy)
		if err != nil {
			return nil, fmt.Errorf("bot: invalid PROXY %q: %w", cfg.Proxy, err)
		}
		proxyURL = u
	}
	// DefaultRequestOpts applies to every call: 10s is enough through the proxy and,
	// unlike the old 30s, cannot stall the serial dispatcher for half a minute.
	// Individual calls raise it where genuinely needed (the nightly backup upload
	// passes its own 120s RequestOpts).
	botOpts := &gotgbot.BotOpts{
		RequestOpts: &gotgbot.RequestOpts{Timeout: telegramRequestTimeout},
		BotClient:   newTelegramBotClient(proxyURL),
	}

	// Every API call goes through the limiter — including the ones a future change
	// adds, which is the point of doing it here rather than per call site.
	lc := &limitedClient{inner: botOpts.BotClient, limiter: limiter}
	botOpts.BotClient = lc

	tg, err := gotgbot.NewBot(cfg.BotToken, botOpts)
	if err != nil {
		return nil, fmt.Errorf("bot: new bot: %w", err)
	}

	// Callback-token cipher, keyed off BOT_TOKEN (+ any TOKEN_KEYS). Fail closed:
	// without a key the bot would emit unprotected/linkable tokens, so refuse to
	// start rather than silently regress anonymity. (BOT_TOKEN is required, so this
	// only trips on a misconfigured build.)
	tokens := encoder.NewTokenCipher(cfg.BotToken, cfg.TokenKeys...)
	if !tokens.Ready() {
		return nil, fmt.Errorf("bot: callback-token cipher has no key (BOT_TOKEN missing?)")
	}

	b := &Bot{
		TG:         tg,
		DB:         database,
		Cfg:        cfg,
		Texts:      txt,
		Dyn:        dynset.New(cfg.DynsetPath, cfg.AIURL, cfg.AISessionID, cfg.DonationLink),
		Tokens:     tokens,
		users:      newUserStore(),
		aiQueue:    newAIQueue(),
		admins:     make(map[string]bool, len(cfg.Admins)),
		errReports: newErrReportStore(50),
		limiter:    limiter,
	}
	// Wired after b exists: the flush path is a method on b.
	b.batcher = newBatcher(cfg.BatchWindow, cfg.AlbumWindow, cfg.BatchMaxWait, cfg.BatchMaxMessages, b.deliverUnits)
	for _, a := range cfg.Admins {
		b.admins[a] = true
	}
	// Wired after b exists: a failed send is queued durably instead of lost.
	lc.onRetryable = b.enqueueFailedSend

	b.Dispatcher = ext.NewDispatcher(&ext.DispatcherOpts{
		Error: b.onError,
		Panic: b.onPanic,
		// Process updates strictly sequentially and IN ORDER (limiter of 1,
		// acquired before each goroutine is spawned). This matches the Python
		// bot's PTB default (concurrent_updates=False) that the conversation FSM
		// and media-group/album state machine were designed around: gotgbot's
		// default 50-way concurrent dispatch would otherwise let two updates from
		// the SAME user race the conversation-state read and the media-group
		// stash (e.g. a second album item re-entering the send path and clearing
		// the group → lost album). Background jobs stay concurrent.
		MaxRoutines: 1,
	})
	b.Updater = ext.NewUpdater(b.Dispatcher, &ext.UpdaterOpts{})

	b.registerHandlers()
	return b, nil
}

// Run starts long polling and blocks until ctx is cancelled, then stops cleanly.
func (b *Bot) Run(ctx context.Context) error {
	b.runCtx = ctx
	// Record when polling effectively starts: the health endpoint and the watchdog
	// measure update-flow staleness from the successful getUpdates timestamps.
	b.limiter.poll.start(time.Now())
	if err := b.Updater.StartPolling(b.TG, &ext.PollingOpts{
		GetUpdatesOpts: &gotgbot.GetUpdatesOpts{
			// Set AllowedUpdates EXPLICITLY. Telegram remembers the last
			// allowed_updates passed to getUpdates and reuses it whenever the
			// parameter is omitted; if anything ever called getUpdates with a list
			// that excluded callback_query, polling without this would silently stop
			// delivering button presses. Listing exactly what the bot handles makes
			// it deterministic (and immune to that gotcha).
			AllowedUpdates: []string{"message", "edited_message", "callback_query"},
			Timeout:        9, // long-poll seconds
			RequestOpts: &gotgbot.RequestOpts{
				Timeout: 11 * time.Second, // slightly above the long-poll timeout
			},
		},
	}); err != nil {
		return fmt.Errorf("bot: start polling: %w", err)
	}

	slog.Info("bot polling", "username", b.TG.User.Username)

	// Start the background jobs (set_commands, AI responder, GM/GN greetings,
	// hourly DB check); they stop when ctx is cancelled.
	b.startBackground(ctx)

	<-ctx.Done()
	shutdownStart := time.Now()

	// PHASE 1 — stop the updater, bounded. Run it in a goroutine so a hung
	// long-poll HTTP call cannot block the rest of the drain; polling stopping is
	// what guarantees no NEW update is dispatched while the batcher drains.
	stopStart := time.Now()
	stopDone := make(chan error, 1)
	go func() { stopDone <- b.Updater.Stop() }()
	var err error
	select {
	case err = <-stopDone:
		slog.Info("updater stopped", "duration", time.Since(stopStart).Round(time.Millisecond))
	case <-time.After(shutdownUpdaterTimeout):
		slog.Warn("updater did not stop within the shutdown budget; continuing",
			"timeout", shutdownUpdaterTimeout)
	}

	// PHASE 2 — deliver whatever the batcher buffered right before SIGTERM.
	// Polling has stopped, so no new messages can be added; this runs before the
	// DB pool is closed. Bounded so a hung Telegram call cannot hang shutdown.
	if b.batcher != nil {
		batchStart := time.Now()
		b.batcher.Shutdown(shutdownBatcherTimeout)
		slog.Info("batcher drained", "duration", time.Since(batchStart).Round(time.Millisecond))
	}

	// PHASE 3 — drain tracked background work before main closes the DB pool, so
	// nothing uses a closed pool (bounded — see waitBackground).
	bgStart := time.Now()
	b.waitBackground(shutdownBackgroundTimeout)
	slog.Info("background work drained",
		"duration", time.Since(bgStart).Round(time.Millisecond),
		"total_shutdown", time.Since(shutdownStart).Round(time.Millisecond))
	return err
}

// PollStatus returns a snapshot of the long-polling loop's activity (see
// PollStatus in ratelimit.go). It is what main wires into /healthz and the
// watchdog: a process that is alive but has received no update for minutes is
// exactly the state the old TCP health probe called healthy.
//
// Nil-safe for a zero-value Bot (tests build one before Run wires the limiter).
func (b *Bot) PollStatus() PollStatus {
	if b == nil || b.limiter == nil || b.limiter.poll == nil {
		return PollStatus{}
	}
	return b.limiter.poll.status()
}

// LastPollOK is the time of the most recent successful getUpdates (zero if none
// has succeeded yet).
func (b *Bot) LastPollOK() time.Time { return b.PollStatus().LastSuccess }

// isAdmin reports whether a uid is in the configured ADMINS list.
func (b *Bot) isAdmin(uid string) bool { return b.admins[uid] }

// onError is the central dispatcher error handler, mirroring
// modules/Global/error_handler.py: it logs, sends a diagnostic report to
// ERROR_CHAT_ID, and replies to the user with a tracking code.
//
// To keep the error channel readable, the report is sent as a compact SUMMARY
// (the tracking code + the error) with a "🔎 more" button; the full detail — the
// triggering update dumped as pretty-printed JSON, paged into <pre> blocks under
// Telegram's length limit — is revealed one page at a time on demand by errMore,
// not dumped all at once. The complete detail is ALSO written to the process
// logs, so it survives even after the in-memory pages are evicted. (A Go error
// carries no stack trace here — unlike a Python exception's __traceback__ — so
// the error string stands in for the traceback and the update dump is the
// primary diagnostic.)
func (b *Bot) onError(tg *gotgbot.Bot, ctx *ext.Context, err error) ext.DispatcherAction {
	code := encoder.GenerateCID(8)

	// Build the update dump once; it goes both to the logs and to the paged report.
	// REDACTED — see redactUpdate. This is an anonymous-messaging bot, and the raw
	// dump carried users' message bodies into the error channel and the logs.
	var updateJSON string
	if ctx != nil && ctx.Update != nil {
		updateJSON = redactUpdate(ctx.Update)
	}
	slog.Error("handler error", "code", code, "err", err, "update", updateJSON)

	var pages []string
	if updateJSON != "" {
		// Page the update dump (escaped) so each page fits in one message under
		// the <pre></pre> wrapper; split on rune boundaries so a multi-byte
		// character is never cut in half.
		pages = chunkString(html.EscapeString(updateJSON), 3500)
	}
	summary := fmt.Sprintf("⚠️ <b>خطایی هنگام پردازش یک آپدیت رخ داد</b>\nکد پیگیری: <code>%s</code>\n\n<pre>%s</pre>",
		code, html.EscapeString(truncate(fmt.Sprintf("%v", err), 600)))
	b.reportIncident(tg, ctx, code, summary, pages)
	return ext.DispatcherActionNoop
}

// onPanic is the dispatcher panic handler. gotgbot recovers handler panics to
// keep the single bot process alive, but WITHOUT this hook a recovered panic is
// only returned to the updater and logged — it never reaches ERROR_CHAT_ID and
// the user gets no tracking code (worse than the Python error_handler, which
// reported exceptions). We route it through the same incident report as onError,
// paging the STACK TRACE (which a plain error doesn't carry).
func (b *Bot) onPanic(tg *gotgbot.Bot, ctx *ext.Context, r any) {
	code := encoder.GenerateCID(8)
	stack := string(debug.Stack())
	slog.Error("handler panic", "code", code, "panic", fmt.Sprintf("%v", r), "stack", stack)

	pages := chunkString(html.EscapeString(stack), 3500)
	summary := fmt.Sprintf("🛑 <b>panic هنگام پردازش یک آپدیت</b>\nکد پیگیری: <code>%s</code>\n\n<pre>%s</pre>",
		code, html.EscapeString(truncate(fmt.Sprintf("%v", r), 600)))
	b.reportIncident(tg, ctx, code, summary, pages)
}

// beginIncident is the C-1b throttle gate shared by onError/onPanic. It answers
// "may this incident be reported now, and how many were suppressed since the last
// report?".
//
// During a systematic fault (a broken handler, a wildly malformed update) the
// same incident can fire thousands of times a minute. Every incident used to cost
// TWO more Telegram sends (the ERROR_CHAT_ID summary and the user's tracking-code
// reply) at the exact moment the bot was already struggling. One report per
// incidentWindow keeps the signal while capping the cost; the suppressed count is
// carried into the next report so nothing is hidden.
func (b *Bot) beginIncident() (suppressed int, send bool) {
	b.incidentMu.Lock()
	defer b.incidentMu.Unlock()
	now := time.Now()
	if !b.lastIncidentAt.IsZero() && now.Sub(b.lastIncidentAt) < incidentWindow {
		b.suppressedIncidents++
		return b.suppressedIncidents, false
	}
	suppressed = b.suppressedIncidents
	b.suppressedIncidents = 0
	b.lastIncidentAt = now
	return suppressed, true
}

// reportIncident files a tracked incident: it stores the detail pages under the
// tracking code, sends a compact summary to ERROR_CHAT_ID with a "🔎 more" button
// that pages the detail on demand, and replies to the triggering user with the
// code. Shared by onError (the update dump) and onPanic (the stack trace).
//
// Throttled by beginIncident: an incident inside the window is counted, logged
// and dropped without any Telegram send. The tracking code is still generated and
// logged by the caller, so the logs remain a complete record even when the channel
// report is suppressed.
func (b *Bot) reportIncident(tg *gotgbot.Bot, ctx *ext.Context, code, summary string, detailPages []string) {
	suppressed, send := b.beginIncident()
	if !send {
		slog.Warn("incident report throttled; suppressing duplicate",
			"code", code, "suppressed_in_window", suppressed)
		return
	}
	if suppressed > 0 {
		// Fold the count into the summary that IS sent, so the channel shows that
		// more of the same happened while reports were muted.
		summary += fmt.Sprintf("\n\n(+%d similar suppressed)", suppressed)
	}
	if b.Cfg.ErrorChatID != "" {
		if chatID, perr := strconv.ParseInt(b.Cfg.ErrorChatID, 10, 64); perr == nil {
			b.errReports.put(code, detailPages)
			opts := &gotgbot.SendMessageOpts{ParseMode: "HTML"}
			// A link button, not a callback one: ERROR_CHAT_ID is a channel and
			// channel callback buttons never reach the bot, so the old "more" button
			// did nothing. The link opens the detail in the admin's private chat.
			if len(detailPages) > 0 && b.TG.User.Username != "" {
				opts.ReplyMarkup = moreLinkButton(b.TG.User.Username, code, len(detailPages))
			}
			_, _ = tg.SendMessage(chatID, summary, opts)
		}
	}
	if ctx != nil && ctx.EffectiveMessage != nil {
		_, _ = ctx.EffectiveMessage.Reply(tg,
			fmt.Sprintf("خطایی رخ داد. کد پیگیری: <code>%s</code>", code),
			&gotgbot.SendMessageOpts{ParseMode: "HTML"},
		)
	}
}

// reportToErrorChat sends a plain diagnostic line to ERROR_CHAT_ID, best-effort
// (used by handleErr's database-error branch, mirroring decorators.py's
// "PostgreSQL ERROR: ..." notice). A no-op when ERROR_CHAT_ID is unset/invalid.
func (b *Bot) reportToErrorChat(tg *gotgbot.Bot, text string) {
	if b.Cfg.ErrorChatID == "" {
		return
	}
	chatID, err := strconv.ParseInt(b.Cfg.ErrorChatID, 10, 64)
	if err != nil {
		return
	}
	_, _ = tg.SendMessage(chatID, text, nil)
}

// --- Telegram flood-wait handling -------------------------------------------
//
// A 429 means the bot is over Telegram's send rate. Continuing to send while a
// retry_after is pending is what turns a short wait into a long one, so the wait
// is recorded once and every outbound path checks it first. That is the difference
// between a 90-second pause and the 607-second ban that was observed.

// noteFloodWait records a Telegram-imposed wait so every outbound call backs off.
func (b *Bot) noteFloodWait(seconds int64) { b.limiter.noteFloodWait(seconds) }

// allowFloodReport throttles the admin notice to one per two minutes, so a flood
// does not produce a flood of reports about the flood.
func (b *Bot) allowFloodReport() bool { return b.limiter.allowReport() }
