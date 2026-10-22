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
//	toft.Jobs.Start(ctx, "reconcile", opts)  // as the app
//	me, err := toft.ViewerFor(r.Header)      // as a person
//	me.Jobs.Start(ctx, "reconcile", opts)
//	me.Jobs.Mine(ctx, "reconcile", 20)
//
//	who, err := toft.Who(ctx, r.Header)      // who is looking: *Identity or nil
//	if who != nil && (who.Role == "owner" || who.Role == "editor") {
//		renderAdminTab()
//	}
//
//	toft.CurrentRun()                        // inside a job: nil everywhere else
//	toft.TaskFor(r.Header)                   // a scheduled firing, else nil
//	task.Log(ctx, "read 1,204 rows")       // a log the platform cannot capture
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
// release, as an isolated workload with a memory limit, a deadline and a durable
// record — in *both* execution modes, because moving twelve minutes and six
// gigabytes out of the app's own process is the point rather than a side effect.
// It never blocks on the work.
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
// **A scheduled task's log is the one thing the platform cannot capture for you.**
// Toft triggers a task by making an HTTP request to a path you published; it
// owns no process, so your writes land in the app's own stream interleaved with
// every other request. [TaskFor] is that firing — nil on an ordinary visit, so one
// endpoint can serve both a schedule and a person — and [Task.Log] is what makes a
// run's log exist.
//
// **A failed run may be retried, and you configure that in Toft, not here.**
// What the SDK does is keep the chain from being your problem: a run the platform
// has queued another attempt at is not [JobRun.Done], and [JobRun.Refresh] follows
// it, so [JobRun.Wait] gives you the outcome of the work rather than of the first
// try at it.
//
// The library is deliberately thin. It owns no delivery semantics — no client-side
// scheduling, no size limits duplicated from the platform, and **no retry policy
// of its own**: the one repeat it makes is of a request that never reached a
// handler, which is a transport detail and not the platform's retry of a *run*.
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
// code. [QueueFullError] and [VersionConflictError] are the two worth naming;
// errors.As reaches all three.
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
