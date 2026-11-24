package bot

// Hardening tests for the M-6 callback-first change (v3.4.1).
//
// Telegram closes a button's spinner only when answerCallbackQuery arrives, and it
// expires the query after ~10 s. During the 2026-11-23 stall the bot answered
// handlers' queries only AFTER their DB work, so taps expired unanswered and the
// user watched a spinner while the handler filed another incident.
//
// The fix is deliberately NOT a blanket pre-answer in prep: Telegram's second
// answerCallbackQuery is not reliably accepted, so pre-answering would swallow the
// handlers' own popups (block/seen/report/settings alerts) on a HEALTHY bot — a
// user-visible regression. Instead prep answers centrally only on the DB-degraded
// fast-fail path, where the handler never runs at all and the tap would otherwise
// be left unanswered. These tests pin exactly that: no answer in normal operation,
// an immediate answer on the degraded path, and answer errors never becoming
// incidents.
//
// New file only: the pre-existing callbacks_test.go is untouched.

import (
	"strings"
	"testing"
	"time"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"

	"github.com/aturzone/chevaletAnonBot/internal/config"
	"github.com/aturzone/chevaletAnonBot/internal/db"
)

// TestHardeningNoPreAnswerOnDroppedUpdate: prep must NOT answer callbacks by
// itself in normal operation. Here the update is dropped by ignoredChat, i.e.
// prep returns before any handler or DB work — and it must leave the query
// unanswered for the (non-existing) handler, not answer it centrally.
func TestHardeningNoPreAnswerOnDroppedUpdate(t *testing.T) {
	tgBot, fake := newHardeningFakeAPI(t)
	b := &Bot{TG: tgBot, Cfg: &config.Config{}, DB: &db.DB{}}
	handlerRan := false
	inner := b.prep(func(*Bot, *gotgbot.Bot, *ext.Context, string) error {
		handlerRan = true
		return nil
	})

	cb := &gotgbot.CallbackQuery{Id: "cq-1", From: gotgbot.User{Id: 4242}, Data: "anything"}
	ctx := &ext.Context{
		Update:        &gotgbot.Update{CallbackQuery: cb},
		EffectiveChat: &gotgbot.Chat{Id: 4242, Type: "channel"},
		EffectiveUser: &gotgbot.User{Id: 4242},
	}
	mark := fake.mark()
	if err := inner(tgBot, ctx); err != nil {
		t.Fatalf("prep = %v; want nil", err)
	}
	if handlerRan {
		t.Error("the handler ran for a chat prep filters out")
	}
	if calls := fake.since(mark); len(calls) != 0 {
		t.Errorf("prep answered a callback in normal operation (calls: %+v); that would double-answer the handler's popup", calls)
	}
}

// TestHardeningDegradedFastFail covers the incident fast-fail: once the DB layer
// has recorded an unavailable failure, prep skips the handler AND the database,
// answers the callback (so the tap does not expire — the stall case behind M-6),
// and replies the ordinary DB-problem text. The Bot's DB is a zero-value db.DB
// (atomic state only, nil pool), so ANY DB call would panic: reaching the end
// without a panic is part of the proof.
func TestHardeningDegradedFastFail(t *testing.T) {
	tgBot, fake := newHardeningFakeAPI(t)
	database := &db.DB{}
	database.MarkFailure()
	if !database.DegradedFor(time.Minute) {
		t.Fatal("test setup: MarkFailure did not put the DB into the degraded state")
	}

	b := &Bot{TG: tgBot, Cfg: &config.Config{}, DB: database}
	handlerRan := false
	inner := b.prep(func(*Bot, *gotgbot.Bot, *ext.Context, string) error {
		handlerRan = true
		return nil
	})

	t.Run("callback is answered immediately", func(t *testing.T) {
		msg := &gotgbot.Message{
			MessageId: 11,
			From:      &gotgbot.User{Id: 4244, FirstName: "u"},
			Chat:      gotgbot.Chat{Id: 4244, Type: "private"},
			Date:      time.Now().Unix(),
			Text:      "hello",
		}
		cb := &gotgbot.CallbackQuery{Id: "cq-3", From: gotgbot.User{Id: 4244}, Data: "anything"}
		ctx := &ext.Context{
			Update:           &gotgbot.Update{CallbackQuery: cb},
			EffectiveMessage: msg,
			EffectiveChat:    &msg.Chat,
			EffectiveUser:    msg.From,
		}
		mark := fake.mark()
		if err := inner(tgBot, ctx); err != nil {
			t.Fatalf("prep = %v; want nil", err)
		}
		if handlerRan {
			t.Error("the handler ran while the database was degraded")
		}
		calls := fake.since(mark)
		if len(calls) != 2 {
			t.Fatalf("calls = %+v; want exactly the answer + the DB notice", calls)
		}
		if calls[0].method != "answerCallbackQuery" || calls[0].params["callback_query_id"] != "cq-3" {
			t.Errorf("first call = %+v; want the callback answered before anything else", calls[0])
		}
		if calls[1].method != "sendMessage" || calls[1].params["chat_id"] != "4244" || calls[1].params["text"] != txtDBProblem {
			t.Errorf("second call = %+v; want the txtDBProblem notice to the user", calls[1])
		}
	})

	t.Run("plain message gets only the DB notice", func(t *testing.T) {
		_, ctx := hardeningTestMessage(4245)
		mark := fake.mark()
		if err := inner(tgBot, ctx); err != nil {
			t.Fatalf("prep = %v; want nil", err)
		}
		calls := fake.since(mark)
		if len(calls) != 1 || calls[0].method != "sendMessage" || calls[0].params["text"] != txtDBProblem {
			t.Fatalf("calls = %+v; want only the txtDBProblem notice", calls)
		}
	})

	// A success (the next probe once the window expires) opens the gate again.
	database.MarkSuccess()
	if database.DegradedFor(time.Minute) {
		t.Error("MarkSuccess must clear the degraded state")
	}
}

// TestHardeningCallbackAnswerErrorNotIncident: an answerCallbackQuery rejected by
// Telegram ("query is too old" — the incident's symptom) is benign. handleErr must
// swallow it, with no user reply and no incident report.
func TestHardeningCallbackAnswerErrorNotIncident(t *testing.T) {
	tgBot, fake := newHardeningFakeAPI(t)
	b := &Bot{TG: tgBot, Cfg: &config.Config{ErrorChatID: "-100123456"}, DB: &db.DB{}, limiter: newSendLimiter()}
	_, ctx := hardeningTestMessage(4246)

	tooOld := &gotgbot.TelegramError{
		Code:        400,
		Description: "Bad Request: query is too old and response timeout expired or query ID is invalid",
	}
	mark := fake.mark()
	if err := b.handleErr(tgBot, ctx, tooOld); err != nil {
		t.Fatalf("handleErr(answer error) = %v; want nil (benign stale tap)", err)
	}
	if calls := fake.since(mark); len(calls) != 0 {
		t.Errorf("a stale callback answer produced Telegram calls: %+v; want none", calls)
	}
}

// TestHardeningNormalPathHandlerAnswersOnly drives a real callback through the REAL
// dispatcher and a real PostgreSQL: a handler that shows a popup must be the ONLY
// one that answers. Two answers (prep + handler) are what Telegram may reject,
// losing the popup — the reason prep does not pre-answer globally. Skipped without
// BOT_TEST_DB (same harness as TestAnchor*).
func TestHardeningNormalPathHandlerAnswersOnly(t *testing.T) {
	h := newHarness(t)
	uid := newUID()
	prompt := &gotgbot.Message{
		MessageId: 777,
		From:      &gotgbot.User{Id: fakeBotID, IsBot: true, Username: "chevalet_test_bot"},
		Chat:      gotgbot.Chat{Id: uid, Type: "private"},
		Text:      "prompt",
	}

	mark := h.tg.mark()
	// alreadySeen is a popup-only handler: it answers with a toast and does nothing
	// else, so the transcript is exactly one answerCallbackQuery.
	h.press(uid, prompt, "alread-seen")

	calls := h.tg.since(mark)
	if len(calls) != 1 {
		t.Fatalf("calls = %+v; want exactly the handler's one answerCallbackQuery", calls)
	}
	if calls[0].method != "answerCallbackQuery" {
		t.Fatalf("call = %+v; want answerCallbackQuery", calls[0])
	}
	if calls[0].params["text"] != txtAlreadySeenOnce {
		t.Errorf("popup text = %q; want the handler's %q", calls[0].params["text"], txtAlreadySeenOnce)
	}
}

// TestHardeningNormalPathNoAnswerBeforeHandlerWork: a normal-path callback is NOT
// pre-answered by prep. The connect prompt's cancel button is handled outside the
// conversations and edits the message before anything else, so the first Bot API
// call must be editMessageText — and cancel never answers, so the query is left to
// the handler exactly as before. A prep pre-answer would show up as a leading
// answerCallbackQuery here.
func TestHardeningNormalPathNoAnswerBeforeHandlerWork(t *testing.T) {
	h := newHarness(t)
	uid := newUID()
	prompt := &gotgbot.Message{
		MessageId: 778,
		From:      &gotgbot.User{Id: fakeBotID, IsBot: true, Username: "chevalet_test_bot"},
		Chat:      gotgbot.Chat{Id: uid, Type: "private"},
		Text:      "prompt",
	}

	mark := h.tg.mark()
	// The cid in the button data is irrelevant to cancel — only the "cancel|" prefix
	// matters for routing — so no DB setup is needed to mint it.
	h.press(uid, prompt, "cancel|anylink")

	calls := h.tg.since(mark)
	if len(calls) == 0 {
		t.Fatal("the callback produced no Bot API calls at all")
	}
	if calls[0].method != "editMessageText" {
		t.Fatalf("first Bot API call = %q (%+v); want the handler's editMessageText (no prep pre-answer)",
			calls[0].method, calls[0])
	}
	var edited bool
	for _, c := range calls {
		if c.method == "answerCallbackQuery" {
			t.Errorf("prep answered a normal-path callback: %+v; popups would break", c)
		}
		if c.method == "editMessageText" && strings.Contains(c.params["text"], txtCancelGone) {
			edited = true
		}
	}
	if !edited {
		t.Errorf("the cancel handler did not edit the prompt away; calls: %+v", calls)
	}
}
