package toft

// The singletons app code reaches for. All of them are cheap to name:
// constructing one touches nothing, so importing this package off-platform never
// fails and never makes a network call. Only calling something that needs the
// platform does.
var (
	// App is this app's view of itself: id, slug, mode, and what an
	// administrator attached.
	App = &AppClient{}

	// Notify sends Slack posts and email as this app.
	Notify = NotifyClient{}

	// Jobs is the app starting background work as itself. For work in a
	// person's name, use [ViewerFor] and its Jobs.
	Jobs = &JobsClient{}

	// Data is the app's one JSON document.
	Data = DataClient{}
)

// Version is this SDK's own release, which moves with the platform's.
const Version = "0.3.0"

// ContractVersion is the callback contract this SDK speaks to /api/v1/runner/*.
// The platform refuses a mismatch rather than tolerating it, so a partial upgrade
// fails loudly instead of behaving strangely. The app contract (/api/v1/app/*) is
// versioned separately — see [AppContractVersion].
const ContractVersion = runnerContractVersion
