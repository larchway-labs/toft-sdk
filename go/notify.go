package toft

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Sending Slack posts and email *as this app*.
//
// The platform holds the credential. This app never sees the workspace bot token
// or the relay password, never chooses which Slack channel it posts to, and
// never chooses the address its mail comes from. It supplies the message; the
// platform supplies the identity. That is the whole shape of the feature, and it
// is what makes an administrator's "detach Slack from that app" a row they
// delete rather than a token they have to rotate across every app on the install.
//
//	toft.Notify.Slack(ctx, "Ingest finished: 1,204 rows in 42s")
//	toft.Notify.SlackFile(ctx, file, toft.SlackFileOptions{Title: "Weekly reconciliation"})
//	toft.Notify.Email(ctx, toft.EmailMessage{To: []string{"finance@example.com"}, ...})
//
// Three properties to write app code against.
//
// **Nothing here blocks on delivery.** Every send returns as soon as the
// platform has accepted and recorded it, with a [Send] whose ID names the
// record. Delivery happens afterwards. A call returning without an error means
// *accepted*, not *delivered* — and that distinction is deliberate, because a
// report generator that fell over because Slack was having an afternoon would be
// a worse outcome than a report that arrives late.
//
// **The record is what you rely on, not the message.** [NotifyClient.Status]
// reads back one send by its id. Owners see the same thing on the app's
// Integrations tab; administrators see every app's on the install's integration
// log.
//
// **Where it goes is not yours to choose.** There is no channel argument, and
// from / reply-to are not fields. Sending one is a refusal, not a silently
// ignored field — a dropped field is a caller who believes they chose. An owner
// who wants a different Slack channel or a team reply-to address sets it on the
// app, where it is stored, audited, and visible to everyone who can read the app.

const (
	slackPath     = "/api/v1/app/notify/slack"
	slackFilePath = "/api/v1/app/notify/slack/file"
	emailPath     = "/api/v1/app/notify/email"
	sendsPath     = "/api/v1/app/sends"
)

// terminalSendStatuses will not change again.
var terminalSendStatuses = map[string]bool{
	"sent": true, "failed": true, "blocked": true, "abandoned": true,
}

// Send is one accepted send, and the platform's record of it.
type Send struct {
	ID          string
	Status      string
	Integration string
	Action      string
	Err         string

	// Raw is the platform's whole answer, for a field this struct has not
	// learned about yet.
	Raw map[string]any
}

// Done reports whether this send's status will change again.
func (s Send) Done() bool { return terminalSendStatuses[s.Status] }

// Delivered reports whether the platform got it all the way out.
func (s Send) Delivered() bool { return s.Status == "sent" }

// Blocked is true when an administrator turned the capability off after
// accepting.
//
// A distinct outcome from a failure, and worth distinguishing in an app's own
// logs: nothing is wrong with the message, and retrying will not help until
// somebody re-attaches the integration.
func (s Send) Blocked() bool { return s.Status == "blocked" }

func sendFrom(raw []byte) (Send, error) {
	var body map[string]any
	if err := decode(raw, &body); err != nil {
		return Send{}, err
	}
	text := func(keys ...string) string {
		for _, key := range keys {
			if value, ok := body[key].(string); ok && value != "" {
				return value
			}
		}
		return ""
	}
	send := Send{
		ID:          text("send_id", "id"),
		Status:      text("status"),
		Integration: text("integration"),
		Action:      text("action"),
		Err:         text("error"),
		Raw:         body,
	}
	if send.Status == "" {
		send.Status = "queued"
	}
	return send, nil
}

// NotifyClient sends Slack posts and email as this app.
type NotifyClient struct{}

// Slack posts one message to this app's Slack channel.
//
// text is Slack mrkdwn — *bold*, _italic_, `code`, <https://example.com|links>
// and lists. The platform appends a line naming this app and linking to it,
// which app code cannot suppress: an app that could post indistinguishably from
// the platform is an app that can impersonate it.
//
// Which channel is not an argument and never will be. The app's owners choose it
// in the UI from the channels somebody invited the bot into, falling back to the
// install's default until they do.
//
// No idempotency key by default — a duplicate Slack post is cheap, and a key
// would be quietly deduplicating an app that meant to post twice. Use
// [NotifyClient.SlackWithKey] for a message whose duplicate would matter.
func (n NotifyClient) Slack(ctx context.Context, text string) (Send, error) {
	return n.SlackWithKey(ctx, text, "")
}

// SlackWithKey is [NotifyClient.Slack] with an idempotency key of the caller's
// choosing.
func (NotifyClient) SlackWithKey(ctx context.Context, text, idempotencyKey string) (Send, error) {
	if err := requireText(text, "slack"); err != nil {
		return Send{}, err
	}
	t, err := client()
	if err != nil {
		return Send{}, err
	}
	raw, err := t.postJSON(ctx, slackPath, map[string]string{"text": text}, idempotencyKey, nil)
	if err != nil {
		return Send{}, err
	}
	return sendFrom(raw)
}

// SlackFileOptions carries the parts of a file post that are not the file.
type SlackFileOptions struct {
	Title   string
	Comment string
	// Filename overrides the name the recipient sees.
	Filename string
	// IdempotencyKey overrides the one this call generates for itself.
	IdempotencyKey string
}

// SlackFile uploads one file to this app's Slack channel.
//
// This is the "post the report" call:
//
//	file, _ := toft.AttachFile("/tmp/recon.xlsx")
//	toft.Notify.SlackFile(ctx, file, toft.SlackFileOptions{
//		Title:   "Weekly reconciliation",
//		Comment: "1,204 rows, 3 breaks",
//	})
//
// A separate capability from posting text, and off by default even when posting
// is on: a text post leaks a sentence, a file post can move an entire dataset
// into a channel in one call. If it refuses, the message says which switch an
// administrator throws.
//
// Defaults to an idempotency key, unlike [NotifyClient.Slack] — re-uploading a
// 20 MiB report because a response was slow is not cheap, in the channel or on
// the wire.
func (NotifyClient) SlackFile(ctx context.Context, file Attachment, opts SlackFileOptions) (Send, error) {
	if opts.Filename != "" {
		file.Name = opts.Filename
	}
	fields := map[string]string{}
	if opts.Title != "" {
		fields["title"] = opts.Title
	}
	if opts.Comment != "" {
		fields["comment"] = opts.Comment
	}
	t, err := client()
	if err != nil {
		return Send{}, err
	}
	raw, err := t.postMultipart(ctx, slackFilePath, fields, "file",
		[]Attachment{file}, defaultKey(opts.IdempotencyKey))
	if err != nil {
		return Send{}, err
	}
	return sendFrom(raw)
}

// EmailMessage is one email, as this app sends it.
//
// There is no From and no ReplyTo, and there never will be. Mail goes out as
// "<App Name> (via Toft)" from the install's sending address, and replies
// go to the app's owners — or to the address an owner set on the app's
// Integrations tab.
type EmailMessage struct {
	To      []string
	Subject string

	// Text is required even when HTML is given. The plain-text half is where the
	// "an application sent this" line is unmissable, and an HTML-only message has
	// no fallback for a reader whose client will not render it.
	Text string

	// HTML is sanitized by the platform: scripts, styles, frames, event handlers
	// and positioning are removed. Style the content, not the frame.
	HTML string

	CC          []string
	BCC         []string
	Attachments []Attachment

	// IdempotencyKey overrides the one this call generates for itself. A
	// cron-driven sender that wants "at most one report per week however many
	// times I run" passes its own stable key, such as "weekly-report-2026-W34".
	IdempotencyKey string
}

// Email sends one email as this app.
//
// Recipients are unrestricted — any address, whether or not they have a
// Toft account, which is the point: the weekly report goes to the finance
// team, and the finance team does not log in here. Every send is recorded in
// full, recipients included, and that record is the only control an
// administrator has over this. Write app code that would read well in an
// incident review.
//
// **Idempotency.** A key is generated by default, once per call, and reused if
// this SDK retries the request. That is what stops a slow response from turning
// into two copies of the report in somebody's inbox. It does *not* deduplicate
// across processes: see [EmailMessage.IdempotencyKey].
func (NotifyClient) Email(ctx context.Context, message EmailMessage) (Send, error) {
	recipients := addresses(message.To)
	if len(recipients) == 0 {
		return Send{}, &Error{Message: "email needs at least one recipient in To"}
	}
	if err := requireText(message.Subject, "email subject"); err != nil {
		return Send{}, err
	}
	if err := requireText(message.Text, "email text"); err != nil {
		return Send{}, err
	}

	body := map[string]any{"to": recipients, "subject": message.Subject, "text": message.Text}
	if message.HTML != "" {
		body["html"] = message.HTML
	}
	if cc := addresses(message.CC); len(cc) > 0 {
		body["cc"] = cc
	}
	if bcc := addresses(message.BCC); len(bcc) > 0 {
		body["bcc"] = bcc
	}

	t, err := client()
	if err != nil {
		return Send{}, err
	}
	key := defaultKey(message.IdempotencyKey)

	var raw []byte
	if len(message.Attachments) > 0 {
		// Attachments travel as multipart with the message as one JSON field,
		// never base64 inside a JSON body: a 10 MiB report becomes a 13 MiB
		// string that then has to be decoded into a second 10 MiB buffer, and
		// paying that on the platform's heap for the tidiness of a single
		// content type is not a trade worth making.
		encoded, err := json.Marshal(body)
		if err != nil {
			return Send{}, &Error{Message: err.Error(), Wrapped: err}
		}
		raw, err = t.postMultipart(ctx, emailPath, map[string]string{"payload": string(encoded)},
			"attachment", message.Attachments, key)
		if err != nil {
			return Send{}, err
		}
	} else {
		raw, err = t.postJSON(ctx, emailPath, body, key, nil)
		if err != nil {
			return Send{}, err
		}
	}
	return sendFrom(raw)
}

// Status reads back one send this app made.
//
// The only thing an app may read about its own sends, and deliberately so: there
// is no listing. An app browsing the record would be learning which addresses its
// administrator finds interesting, whereas reading back an id it minted, about
// content it wrote, tells it nothing it did not know.
func (NotifyClient) Status(ctx context.Context, sendID string) (Send, error) {
	if sendID == "" {
		return Send{}, &Error{Message: "Status needs the id of a send"}
	}
	t, err := client()
	if err != nil {
		return Send{}, err
	}
	raw, err := t.get(ctx, sendsPath+"/"+sendID, nil)
	if err != nil {
		return Send{}, err
	}
	return sendFrom(raw)
}

func requireText(value, what string) error {
	if strings.TrimSpace(value) == "" {
		return &Error{Message: fmt.Sprintf("%s needs some text", what)}
	}
	return nil
}

// addresses drops blanks, because one empty string in a slice is a caller's
// missing configuration rather than a recipient.
func addresses(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func defaultKey(key string) string {
	if key != "" {
		return key
	}
	return freshKey()
}
