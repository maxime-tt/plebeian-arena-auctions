package main

// The create-content rules a kind-1440 create must satisfy.  They are CODE, not
// committed state: applyCreate enforces them on every create, root or
// supersession, and no genesis term (Params field) can loosen or tighten them.
//
// Two consumers must never disagree about them:
//
//   - applyCreate, which refuses a create that violates one, and
//   - the kind-1448 params event, which publishes them (BuildParamsEvent), so a
//     relay-only client can pre-validate a create against the same numbers the
//     chain enforces instead of round-tripping a doomed event and reading the
//     refusal afterwards.
//
// Both sides are therefore written against these constants; a limit that lives
// as a literal in one place and a number on the wire in another is a bug the
// moment either is edited.
const (
	// MaxTitleChars is the title ceiling.  A title also must be non-blank
	// after trimming; emptiness is not a length rule and has no constant.
	MaxTitleChars = 120
	// MaxDescriptionChars is the description ceiling (bytes).
	MaxDescriptionChars = 2000
	// MaxImageChars is the image ceiling (bytes).  ImageRule additionally
	// bounds what may appear in it.
	MaxImageChars = 500
	// MaxCategoryChars is the category ceiling (bytes).
	MaxCategoryChars = 40
	// MaxNameChars is the seller-display-name ceiling (bytes).  It is the
	// human name on the lot, not the Nostr identity.
	MaxNameChars = 40

	// MinStartPrice is the floor on a lot's start price, in sat.
	MinStartPrice int64 = 1
	// MaxStartPrice is the ceiling on a lot's start price, in sat: a price
	// above it is not a lot anyone can bid on, only a way to strand one.
	MaxStartPrice int64 = 10_000_000
	// MinIncrement is the floor on the minimum bid increment, in sat.
	MinIncrement int64 = 1
	// MaxIncrement is the ceiling on the minimum bid increment, in sat.
	MaxIncrement int64 = 1_000_000
)

// ImageRule names, on the wire, the exact image rule safeImage enforces.  It is
// a rule NAME rather than a pattern: safeImage allows the empty string, an
// https URL (`https://…`), or a same-site path (`./…` or `/…` with a single
// leading slash, so `//host/x` is a protocol-relative URL and is refused), and
// then only bytes from a fixed URI-safe set — [A-Za-z0-9] and
// `-._~:/?#[]@!$&()*+,;=%`.  Anything else (a space, `<`, `"`, a bare `http://`
// URL) is refused, so the value can never break out of an HTML attribute
// (internal audit A5).  A client that wants to pre-validate must implement that
// rule; the name tells it which one, it does not parametrise it.
const ImageRule = "same-site-or-https"
