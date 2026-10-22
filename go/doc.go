// Package toft is the Toft app SDK for Go.
//
// One import, one entry point:
//
//	import "toft"
//
//	toft.App.Self(ctx)                       // who am I: id, slug, mode
//	toft.App.Can(ctx, "email")               // what did an administrator attach
//
//	toft.ScratchDir("uploads")               // ephemeral, per app, no network
//	toft.SessionScratchDir(sessionID)        // one browser login's corner of it
//	toft.NewSessionID()                      // …for a viewer who signed in nowhere
//	toft.Storage("reports")                  // durable, resolved by name
//	toft.Store("reports").Put(path, name)    // a file into a store
//
//	toft.Data.Get(ctx)                       // this app's one JSON document
//	toft.Data.Set(ctx, settings)             // survives restart, redeploy, rollback
//	toft.Data.Update(ctx, bump)              // …and a second writer
//
//	toft.Notify.Slack(ctx, "Ingest finished: 1,204 rows")
//	toft.Notify.SlackFile(ctx, file, toft.SlackFileOptions{Title: "Weekly reconciliation"})
//	toft.Notify.Email(ctx, toft.EmailMessage{To: to, Subject: subject, Text: body})
//
//	run, _ := toft.Jobs.Start(ctx, "reconcile", opts) // as the app…
//	toft.Jobs.Respond(w, r, run)             // …and answer at once: never wait
//	me, err := toft.ViewerFor(r.Header)      // as a person
//	me.Jobs.Start(ctx, "reconcile", opts)
//	me.Jobs.Mine(ctx, toft.ListOptions{Job: "reconcile"})
//
//	who, err := toft.Who(ctx, r.Header)      // who is looking: *Identity or nil
//	if who != nil && (who.Role == "owner" || who.Role == "editor") {
//		renderAdminTab()
//	}
//
//	toft.CurrentRun()                        // inside a job: nil everywhere else
//
// # What is the same in every language
//
// **The credential stays with the platform.** An app never holds the Slack bot
// token or the relay password, never names the channel it posts to, and never
// chooses the address its mail comes from. It supplies the message; Toft
// supplies the identity. An administrator attaches an integration to an app, and
// detaching it is a row they delete rather than a token they rotate everywhere.
//
// **Ask before you send.** [AppClient.Can] is cheap and cached. An app that checks
// at startup can tell someone "email is not enabled for this app" on a screen,
// instead of leaving it to be discovered at 2am in a log.
//
// **Nothing here blocks on delivery.** A send returns once the platform has
// accepted and recorded it; delivery happens after. The returned [Send] names that
// record, and [NotifyClient.Status] reads it back. The record — not the message —
// is what a workflow may rely on.
//
// **Off-platform it fails rather than pretending.** Every call that needs the
// platform names the environment variable that is missing. There is deliberately
// no dry-run mode: an SDK that silently printed to a console instead of paging a
// channel would be a trap that only springs in production. Develop against a real
// install.
//
// **Background work is a different entrypoint of this same app.**
// [JobsClient.Start] runs a command toft.toml declares, from the same
// release, as its own process with a memory limit, a deadline and a durable
// record — in *both* execution modes, because moving twelve minutes and six
// gigabytes out of the app's own process is the point rather than a side effect.
// It never blocks on the work.
//
// **A handler responds; scripts and jobs wait.** The one call after Start in a
// handler is [JobsClient.Respond]: 202 with Location for a script, 303 to the
// run's live status page for a browser. The gateway cuts every request at a
// deadline it puts on the request, so [JobRun.Wait] inside one — with no
// timeout, or one past that deadline — is refused at once with
// would_outlive_request, whose hint is the Respond line to paste. Waiting
// belongs to a script, a CLI, or a job.
//
// **Params are the only input.** A job's input is the params toft.toml
// declares, validated and typed at start; anything larger goes into the app's
// storage first, and its name goes in a param. Its result comes back through
// its output ([Run.WriteOutputJSON]), the app's storage, or the app's own
// database.
//
// **Two objects, and which one you call is the decision.** [Jobs] is the app
// acting as itself; [ViewerFor] gives you the app acting for the person looking at
// the page. Not a flag, because a flag would make the anonymous case the default
// spelling of the attributed one, and attribution would then be lost by omission.
//
// **Two entrypoints for a viewer, and which one fails is the difference.**
// [ViewerFor] is *act as this person* and fails when there is nobody to act for —
// a silent downgrade would move a run out of somebody's name and into nobody's.
// [Who] is *who is this*, and answers nil for an anonymous visitor because that is
// an answer rather than a failure. It fails for one thing only: a request
// carrying a signed-in viewer to an app an administrator has not enabled viewer
// identity on, which is a configuration gap to close and not a runtime state to
// branch on. A request with nobody on it (anonymous, or an API key) is nil
// whatever the setting; App.Can("viewer.identity") reads the setting itself.
//
// **A run ends exactly once, and nothing re-runs by itself.** A failure is one
// run with its reason in [JobRun.FailureReason]; running it again is a
// deliberate act — the app calling Start, or a person pressing Run again in
// Toft — never a setting.
//
// The library is deliberately thin. It owns no delivery semantics — no client-side
// scheduling, no size limits duplicated from the platform, and **no retry policy
// of its own**: the one repeat it makes is of a request that never reached a
// handler, which is a transport detail and never a second run.
// The stable surface is the HTTP API; anything clever added here becomes a second
// source of truth about how a send behaves.
//
// # What is different because this is Go
//
// **A context on everything that touches the network.** Python and Node take
// none; a Go app already has one on every request, and an SDK that ignored it
// would be the one call in a handler that cannot be cancelled.
//
// **Errors, not exceptions.** Every platform refusal comes back as an [Error]
// carrying the status, the platform's own words, and the decoded body — so a
// caller renders "3 runs already queued, and 3 is the limit" rather than a status
// code. Every refusal — the platform's and the SDK's own, like no_viewer and
// would_outlive_request — has [Error.Code], [Error.Retryable] and [Error.Hint],
// and the hint is the next step written to be pasted. [QueueFullError] and
// [VersionConflictError] are the two worth naming; errors.As reaches all three.
//
// **Standard library only, and the same promise as the other two.** net/http and
// encoding/json are enough, so this adds nothing to an app's go.sum and cannot
// collide with a version it already pins.
//
// **The build installs it; nothing is fetched.** Toft writes this module
// beside your release and points a go.work at it, so `import "toft"`
// resolves without a require line, a proxy, or a checksum database — and the SDK
// an app compiles against is always the one that shipped with the platform it is
// talking to. Nothing of it survives the build: Go links statically, so the
// workspace file and the module are removed once the binary exists.
package toft
