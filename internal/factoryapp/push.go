package factoryapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/webpush"
	"github.com/pricec/factory/internal/wire"
)

// Web Push (iOS 16.4+ home-screen PWA, spec §9.6). Anything that currently
// notifies the operator in the UI (planner replies, terminal job
// notifications, pending-job notices, merge/conflict notices, new alerts)
// is also pushed to every subscribed device. VAPID keys and subscriptions
// persist in the tasks settings table; the private key is never logged and
// never leaves this process.

const (
	settingPushVAPID = "push.vapid" // base64url P-256 private scalar
	settingPushSubs  = "push.subs"  // JSON []pushSub

	// EnvPushDisable turns Web Push off server-side ("1"/"true"): the
	// endpoints answer 404 and nothing is sent.
	EnvPushDisable = "FACTORY_PUSH_DISABLE"
	// EnvPushSubject overrides the VAPID "sub" contact (mailto: or https:).
	EnvPushSubject = "FACTORY_PUSH_SUBJECT"

	maxPushBody  = 8 << 10
	maxPushSubs  = 50
	pushBodyChar = 140
)

// pushAllowedHosts are the push services a subscription endpoint may point
// at (suffix match). The server POSTs to these URLs, so an unrestricted
// endpoint would be an SSRF primitive on an unauthenticated surface.
var pushAllowedHosts = []string{
	".push.apple.com",
	"fcm.googleapis.com",
	".push.services.mozilla.com",
	".notify.windows.com",
}

type pushSub struct {
	webpush.Subscription
	Created string `json:"created"`
}

// pushState is the process-local push runtime.
type pushState struct {
	mu       sync.Mutex
	disabled bool
	sender   *webpush.Sender
	vapidPub string
	anyHost  bool
	subject  string
	client   *http.Client
}

func (a *App) initPush(subject string, client *http.Client, anyHost bool) {
	v := strings.ToLower(os.Getenv(EnvPushDisable))
	if s := os.Getenv(EnvPushSubject); s != "" {
		subject = s
	} else if !strings.HasPrefix(subject, "https://") {
		subject = "mailto:admin@example.com"
	}
	a.push = &pushState{disabled: v == "1" || v == "true", subject: subject, client: client, anyHost: anyHost}
}

// ensureVAPID loads the persisted key or generates and persists one.
// Callers hold p.mu.
func (a *App) ensureVAPID(ctx context.Context) error {
	p := a.push
	if p.sender != nil {
		return nil
	}
	priv, ok, err := a.tasks.GetSetting(ctx, settingPushVAPID)
	if err != nil {
		return err
	}
	var v *webpush.VAPID
	if ok {
		if v, err = webpush.ParseVAPID(priv); err != nil {
			return err
		}
	} else {
		if v, err = webpush.GenerateVAPID(); err != nil {
			return err
		}
		if err := a.tasks.SetSetting(ctx, settingPushVAPID, v.Private()); err != nil {
			return err
		}
		log.Printf("factoryapp: push: generated VAPID key pair")
	}
	p.vapidPub = v.Public()
	p.sender = &webpush.Sender{VAPID: v, Subject: p.subject, Client: p.client}
	return nil
}

func (a *App) loadSubs(ctx context.Context) ([]pushSub, error) {
	raw, ok, err := a.tasks.GetSetting(ctx, settingPushSubs)
	if err != nil || !ok || raw == "" {
		return nil, err
	}
	var subs []pushSub
	if err := json.Unmarshal([]byte(raw), &subs); err != nil {
		return nil, err
	}
	return subs, nil
}

func (a *App) saveSubs(ctx context.Context, subs []pushSub) error {
	b, err := json.Marshal(subs)
	if err != nil {
		return err
	}
	return a.tasks.SetSetting(ctx, settingPushSubs, string(b))
}

func (a *App) pushHostAllowed(endpoint string) bool {
	if a.push.anyHost {
		return true
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	for _, s := range pushAllowedHosts {
		if h == strings.TrimPrefix(s, ".") || (strings.HasPrefix(s, ".") && strings.HasSuffix(h, s)) || h == s {
			return true
		}
	}
	return false
}

// handlePushKey serves GET /v1/push/key: the VAPID public key a browser
// subscribes with. Generates the pair on first use.
func (a *App) handlePushKey(w http.ResponseWriter, r *http.Request) {
	if a.push.disabled {
		writeError(w, http.StatusNotFound, "not_found", "web push is disabled on this factory")
		return
	}
	a.push.mu.Lock()
	err := a.ensureVAPID(r.Context())
	pub := a.push.vapidPub
	a.push.mu.Unlock()
	if err != nil {
		log.Printf("factoryapp: push: vapid: %v", err)
		writeError(w, http.StatusInternalServerError, "internal", "VAPID key unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"public_key": pub})
}

// handlePushSubscribe serves POST /v1/push/subscribe with a PushSubscription
// JSON body. Idempotent per endpoint.
func (a *App) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if a.push.disabled {
		writeError(w, http.StatusNotFound, "not_found", "web push is disabled on this factory")
		return
	}
	var sub webpush.Subscription
	if err := json.NewDecoder(io.LimitReader(r.Body, maxPushBody)).Decode(&sub); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return
	}
	if err := sub.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if !a.pushHostAllowed(sub.Endpoint) {
		writeError(w, http.StatusBadRequest, "bad_request", "endpoint: not a known push service")
		return
	}
	a.push.mu.Lock()
	defer a.push.mu.Unlock()
	subs, err := a.loadSubs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := subs[:0:0]
	for _, s := range subs {
		if s.Endpoint != sub.Endpoint {
			out = append(out, s)
		}
	}
	if len(out) >= maxPushSubs {
		out = out[len(out)-maxPushSubs+1:]
	}
	out = append(out, pushSub{Subscription: sub, Created: a.tasks.Now().UTC().Format(time.RFC3339)})
	if err := a.saveSubs(r.Context(), out); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscribed": true})
}

// handlePushUnsubscribe serves POST /v1/push/unsubscribe {"endpoint": ...}:
// the settings toggle that disables push for one device.
func (a *App) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if a.push.disabled {
		writeError(w, http.StatusNotFound, "not_found", "web push is disabled on this factory")
		return
	}
	var in struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxPushBody)).Decode(&in); err != nil || in.Endpoint == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "endpoint: required")
		return
	}
	a.push.mu.Lock()
	defer a.push.mu.Unlock()
	if err := a.dropSubLocked(r.Context(), in.Endpoint); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscribed": false})
}

func (a *App) dropSubLocked(ctx context.Context, endpoint string) error {
	subs, err := a.loadSubs(ctx)
	if err != nil {
		return err
	}
	out := subs[:0:0]
	for _, s := range subs {
		if s.Endpoint != endpoint {
			out = append(out, s)
		}
	}
	if len(out) == len(subs) {
		return nil
	}
	return a.saveSubs(ctx, out)
}

// pushMessage is the notification payload the service worker renders.
type pushMessage struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Tag   string `json:"tag"`
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// pushForEnvelope maps an operator-inbox envelope to a notification; ok is
// false for envelopes that do not notify the operator.
func pushForEnvelope(inboxID string, env wire.MessageEnvelope) (pushMessage, bool) {
	if inboxID != auth.PlannerMainInbox {
		return pushMessage{}, false
	}
	var p struct {
		Text         string `json:"text"`
		Summary      string `json:"summary"`
		JobID        string `json:"job_id"`
		State        string `json:"state"`
		LastError    string `json:"last_error"`
		Reason       string `json:"reason"`
		SelfInit     bool   `json:"self_initiated"`
		PendingMin   int    `json:"pending_minutes"`
		TaskID       string `json:"task_id"`
		MergeRequest string `json:"merge_request_id"`
	}
	_ = json.Unmarshal(env.Payload, &p)
	task := env.ThreadID
	if task == "" {
		task = p.TaskID
	}
	url := "/#/inbox"
	if task != "" {
		url = "/#/task/" + task
	}
	switch env.Type {
	case "brain.reply":
		if p.Text == "" {
			return pushMessage{}, false
		}
		return pushMessage{Title: "Planner", Body: clip(p.Text, pushBodyChar), URL: url, Tag: "reply-" + task}, true
	case "notify.job":
		if p.SelfInit {
			return pushMessage{}, false
		}
		body := p.Summary
		if p.LastError != "" {
			body = p.LastError
		}
		return pushMessage{Title: "Job " + p.JobID + " " + p.State, Body: clip(body, pushBodyChar), URL: url, Tag: "job-" + p.JobID}, true
	case "notify.pending":
		return pushMessage{Title: "Job " + p.JobID + " stuck pending", Body: clip(p.Reason, pushBodyChar), URL: url, Tag: "pending-" + p.JobID}, true
	case "notify.merge", "notify.conflict":
		return pushMessage{Title: strings.Replace(env.Type, "notify.", "Merge ", 1), Body: clip(p.Summary+" "+p.Reason, pushBodyChar), URL: url, Tag: env.Type + "-" + task}, true
	}
	return pushMessage{}, false
}

// pushEnvelope is the inbox publish tap: non-blocking.
func (a *App) pushEnvelope(inboxID string, env wire.MessageEnvelope) {
	if m, ok := pushForEnvelope(inboxID, env); ok {
		go a.sendPush(m)
	}
}

// pushAlert pushes a newly raised alert.
func (a *App) pushAlert(al tasks.Alert) {
	url := "/#/inbox"
	if al.TaskID != "" {
		url = "/#/task/" + al.TaskID
	}
	go a.sendPush(pushMessage{Title: "P" + strconv.Itoa(al.Priority) + " alert: " + clip(al.Title, 80), Body: clip(al.Body, pushBodyChar), URL: url, Tag: "alert-" + al.ID})
}

// sendPush delivers m to every subscription, pruning dead ones.
func (a *App) sendPush(m pushMessage) {
	if a.push == nil || a.push.disabled {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a.push.mu.Lock()
	if err := a.ensureVAPID(ctx); err != nil {
		a.push.mu.Unlock()
		log.Printf("factoryapp: push: vapid: %v", err)
		return
	}
	subs, err := a.loadSubs(ctx)
	sender := a.push.sender
	a.push.mu.Unlock()
	if err != nil || len(subs) == 0 {
		if err != nil {
			log.Printf("factoryapp: push: load subscriptions: %v", err)
		}
		return
	}
	payload, _ := json.Marshal(m)
	var dead []string
	var wg sync.WaitGroup
	var dmu sync.Mutex
	for _, s := range subs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sender.Send(ctx, s.Subscription, payload); err != nil {
				if errors.Is(err, webpush.ErrGone) {
					dmu.Lock()
					dead = append(dead, s.Endpoint)
					dmu.Unlock()
					return
				}
				log.Printf("factoryapp: push: %v", err)
			}
		}()
	}
	wg.Wait()
	if len(dead) == 0 {
		return
	}
	a.push.mu.Lock()
	defer a.push.mu.Unlock()
	for _, e := range dead {
		if err := a.dropSubLocked(ctx, e); err != nil {
			log.Printf("factoryapp: push: prune: %v", err)
		}
	}
}
