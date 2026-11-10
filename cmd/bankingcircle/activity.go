package main

import (
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/webduvet/fintechlab-simple/internal/activity"
	"github.com/webduvet/fintechlab-simple/internal/bankingcircle"
)

// What the console's two Banking Circle panels say.
//
// Both summarisers read the caller's own request rather than this
// service's interpretation of it, so a payment refused before it reached
// the ledger still appears with the amount that was attempted. A panel
// that only showed accepted payments would be a panel that goes quiet
// exactly when something is wrong.

func summarizeBridgePayment(c *activity.Call) (string, map[string]string) {
	amount := strings.TrimSpace(c.JSONField("amount") + " " + c.JSONField("currency"))
	holder := c.JSONField("holder")
	summary := fmt.Sprintf("payout %s from %s", amount, c.JSONField("accountId"))
	if holder != "" {
		summary += " to " + holder
	}
	return summary, nonEmpty(map[string]string{
		"payment_id":   c.JSONField("paymentId"),
		"external_ref": c.JSONField("externalRef"),
		"account":      c.JSONField("accountId"),
		"beneficiary":  c.JSONField("iban"),
		"holder":       holder,
		"amount":       amount,
	})
}

func summarizeIncoming(c *activity.Call) (string, map[string]string) {
	amount := strings.TrimSpace(c.JSONField("amount") + " " + c.JSONField("currency"))
	return fmt.Sprintf("funds in: %s to the safeguarding account", amount),
		nonEmpty(map[string]string{
			"amount":    amount,
			"reference": c.JSONField("reference"),
		})
}

func nonEmpty(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// recordNotification logs one delivery attempt of an encrypted batch.
//
// The batch is the unit, not the event: the vendor's own contract is that
// notifications are delivered in batches of up to five, and an operator
// asking "did the webhook fire" is asking about the POST. The event types
// inside it are what makes the line worth reading, so they are named.
func (a *app) recordNotification(sub *bankingcircle.Subscription, env bankingcircle.Envelope, status int, err error) {
	kinds := notificationKinds(env)

	detail := nonEmpty(map[string]string{
		"subscription": sub.ID,
		"endpoint":     sub.Endpoint,
		"events":       strconv.Itoa(len(env.Notifications)),
		"http":         statusText(status),
		// The traps whose steps made any of these, when a trap did.
		"trap": trapIDs(env),
	})
	summary := fmt.Sprintf("batch of %d to %s — %s", len(env.Notifications), sub.Endpoint, strings.Join(kinds, ", "))
	ev := activity.Event{Env: sub.Env, Op: "notification", Peer: sub.Endpoint, Summary: summary, Detail: detail, Status: activity.StatusOK}
	if err != nil {
		// The client's own refusal, verbatim: this is the line that says
		// whether the platform is listening, which is the whole question.
		ev.Status = activity.StatusBad
		ev.Summary = summary + " — " + err.Error()
		ev.Detail["error"] = err.Error()
	}
	a.notifLog.Record(ev)
}

// recordQueued logs notifications parked behind a paused subscription.
//
// It is in the same log as the deliveries, on purpose. "Nothing has gone
// out" and "nothing has gone out because you paused it" are the same
// silence from every other angle, and the one place an operator looks to
// tell them apart is this list. The line is amber rather than red: queued
// is a state somebody chose, not a failure, and it resolves the moment
// they resume.
func (a *app) recordQueued(sub *bankingcircle.Subscription, env bankingcircle.Envelope) {
	kinds := notificationKinds(env)
	a.notifLog.Record(activity.Event{
		Env:  sub.Env,
		Op:   "notification.queued",
		Peer: sub.Endpoint,
		Summary: fmt.Sprintf("queued: %d notification(s) for %s — %s — delivery is paused",
			len(env.Notifications), sub.Endpoint, strings.Join(kinds, ", ")),
		Status: activity.StatusWarn,
		Detail: nonEmpty(map[string]string{
			"subscription": sub.ID,
			"endpoint":     sub.Endpoint,
			"events":       strconv.Itoa(len(env.Notifications)),
			"waiting":      strconv.Itoa(a.dispatch.Queued(sub.ID)),
			"trap":         trapIDs(env),
		}),
	})
}

// recordPause logs the switch itself, so the log says who stopped the pipe
// and when -- without it, a queued line has no beginning and a batch that
// arrives late has no explanation.
func (a *app) recordPause(sub *bankingcircle.Subscription, paused bool, n int) {
	ev := activity.Event{
		Env:    sub.Env,
		Op:     "notification.pause",
		Peer:   sub.Endpoint,
		Status: activity.StatusWarn,
		Summary: fmt.Sprintf("delivery to %s paused — notifications will queue here until it is resumed",
			sub.Endpoint),
		Detail: nonEmpty(map[string]string{
			"subscription": sub.ID,
			"endpoint":     sub.Endpoint,
			"waiting":      strconv.Itoa(n),
		}),
	}
	if !paused {
		ev.Status = activity.StatusOK
		ev.Summary = fmt.Sprintf("delivery to %s resumed — %d notification(s) released", sub.Endpoint, n)
		ev.Detail["released"] = strconv.Itoa(n)
	}
	a.notifLog.Record(ev)
}

// notificationKinds names the event types in a batch, collapsing repeats
// into "Type ×3". The count alone is not worth reading; the types are.
func notificationKinds(env bankingcircle.Envelope) []string {
	types := map[string]int{}
	order := []string{}
	for _, n := range env.Notifications {
		if _, seen := types[n.NotificationType]; !seen {
			order = append(order, n.NotificationType)
		}
		types[n.NotificationType]++
	}
	kinds := make([]string, 0, len(order))
	for _, k := range order {
		if types[k] > 1 {
			kinds = append(kinds, fmt.Sprintf("%s ×%d", k, types[k]))
			continue
		}
		kinds = append(kinds, k)
	}
	return kinds
}

func statusText(status int) string {
	if status == 0 {
		return ""
	}
	return strconv.Itoa(status)
}

// recordRead logs one reconciliation read — a report or a payment's status
// — with who asked, what they asked for and what went back, in the
// caller's environment. The Reconciliation reads panel and the diagram's
// reconciliation arrow are both read from this log.
func (a *app) recordRead(r *http.Request, op, summary string, detail map[string]string, status string) {
	a.reportLog.Record(activity.Event{Env: scopeOf(r), Op: op, Peer: remoteHost(r), Summary: summary, Detail: nonEmpty(detail), Status: status})
}

// problemNames is the parameters a refused report named, sorted.
func problemNames(problems map[string][]string) string {
	names := make([]string, 0, len(problems))
	for name := range problems {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// dateRange is "d" for a single day and "from..to" otherwise.
func dateRange(from, to string) string {
	if from == to {
		return from
	}
	return from + ".." + to
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
