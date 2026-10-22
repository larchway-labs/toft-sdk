package toft

import (
	"fmt"
	"os"
	"strings"
)

// What Toft injects, and what to do when it has not.
//
// Anything that talks to the platform returns an error off-platform and names
// the variable that is missing. It does not quietly become a no-op, and there
// is deliberately no dry-run mode: an SDK that printed to a console instead of
// paging a channel is a trap that only springs in production, and it springs at
// 2am on the one night somebody needed the page. Develop against a real install.
//
// The single exception is the scratch helpers, which are pure filesystem and
// fall back to the system temp directory — a path is a path.
const (
	EnvAppToken = "TOFT_APP_TOKEN"
	EnvAPIURL   = "TOFT_API_URL"
	EnvAppID    = "TOFT_APP_ID"
	EnvAppSlug  = "TOFT_APP_SLUG"
	EnvStorage  = "TOFT_STORAGE"
	EnvBasePath = "BASE_PATH"

	// Not read for their values here, only to tell one absence from another:
	// inside a job the app token is *meant* to be missing, and the run id is
	// exactly the condition "we are inside a job".
	EnvRunID   = "TOFT_RUN_ID"
	EnvJobName = "TOFT_JOB_NAME"
	// EnvScheduledFor is the instant a run is for; see [Run.ScheduledFor].
	EnvScheduledFor = "TOFT_SCHEDULED_FOR"
)

// AppContractVersion is the app contract this SDK speaks, sent on every
// /api/v1/app/* call. The platform refuses a mismatch rather than tolerating
// it, so a partial upgrade fails loudly instead of half-working in a way nobody
// traces back to a version skew.
const AppContractVersion = "1"

// ContractHeader carries [AppContractVersion].
const ContractHeader = "X-Toft-App-Contract"

// require reads an injected variable, or fails saying what sets it.
//
// The message names the variable rather than describing the situation, because
// the fix is always the same: run this on Toft — except in the one case
// where it is not, which [missing] explains.
func require(name string) (string, error) {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value, nil
	}
	return "", &Error{Body: refusal(missingCode(name)), Message: missing(name)}
}

// missingCode is the code [missing]'s sentence carries: job_send_only — the
// platform's own word for the same rule — when a job asked for the app token,
// off_platform otherwise. Neither is helped by trying again, and without a
// code a statusless error read as retryable, which is a network failure's
// answer.
func missingCode(name string) string {
	if name == EnvAppToken && strings.TrimSpace(os.Getenv(EnvRunID)) != "" {
		return "job_send_only"
	}
	return "off_platform"
}

// missing is why the variable is absent, in the two cases that differ.
//
// **The app token missing inside a job is not a broken deploy.** A job workload
// is given none, deliberately: the app's credential belongs to the process
// serving its requests, and a job serves none. The generic sentence is actively
// misleading there — it tells an author running a deployed app on the platform
// that they are not running on the platform, and sends them looking for a
// missing variable, a broken deploy or a platform bug. The answer is
// architectural, so it has to be in the sentence.
//
// [EnvRunID] is what says we are inside a job; the runner exports it for every
// run and for nothing else. Same branch, same wording, in all four SDKs — an
// author who moves language must not meet a different explanation of one
// platform rule.
func missing(name string) string {
	runID := strings.TrimSpace(os.Getenv(EnvRunID))
	if name == EnvAppToken && runID != "" {
		where := "run " + runID
		if job := strings.TrimSpace(os.Getenv(EnvJobName)); job != "" {
			where = fmt.Sprintf("job %q, run %s", job, runID)
		}
		return fmt.Sprintf(
			"%s is not set because this is a job (%s), and a job is deliberately given "+
				"no app token — this is not a broken deploy. toft.Data, toft.App and toft.Notify "+
				"belong to the process serving this app's requests, and a job serves none. "+
				"Two ways round it: do this from the service process, or carry the result "+
				"out of the run with Run.SetResult or Run.WriteOutputJSON and let an "+
				"endpoint fold it in.", name, where)
	}
	return fmt.Sprintf(
		"%s is not set. This call needs the environment Toft injects into a "+
			"deployed app — it only works in an app running on the platform.", name)
}
