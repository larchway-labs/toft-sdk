# Toft app SDKs

The libraries a Toft app calls back with — who is looking, which role they
hold, the app's own data, storage, jobs, tasks and notifications — in the four
languages Toft serves. This is version **0.9.6**, exactly as that release of
Toft ships them.

**You usually do not need this repository.** Toft installs the SDK into your
app when it builds it, from the copy inside the platform itself, so an app and
the install it runs on always agree. Run `toft sdk vendor` in your app to put
that same copy in your tree for local development and your editor.

This repository is for reading the source, and for vendoring a specific
version by hand.

| Language | Directory | Import |
|---|---|---|
| Python | `python/` | `import toft` |
| Node | `node/` | `import toft from "@larchway/toft"` |
| Go | `go/` | `import "toft"` |
| R | `r/` | `library(toft)` |

Every tag is a Toft release: `v0.9.6` is the SDK that Toft 0.9.6
embeds. A tag is never moved.

The documentation for each call is in the CLI & SDK guide:
https://larchway-labs.github.io/toft-docs/latest/cli/

## License

Apache License 2.0 — see [LICENSE](LICENSE). Vendor it into your app, closed
source included.
